package client

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Battleground / arena opcodes (3.3.5a / AzerothCore Opcodes.h).
const (
	SmsgBattlefieldStatus       uint16 = 0x02D4
	CmsgBattlefieldPort         uint16 = 0x02D5
	CmsgLeaveBattlefield        uint16 = 0x02E1
	SmsgGroupJoinedBattleground uint16 = 0x02E8
	CmsgBattlemasterJoin        uint16 = 0x02EE
	CmsgArenaTeamInvite         uint16 = 0x034F
	CmsgArenaTeamAccept         uint16 = 0x0351
	CmsgBattlemasterJoinArena   uint16 = 0x0358
	SmsgArenaError              uint16 = 0x0376
)

// Battleground status ids (Battleground.h BattlegroundStatus).
const (
	BattlegroundStatusNone       uint32 = 0
	BattlegroundStatusWaitQueue  uint32 = 1
	BattlegroundStatusWaitJoin   uint32 = 2
	BattlegroundStatusInProgress uint32 = 3
	BattlegroundStatusWaitLeave  uint32 = 4
)

// ArenaTeamType is the player count per side (ArenaTeam.h ArenaTeamTypes). It is also
// what SMSG_BATTLEFIELD_STATUS reports as the arena type.
type ArenaTeamType uint8

const (
	ArenaTeam2v2 ArenaTeamType = 2
	ArenaTeam3v3 ArenaTeamType = 3
	ArenaTeam5v5 ArenaTeamType = 5
)

// ArenaSlot is the index CMSG_BATTLEMASTER_JOIN_ARENA takes (ArenaTeam.h ArenaTeamSlot).
// It is a distinct type from ArenaTeamType because the two carry different values for
// the same bracket and are otherwise trivially swapped: the server answers a bad slot by
// dropping the packet without a reply.
type ArenaSlot uint8

const (
	ArenaSlot2v2 ArenaSlot = 0
	ArenaSlot3v3 ArenaSlot = 1
	ArenaSlot5v5 ArenaSlot = 2
)

// Group join results (SharedDefines.h GroupJoinBattlegroundResult). A positive value
// is the BattlemasterList id the group was queued for. Zero and below are refusals.
const (
	ErrGroupJoinBattlegroundFail       int32 = 0
	ErrBattlegroundNone                int32 = -1
	ErrGroupJoinBattlegroundDeserters  int32 = -2
	ErrArenaTeamPartySize              int32 = -3
	ErrBattlegroundTooManyQueues       int32 = -4
	ErrBattlegroundCannotQueueForRated int32 = -5
	ErrBattlegroundQueuedForRated      int32 = -6
	ErrBattlegroundTeamLeftQueue       int32 = -7
	ErrBattlegroundNotInBattleground   int32 = -8
	ErrBattlegroundJoinXPGain          int32 = -9
	ErrBattlegroundJoinRangeIndex      int32 = -10
	ErrBattlegroundJoinTimedOut        int32 = -11
	ErrBattlegroundJoinFailed          int32 = -12
	ErrLFGCantUseBattleground          int32 = -13
	ErrInRandomBG                      int32 = -14
	ErrInNonRandomBG                   int32 = -15
)

// battlefieldPortMagic is the uint16 the client sends and the server ignores
// (BuildBattlegroundStatusPacket writes it, HandleBattleFieldPortOpcode reads it).
const battlefieldPortMagic uint16 = 0x1F90

// BattlefieldPort actions (HandleBattleFieldPortOpcode).
const (
	BattlefieldPortLeaveQueue uint8 = 0
	BattlefieldPortEnter      uint8 = 1
)

// battlefieldStatusShortLen is the STATUS_NONE form: queue slot plus uint64(0).
const battlefieldStatusShortLen = 12

// battlefieldStatusHeadLen is the fixed part of every other form, up to and
// including the status id.
const battlefieldStatusHeadLen = 23

// battlefieldStatusTailLen is what the given status appends after the head. A packet
// short of head+tail is rejected rather than parsed: binary.Read leaves a field zeroed
// when it runs out of bytes, and a WAIT_JOIN whose map id silently reads back as 0 is
// worse than no status at all.
func battlefieldStatusTailLen(status uint32) int {
	switch status {
	case BattlegroundStatusWaitQueue:
		return 8 // average wait time, time in queue
	case BattlegroundStatusWaitJoin:
		return 16 // map id, unk uint64, time to remove
	case BattlegroundStatusInProgress:
		return 21 // map id, unk uint64, time to remove, time from start, faction
	default:
		return 0
	}
}

