package e2eharness

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/azerothcore/AzerothGhost/client"
)

// ArenaBattlemasterEntry is the "Arena Battlemaster" creature template.
const ArenaBattlemasterEntry uint32 = 26007

// ArenaTournamentEvent gates every Arena Battlemaster spawn and is off by default, so
// the NPC has to be brought into the world before anyone can queue at one.
const ArenaTournamentEvent = 31

// DefaultBattlefieldTimeout is used by battlefield waiters when timeout <= 0. The rated
// queue is swept every 5s, hardcoded as BattlegroundMgr::Update's
// m_NextPeriodicQueueUpdateTime rather than configurable, so this is several sweeps.
const DefaultBattlefieldTimeout = 20 * time.Second

// WaitBattlefieldStatus waits until the bot has received an SMSG_BATTLEFIELD_STATUS
// carrying the given status (client.BattlegroundStatusWaitQueue / WaitJoin / InProgress)
// and returns it.
//
// It scans every status received since login or the last DrainBattlefieldStatuses, so
// a status that arrived before the call still matches. A queue is a sequence (WAIT_QUEUE,
// then WAIT_JOIN on the next matchmaker sweep, which can be immediate), and waiting for
// each in turn must not lose the second one. Drain before re-queueing.
func (b *ScenarioBot) WaitBattlefieldStatus(t *testing.T, status uint32, timeout time.Duration) client.BattlefieldStatus {
	t.Helper()
	st, ok := b.TryWaitBattlefieldStatus(status, timeout)
	if !ok {
		HarnessFailf(t, "%s: no SMSG_BATTLEFIELD_STATUS %s within %s (%s)",
			b.Name, client.BattlegroundStatusName(status), timeout, b.battlefieldDiagnostics())
	}
	return st
}