// BattlefieldStatus is a parsed SMSG_BATTLEFIELD_STATUS.
//
// The short 12 byte form (STATUS_NONE, or no battleground) carries a queue slot and
// nothing else. It parses with Status == BattlegroundStatusNone and HasBattleground
// false. Fields below Status are only filled for the status that carries them.
type BattlefieldStatus struct {
	QueueSlot       uint32
	HasBattleground bool

	ArenaType      ArenaTeamType
	BgTypeID       uint32
	MinLevel       uint8
	MaxLevel       uint8
	ClientInstance uint32 // CreateClientVisibleInstanceId; always 0 for arenas
	IsRated        bool
	Status         uint32

	AverageWaitTime uint32 // WAIT_QUEUE
	TimeInQueue     uint32 // WAIT_QUEUE
	MapID           uint32 // WAIT_JOIN, IN_PROGRESS
	TimeToRemove    uint32 // WAIT_JOIN: invite expiry; IN_PROGRESS: auto-leave
	TimeFromStart   uint32 // IN_PROGRESS
	IsAlliance      bool   // IN_PROGRESS: the side the player was placed on
}

// BattlegroundStatusName returns a short label for logging.
func BattlegroundStatusName(st uint32) string {
	switch st {
	case BattlegroundStatusNone:
		return "NONE"
	case BattlegroundStatusWaitQueue:
		return "WAIT_QUEUE"
	case BattlegroundStatusWaitJoin:
		return "WAIT_JOIN"
	case BattlegroundStatusInProgress:
		return "IN_PROGRESS"
	case BattlegroundStatusWaitLeave:
		return "WAIT_LEAVE"
	default:
		return fmt.Sprintf("STATUS_%d", st)
	}
}

// ParseBattlefieldStatus decodes SMSG_BATTLEFIELD_STATUS as built by
// BattlegroundMgr::BuildBattlegroundStatusPacket.
func ParseBattlefieldStatus(data []byte) (BattlefieldStatus, error) {
	var st BattlefieldStatus
	if len(data) != battlefieldStatusShortLen && len(data) < battlefieldStatusHeadLen {
		return st, fmt.Errorf("SMSG_BATTLEFIELD_STATUS: %d bytes, want %d or at least %d",
			len(data), battlefieldStatusShortLen, battlefieldStatusHeadLen)
	}

	r := bytes.NewReader(data)
	read := func(v any) { _ = binary.Read(r, binary.LittleEndian, v) }
	read(&st.QueueSlot)

	if len(data) == battlefieldStatusShortLen {
		return st, nil
	}
	st.HasBattleground = true

	var isArena, isRated uint8
	var magic uint16
	read(&st.ArenaType)
	read(&isArena)
	read(&st.BgTypeID)
	read(&magic)
	read(&st.MinLevel)
	read(&st.MaxLevel)
	read(&st.ClientInstance)
	read(&isRated)
	read(&st.Status)
	st.IsRated = isRated != 0

	if want := battlefieldStatusHeadLen + battlefieldStatusTailLen(st.Status); len(data) < want {
		return st, fmt.Errorf("SMSG_BATTLEFIELD_STATUS %s: %d bytes, want %d",
			BattlegroundStatusName(st.Status), len(data), want)
	}

	switch st.Status {
	case BattlegroundStatusWaitQueue:
		read(&st.AverageWaitTime)
		read(&st.TimeInQueue)
	case BattlegroundStatusWaitJoin:
		var unk uint64
		read(&st.MapID)
		read(&unk)
		read(&st.TimeToRemove)
	case BattlegroundStatusInProgress:
		var unk uint64
		var alliance uint8
		read(&st.MapID)
		read(&unk)
		read(&st.TimeToRemove)
		read(&st.TimeFromStart)
		read(&alliance)
		st.IsAlliance = alliance != 0
	}

	return st, nil
}

// BattlegroundJoinResult is a parsed SMSG_GROUP_JOINED_BATTLEGROUND, the server's
// verdict on a group join. Every member receives one: Result > 0 (the
// BattlemasterList id) means the group was queued, Result <= 0 is a refusal
// (ErrArenaTeamPartySize and friends).
type BattlegroundJoinResult struct {
	Result     int32
	PlayerGUID uint64 // ErrBattlegroundJoinTimedOut / ErrBattlegroundJoinFailed: the member at fault
}

// Refused reports whether the join was turned down.
func (r BattlegroundJoinResult) Refused() bool { return r.Result <= 0 }

// BattlegroundJoinResultName returns a short label for logging.
func BattlegroundJoinResultName(result int32) string {
	switch result {
	case ErrGroupJoinBattlegroundFail:
		return "ERR_GROUP_JOIN_BATTLEGROUND_FAIL"
	case ErrBattlegroundNone:
		return "ERR_BATTLEGROUND_NONE"
	case ErrGroupJoinBattlegroundDeserters:
		return "ERR_GROUP_JOIN_BATTLEGROUND_DESERTERS"
	case ErrArenaTeamPartySize:
		return "ERR_ARENA_TEAM_PARTY_SIZE"
	case ErrBattlegroundTooManyQueues:
		return "ERR_BATTLEGROUND_TOO_MANY_QUEUES"
	case ErrBattlegroundCannotQueueForRated:
		return "ERR_BATTLEGROUND_CANNOT_QUEUE_FOR_RATED"
	case ErrBattlegroundQueuedForRated:
		return "ERR_BATTLEGROUND_QUEUED_FOR_RATED"
	case ErrBattlegroundTeamLeftQueue:
		return "ERR_BATTLEGROUND_TEAM_LEFT_QUEUE"
	case ErrBattlegroundNotInBattleground:
		return "ERR_BATTLEGROUND_NOT_IN_BATTLEGROUND"
	case ErrBattlegroundJoinXPGain:
		return "ERR_BATTLEGROUND_JOIN_XP_GAIN"
	case ErrBattlegroundJoinRangeIndex:
		return "ERR_BATTLEGROUND_JOIN_RANGE_INDEX"
	case ErrBattlegroundJoinTimedOut:
		return "ERR_BATTLEGROUND_JOIN_TIMED_OUT"
	case ErrBattlegroundJoinFailed:
		return "ERR_BATTLEGROUND_JOIN_FAILED"
	case ErrLFGCantUseBattleground:
		return "ERR_LFG_CANT_USE_BATTLEGROUND"
	case ErrInRandomBG:
		return "ERR_IN_RANDOM_BG"
	case ErrInNonRandomBG:
		return "ERR_IN_NON_RANDOM_BG"
	}
	if result > 0 {
		return fmt.Sprintf("JOINED_BG_%d", result)
	}
	return fmt.Sprintf("RESULT_%d", result)
}

// ParseBattlegroundJoinResult decodes SMSG_GROUP_JOINED_BATTLEGROUND as built by
// BattlegroundMgr::BuildGroupJoinedBattlegroundPacket.
func ParseBattlegroundJoinResult(data []byte) (BattlegroundJoinResult, error) {
	var r BattlegroundJoinResult
	if len(data) < 4 {
		return r, fmt.Errorf("SMSG_GROUP_JOINED_BATTLEGROUND too short: %d", len(data))
	}
	r.Result = int32(binary.LittleEndian.Uint32(data))
	if (r.Result == ErrBattlegroundJoinTimedOut || r.Result == ErrBattlegroundJoinFailed) && len(data) >= 12 {
		r.PlayerGUID = binary.LittleEndian.Uint64(data[4:])
	}
	return r, nil
}

// ParseArenaError decodes SMSG_ARENA_ERROR ("You are not in a %uv%u arena team")
// and returns the team type the server asked for.
func ParseArenaError(data []byte) (ArenaTeamType, error) {
	if len(data) < 5 {
		return 0, fmt.Errorf("SMSG_ARENA_ERROR too short: %d", len(data))
	}
	if binary.LittleEndian.Uint32(data) != 0 {
		return 0, fmt.Errorf("SMSG_ARENA_ERROR: unknown form %d", binary.LittleEndian.Uint32(data))
	}
	return ArenaTeamType(data[4]), nil
}

// JoinBattlegroundQueue sends CMSG_BATTLEMASTER_JOIN at a Battlemaster. bgTypeID is
// the BattlemasterList id, instanceID 0 means "first available".
func (w *WorldClient) JoinBattlegroundQueue(battlemasterGUID uint64, bgTypeID, instanceID uint32, asGroup bool) error {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, battlemasterGUID)
	_ = binary.Write(buf, binary.LittleEndian, bgTypeID)
	_ = binary.Write(buf, binary.LittleEndian, instanceID)
	buf.WriteByte(boolByte(asGroup))
	return w.sendPacket(CmsgBattlemasterJoin, buf.Bytes())
}