// TryWaitBattlefieldStatus is WaitBattlefieldStatus without the failure, for callers
// that treat a missing status as an outcome rather than an error.
func (b *ScenarioBot) TryWaitBattlefieldStatus(status uint32, timeout time.Duration) (client.BattlefieldStatus, bool) {
	if timeout <= 0 {
		timeout = DefaultBattlefieldTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		for _, st := range b.World.BattlefieldStatuses() {
			if st.Status == status {
				return st, true
			}
		}
		if time.Now().After(deadline) {
			return client.BattlefieldStatus{}, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// DrainBattlefieldStatuses forgets the statuses received so far, so the next
// WaitBattlefieldStatus only matches packets from the next queue.
func (b *ScenarioBot) DrainBattlefieldStatuses() {
	b.World.DrainBattlefieldStatuses()
}

// battlefieldDiagnostics summarises what the server did say, for failure messages.
func (b *ScenarioBot) battlefieldDiagnostics() string {
	var parts []string
	if last, ok := b.World.LastBattlefieldStatus(); ok {
		parts = append(parts, "last status "+client.BattlegroundStatusName(last.Status))
	} else {
		parts = append(parts, "no status received")
	}
	if r, ok := b.World.LastBattlegroundJoinResult(); ok && r.Refused() {
		parts = append(parts, "join refused: "+client.BattlegroundJoinResultName(r.Result))
	}
	if teamType := b.World.LastArenaError(); teamType != 0 {
		parts = append(parts, fmt.Sprintf("not in a %dv%d arena team", teamType, teamType))
	}
	return strings.Join(parts, ", ")
}

// JoinRatedArena sends CMSG_BATTLEMASTER_JOIN_ARENA as a party leader.
//
// The battlemaster must be in the leader's own map, so place the whole party at one
// first (TeleportToArenaBattlemaster). A rated join also needs the party to be exactly
// the arena team's size and the season to be enabled. A refusal is not an error here:
// it shows up as no WAIT_QUEUE, and WaitBattlefieldStatus names the reason.
func (b *ScenarioBot) JoinRatedArena(t *testing.T, battlemasterGUID uint64, arenaSlot client.ArenaSlot) {
	t.Helper()
	if err := b.World.JoinArenaQueue(battlemasterGUID, arenaSlot, true, true); err != nil {
		HarnessFailf(t, "%s: CMSG_BATTLEMASTER_JOIN_ARENA: %v", b.Name, err)
	}
}

// LeaveBattlefieldQueue answers the bot's most recent queue status with "leave queue",
// and reports whether there was one to answer.
//
// A group is only erased from its bracket once its last member is gone, so drain every
// member of a queued team. Bot logout and disbanding the arena team (CreateArenaTeam's
// cleanup) both get there on their own, but only once the server has processed them,
// which can be after the next test has started queueing. Best effort: a bot that never
// queued is a no-op.
func (b *ScenarioBot) LeaveBattlefieldQueue(t *testing.T) bool {
	t.Helper()
	statuses := b.World.BattlefieldStatuses()
	for i := len(statuses) - 1; i >= 0; i-- {
		if !statuses[i].HasBattleground {
			continue
		}
		if err := b.World.LeaveBattlefieldQueue(statuses[i]); err != nil {
			t.Logf("%s: leave battlefield queue: %v", b.Name, err)
			return false
		}
		return true
	}
	return false
}

// EnableArenaSeason turns the arena season on and spawns the Arena Battlemasters, which
// are gated behind a game event that is off by default. Rated joins are refused outright
// without both.
//
// Cleanup puts the season back into the state it was found in and stops the event again.
// Stopping is the right restore because the event is GM-only on stock data: its row has
// start_time == end_time, and CheckOneGameEvent wants now < End, so it never turns itself
// on. Both are realm-global, so two tests that call this must not overlap.
func EnableArenaSeason(t *testing.T, bot *ScenarioBot) {
	t.Helper()

	var previousState int
	if err := bot.CharDB.QueryRow("SELECT season_state FROM active_arena_season").Scan(&previousState); err != nil {
		HarnessFailf(t, "read active_arena_season: %v", err)
	}

	bot.GM(t, ".arena season set state 1")
	bot.GM(t, fmt.Sprintf(".event start %d", ArenaTournamentEvent))
	t.Cleanup(func() {
		bot.World.SendGMCommand(fmt.Sprintf(".event stop %d", ArenaTournamentEvent))
		if previousState != 1 {
			bot.World.SendGMCommand(fmt.Sprintf(".arena season set state %d", previousState))
		}
	})
	bot.FlushWorld(t)
}

// CreateArenaTeam creates an arena team captained by leader and brings member into it,
// returning the team id. The team is disbanded on cleanup, which also takes its members
// out of any arena queue they are still in.
//
// It builds a two player team, so the only teamType a rated queue will accept from it is
// client.ArenaTeam2v2: a rated join needs the party to be exactly the team's size, and a
// 3v3 or 5v5 team created here can never reach that. member must be at MaxPlayerLevel or
// the invite comes back as ERR_ARENA_TEAM_TARGET_TOO_LOW_S and the team stays at one.
//
// The name is a charter name, so it is capped at MAX_CHARTER_NAME and must be unique on
// the realm (UniqueArenaTeamName).
func CreateArenaTeam(t *testing.T, leader, member *ScenarioBot, name string, teamType client.ArenaTeamType) uint32 {
	t.Helper()

	leader.GM(t, fmt.Sprintf(`.arena create %s "%s" %d`, leader.Name, name, teamType))
	leader.FlushWorld(t)

	teamID := ArenaTeamIDByName(t, leader.CharDB, name)
	t.Cleanup(func() {
		leader.World.SendGMCommand(fmt.Sprintf(".arena disband %d", teamID))
	})

	if err := leader.World.InviteToArenaTeam(teamID, member.Name); err != nil {
		HarnessFailf(t, "CMSG_ARENA_TEAM_INVITE %s -> %s: %v", leader.Name, member.Name, err)
	}
	// Flush the inviter, not the invitee: FlushWorld only proves the commands queued on
	// the session it is called on have been applied. An accept that arrives before the
	// invite finds no pending invite and is dropped, and nothing here resends it.
	leader.FlushWorld(t)
	if err := member.World.AcceptArenaTeamInvite(); err != nil {
		HarnessFailf(t, "CMSG_ARENA_TEAM_ACCEPT %s: %v", member.Name, err)
	}
	member.FlushWorld(t)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ArenaTeamMemberCount(t, leader.CharDB, teamID) >= 2 {
			t.Logf("arena team %q id=%d captain=%s member=%s", name, teamID, leader.Name, member.Name)
			return teamID
		}
		time.Sleep(100 * time.Millisecond)
	}
	Preconditionf(t, "arena team %q (id=%d) never reached 2 members", name, teamID)
	return teamID
}

// ArenaTeamIDByName polls for the team row. ArenaTeam::Create persists through an async
// CharacterDatabase.Execute, so the row lands some time after the command is answered.
func ArenaTeamIDByName(t *testing.T, charDB *sql.DB, name string) uint32 {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var id uint32
		err := charDB.QueryRow("SELECT arenaTeamId FROM arena_team WHERE name = ?", name).Scan(&id)
		if err == nil {
			return id
		}
		if err != sql.ErrNoRows {
			HarnessFailf(t, "query arena team %q: %v", name, err)
		}
		if time.Now().After(deadline) {
			Preconditionf(t, "arena team %q never appeared after `.arena create`", name)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ArenaTeamMemberCount counts persisted members of a team.
func ArenaTeamMemberCount(t *testing.T, charDB *sql.DB, teamID uint32) int {
	t.Helper()
	var n int
	if err := charDB.QueryRow(
		"SELECT COUNT(*) FROM arena_team_member WHERE arenaTeamId = ?", teamID).Scan(&n); err != nil {
		HarnessFailf(t, "count arena team members (team %d): %v", teamID, err)
	}
	return n
}

// ArenaTeamRating reads a team's persisted rating. The row is written with the rating
// at creation, so once ArenaTeamIDByName has found it there is nothing to wait for.
//
// The rating comes from the ArenaTeam constructor, not from AddMember, and which config
// it reads depends on the active season: Arena.LegacyArenaStartRating (1500) while
// active_arena_season.season_id is below 6, Arena.ArenaStartRating (0) from season 6 on.
// So zero is a valid answer on a modern season and the wrong expectation on an early one.
//
// Matchmaker rating is not readable here: AddMember seeds it from
// Arena.ArenaStartMatchmakerRating, but it only reaches the DB through
// ArenaTeam::SaveToDB after a match.
func ArenaTeamRating(t *testing.T, charDB *sql.DB, teamID uint32) uint32 {
	t.Helper()
	var rating uint32
	if err := charDB.QueryRow(
		"SELECT rating FROM arena_team WHERE arenaTeamId = ?", teamID).Scan(&rating); err != nil {
		HarnessFailf(t, "read arena team %d rating: %v", teamID, err)
	}
	return rating
}

// TeleportToArenaBattlemaster places every bot at one Arena Battlemaster spawn and
// returns that unit's live ObjectGuid, which is the same for every bot.
//
// The spawn is looked up in the world DB rather than hardcoded, and one specific spawn
// is targeted rather than `.go creature <entry>`, which picks an arbitrary one that may
// not be in a loaded grid. Requires EnableArenaSeason first: the spawns are gated behind
// a game event.
func TeleportToArenaBattlemaster(t *testing.T, bots ...*ScenarioBot) uint64 {
	t.Helper()
	if len(bots) == 0 {
		HarnessFailf(t, "TeleportToArenaBattlemaster: no bots")
	}

	spawnGUID := ArenaBattlemasterSpawn(t, bots[0])
	t.Logf("using Arena Battlemaster spawn guid=%d", spawnGUID)

	for _, b := range bots {
		GoCreatureGUID(t, b.World, spawnGUID)
		b.FlushWorld(t)
	}
	return WaitNearbyUnitByEntry(t, bots[0].World, ArenaBattlemasterEntry, 20*time.Second)
}

// ArenaBattlemasterSpawn returns a continent Arena Battlemaster spawn id from the world DB.
func ArenaBattlemasterSpawn(t *testing.T, bot *ScenarioBot) uint32 {
	t.Helper()
	db := bot.withWorldDB(t)

	var guid uint32
	if err := db.QueryRow(
		"SELECT guid FROM creature WHERE id = ? AND map = 0 ORDER BY guid LIMIT 1",
		ArenaBattlemasterEntry).Scan(&guid); err != nil {
		Preconditionf(t, "no Arena Battlemaster (entry %d) spawned on map 0: %v", ArenaBattlemasterEntry, err)
	}
	return guid
}