// JoinArenaQueue sends CMSG_BATTLEMASTER_JOIN_ARENA at an Arena Battlemaster.
//
// The handler drops the request without a reply when the battlemaster is not in the
// player's own map, when the player is already inside a battleground, when isRated is
// set without asGroup, when asGroup is set and the sender is not the party leader,
// when a rated join is made while the arena season is not in progress, when arenas are
// disabled, and when the player's level has no bracket. Any other refusal comes back as
// SMSG_GROUP_JOINED_BATTLEGROUND (LastBattlegroundJoinResult) or, when the player has
// no team for the slot, as SMSG_ARENA_ERROR (LastArenaError).
func (w *WorldClient) JoinArenaQueue(battlemasterGUID uint64, arenaSlot ArenaSlot, asGroup, isRated bool) error {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, battlemasterGUID)
	buf.WriteByte(uint8(arenaSlot))
	buf.WriteByte(boolByte(asGroup))
	buf.WriteByte(boolByte(isRated))
	return w.sendPacket(CmsgBattlemasterJoinArena, buf.Bytes())
}

// BattlefieldPort answers a battlefield invite: enter the arena or leave the queue.
// Answer with the fields of the SMSG_BATTLEFIELD_STATUS being replied to.
func (w *WorldClient) BattlefieldPort(arenaType ArenaTeamType, bgTypeID uint32, action uint8) error {
	buf := new(bytes.Buffer)
	buf.WriteByte(uint8(arenaType))
	buf.WriteByte(0) // unk2
	_ = binary.Write(buf, binary.LittleEndian, bgTypeID)
	_ = binary.Write(buf, binary.LittleEndian, battlefieldPortMagic)
	buf.WriteByte(action)
	return w.sendPacket(CmsgBattlefieldPort, buf.Bytes())
}

// LeaveBattlefieldQueue answers st with "leave queue".
//
// A group is only erased from its bracket once its last member is gone. Logging out
// counts, so a queue does drain itself, but only when the server gets round to the
// disconnect. Leaving explicitly is how a caller decides when.
func (w *WorldClient) LeaveBattlefieldQueue(st BattlefieldStatus) error {
	return w.BattlefieldPort(st.ArenaType, st.BgTypeID, BattlefieldPortLeaveQueue)
}

// EnterBattlefield answers st with "enter battle".
func (w *WorldClient) EnterBattlefield(st BattlefieldStatus) error {
	return w.BattlefieldPort(st.ArenaType, st.BgTypeID, BattlefieldPortEnter)
}

// LeaveBattlefield sends CMSG_LEAVE_BATTLEFIELD, which leaves a battleground the
// player is inside. Leaving a queue is BattlefieldPort / LeaveBattlefieldQueue.
func (w *WorldClient) LeaveBattlefield(bgTypeID uint32) error {
	buf := new(bytes.Buffer)
	buf.WriteByte(0) // unk1
	buf.WriteByte(0) // unk2
	_ = binary.Write(buf, binary.LittleEndian, bgTypeID)
	_ = binary.Write(buf, binary.LittleEndian, uint16(0)) // unk3
	return w.sendPacket(CmsgLeaveBattlefield, buf.Bytes())
}

// InviteToArenaTeam sends CMSG_ARENA_TEAM_INVITE for the given team.
func (w *WorldClient) InviteToArenaTeam(teamID uint32, inviteeName string) error {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, teamID)
	buf.WriteString(inviteeName)
	buf.WriteByte(0)
	return w.sendPacket(CmsgArenaTeamInvite, buf.Bytes())
}

// AcceptArenaTeamInvite sends CMSG_ARENA_TEAM_ACCEPT (empty payload on 3.3.5a).
func (w *WorldClient) AcceptArenaTeamInvite() error {
	return w.sendPacket(CmsgArenaTeamAccept, nil)
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func (w *WorldClient) handleBattlefieldStatus(data []byte) {
	st, err := ParseBattlefieldStatus(data)
	if err != nil {
		w.logAt(LogWarn, "SMSG_BATTLEFIELD_STATUS parse: %v", err)
		return
	}

	w.bfMu.Lock()
	w.battlefieldStatuses = append(w.battlefieldStatuses, st)
	w.bfMu.Unlock()

	// Sparse (queue join, pop, entry), so Info is the right volume.
	where := ""
	if st.MapID != 0 {
		where = fmt.Sprintf(" map=%d", st.MapID)
	}
	w.logAt(LogInfo, "SMSG_BATTLEFIELD_STATUS slot=%d %s arenaType=%d rated=%v%s",
		st.QueueSlot, BattlegroundStatusName(st.Status), st.ArenaType, st.IsRated, where)
	w.invokeBattlefieldStatusHooks(st)
}

func (w *WorldClient) handleGroupJoinedBattleground(data []byte) {
	r, err := ParseBattlegroundJoinResult(data)
	if err != nil {
		w.logAt(LogWarn, "SMSG_GROUP_JOINED_BATTLEGROUND parse: %v", err)
		return
	}

	w.bfMu.Lock()
	w.lastBattlegroundJoinResult = r
	w.battlegroundJoinResultSeen = true
	w.bfMu.Unlock()

	if r.Refused() {
		w.logAt(LogWarn, "SMSG_GROUP_JOINED_BATTLEGROUND %s", BattlegroundJoinResultName(r.Result))
	} else {
		w.logAt(LogInfo, "SMSG_GROUP_JOINED_BATTLEGROUND %s", BattlegroundJoinResultName(r.Result))
	}
}

func (w *WorldClient) handleArenaError(data []byte) {
	teamType, err := ParseArenaError(data)
	if err != nil {
		w.logAt(LogWarn, "SMSG_ARENA_ERROR parse: %v", err)
		return
	}

	w.bfMu.Lock()
	w.lastArenaErrorTeamType = teamType
	w.bfMu.Unlock()

	w.logAt(LogWarn, "SMSG_ARENA_ERROR not in a %dv%d arena team", teamType, teamType)
}

// BattlefieldStatuses returns every SMSG_BATTLEFIELD_STATUS received since login or
// the last DrainBattlefieldStatuses, oldest first.
//
// A queue produces a sequence (WAIT_QUEUE, then WAIT_JOIN on the next matchmaker
// sweep, which can be immediate), so waiters scan this rather than the last packet
// or a hook armed after the fact.
func (w *WorldClient) BattlefieldStatuses() []BattlefieldStatus {
	w.bfMu.RLock()
	defer w.bfMu.RUnlock()
	return append([]BattlefieldStatus(nil), w.battlefieldStatuses...)
}

// DrainBattlefieldStatuses forgets the statuses received so far, and with them the
// last join result and arena error. Call it before re-queueing, so a waiter does not
// match the previous queue's packets and a failure does not quote the previous
// queue's refusal.
func (w *WorldClient) DrainBattlefieldStatuses() {
	w.bfMu.Lock()
	w.battlefieldStatuses = nil
	w.lastBattlegroundJoinResult = BattlegroundJoinResult{}
	w.battlegroundJoinResultSeen = false
	w.lastArenaErrorTeamType = 0
	w.bfMu.Unlock()
}

// LastBattlefieldStatus returns the most recent SMSG_BATTLEFIELD_STATUS, or false
// when none has arrived since the last drain.
func (w *WorldClient) LastBattlefieldStatus() (BattlefieldStatus, bool) {
	w.bfMu.RLock()
	defer w.bfMu.RUnlock()
	if len(w.battlefieldStatuses) == 0 {
		return BattlefieldStatus{}, false
	}
	return w.battlefieldStatuses[len(w.battlefieldStatuses)-1], true
}

// LastBattlegroundJoinResult returns the most recent SMSG_GROUP_JOINED_BATTLEGROUND,
// or false when none has arrived since login or the last DrainBattlefieldStatuses.
func (w *WorldClient) LastBattlegroundJoinResult() (BattlegroundJoinResult, bool) {
	w.bfMu.RLock()
	defer w.bfMu.RUnlock()
	return w.lastBattlegroundJoinResult, w.battlegroundJoinResultSeen
}

// LastArenaError returns the team type of the most recent SMSG_ARENA_ERROR ("not in
// a NvN arena team"), or 0 when none has arrived since login or the last
// DrainBattlefieldStatuses.
func (w *WorldClient) LastArenaError() ArenaTeamType {
	w.bfMu.RLock()
	defer w.bfMu.RUnlock()
	return w.lastArenaErrorTeamType
}
