package client

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildStatus writes the fixed head of SMSG_BATTLEFIELD_STATUS exactly as
// BattlegroundMgr::BuildBattlegroundStatusPacket does.
func buildStatus(queueSlot uint32, arenaType ArenaTeamType, isArena bool, bgTypeID uint32,
	minLevel, maxLevel uint8, clientInstance uint32, isRated bool, status uint32) *bytes.Buffer {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, queueSlot)
	buf.WriteByte(uint8(arenaType))
	if isArena {
		buf.WriteByte(0xE)
	} else {
		buf.WriteByte(0)
	}
	_ = binary.Write(buf, binary.LittleEndian, bgTypeID)
	_ = binary.Write(buf, binary.LittleEndian, battlefieldPortMagic)
	buf.WriteByte(minLevel)
	buf.WriteByte(maxLevel)
	_ = binary.Write(buf, binary.LittleEndian, clientInstance)
	buf.WriteByte(boolByte(isRated))
	_ = binary.Write(buf, binary.LittleEndian, status)
	return buf
}

func buildWaitQueue(bgTypeID uint32) []byte {
	buf := buildStatus(0, ArenaTeam2v2, true, bgTypeID, 80, 80, 0, true, BattlegroundStatusWaitQueue)
	_ = binary.Write(buf, binary.LittleEndian, uint32(0))
	_ = binary.Write(buf, binary.LittleEndian, uint32(0))
	return buf.Bytes()
}

func buildWaitJoin(bgTypeID, mapID uint32) []byte {
	buf := buildStatus(0, ArenaTeam2v2, true, bgTypeID, 80, 80, 0, true, BattlegroundStatusWaitJoin)
	_ = binary.Write(buf, binary.LittleEndian, mapID)
	_ = binary.Write(buf, binary.LittleEndian, uint64(0))
	_ = binary.Write(buf, binary.LittleEndian, uint32(60000))
	return buf.Bytes()
}

func TestParseBattlefieldStatus_ShortFormHasNoBattleground(t *testing.T) {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, uint32(2))
	_ = binary.Write(buf, binary.LittleEndian, uint64(0))

	st, err := ParseBattlefieldStatus(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if st.QueueSlot != 2 {
		t.Fatalf("queueSlot=%d want 2", st.QueueSlot)
	}
	if st.HasBattleground {
		t.Fatal("short form should not report a battleground")
	}
	if st.Status != BattlegroundStatusNone {
		t.Fatalf("status=%d want NONE", st.Status)
	}
}

func TestParseBattlefieldStatus_WaitQueue(t *testing.T) {
	buf := buildStatus(0, ArenaTeam2v2, true, 6, 80, 80, 0, true, BattlegroundStatusWaitQueue)
	_ = binary.Write(buf, binary.LittleEndian, uint32(4200)) // average wait
	_ = binary.Write(buf, binary.LittleEndian, uint32(1100)) // time in queue

	st, err := ParseBattlefieldStatus(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasBattleground {
		t.Fatal("want HasBattleground")
	}
	if st.Status != BattlegroundStatusWaitQueue {
		t.Fatalf("status=%s", BattlegroundStatusName(st.Status))
	}
	if st.ArenaType != ArenaTeam2v2 || !st.IsRated {
		t.Fatalf("arenaType=%d rated=%v", st.ArenaType, st.IsRated)
	}
	if st.BgTypeID != 6 {
		t.Fatalf("bgTypeID=%d want 6", st.BgTypeID)
	}
	if st.AverageWaitTime != 4200 || st.TimeInQueue != 1100 {
		t.Fatalf("avg=%d inQueue=%d", st.AverageWaitTime, st.TimeInQueue)
	}
}

func TestParseBattlefieldStatus_WaitJoinCarriesMap(t *testing.T) {
	st, err := ParseBattlefieldStatus(buildWaitJoin(6, 559)) // Nagrand Arena
	if err != nil {
		t.Fatal(err)
	}
	if st.MapID != 559 {
		t.Fatalf("mapID=%d want 559", st.MapID)
	}
	if st.TimeToRemove != 60000 {
		t.Fatalf("timeToRemove=%d", st.TimeToRemove)
	}
}

func TestParseBattlefieldStatus_InProgressCarriesSide(t *testing.T) {
	buf := buildStatus(0, ArenaTeam3v3, true, 6, 80, 80, 0, true, BattlegroundStatusInProgress)
	_ = binary.Write(buf, binary.LittleEndian, uint32(559))
	_ = binary.Write(buf, binary.LittleEndian, uint64(0))
	_ = binary.Write(buf, binary.LittleEndian, uint32(0))    // auto leave
	_ = binary.Write(buf, binary.LittleEndian, uint32(9000)) // since start
	buf.WriteByte(1)                                         // alliance

	st, err := ParseBattlefieldStatus(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if st.TimeFromStart != 9000 {
		t.Fatalf("timeFromStart=%d", st.TimeFromStart)
	}
	if !st.IsAlliance {
		t.Fatal("want alliance side")
	}
}

func TestParseBattlefieldStatus_RejectsTruncatedTail(t *testing.T) {
	full := buildWaitJoin(6, 559)
	for n := battlefieldStatusHeadLen; n < len(full); n++ {
		if _, err := ParseBattlefieldStatus(full[:n]); err == nil {
			t.Errorf("WAIT_JOIN truncated to %d bytes parsed without error", n)
		}
	}
	if _, err := ParseBattlefieldStatus(full); err != nil {
		t.Fatalf("full WAIT_JOIN rejected: %v", err)
	}
}

func TestParseBattlefieldStatus_RejectsOddLengths(t *testing.T) {
	for _, n := range []int{2, 11, 13, 22} {
		if _, err := ParseBattlefieldStatus(make([]byte, n)); err == nil {
			t.Errorf("%d byte packet parsed without error", n)
		}
	}
}

func TestBattlegroundStatusName(t *testing.T) {
	if got := BattlegroundStatusName(BattlegroundStatusWaitJoin); got != "WAIT_JOIN" {
		t.Fatalf("name=%s", got)
	}
	if got := BattlegroundStatusName(99); got != "STATUS_99" {
		t.Fatalf("name=%s", got)
	}
}

func TestBattlefieldStatuses_KeepEveryPacketInOrder(t *testing.T) {
	w := NewWorldClient("u", nil, nil)
	w.handleBattlefieldStatus(buildWaitQueue(6))
	w.handleBattlefieldStatus(buildWaitJoin(6, 559))

	got := w.BattlefieldStatuses()
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].Status != BattlegroundStatusWaitQueue || got[1].Status != BattlegroundStatusWaitJoin {
		t.Fatalf("order=%s,%s", BattlegroundStatusName(got[0].Status), BattlegroundStatusName(got[1].Status))
	}
	last, ok := w.LastBattlefieldStatus()
	if !ok || last.MapID != 559 {
		t.Fatalf("last ok=%v map=%d", ok, last.MapID)
	}
}

func TestBattlefieldStatuses_DrainForgets(t *testing.T) {
	w := NewWorldClient("u", nil, nil)
	w.handleBattlefieldStatus(buildWaitQueue(6))
	w.DrainBattlefieldStatuses()

	if got := w.BattlefieldStatuses(); len(got) != 0 {
		t.Fatalf("len=%d after drain", len(got))
	}
	if _, ok := w.LastBattlefieldStatus(); ok {
		t.Fatal("last status survived the drain")
	}
}

func TestBattlefieldStatuses_ReturnsACopy(t *testing.T) {
	w := NewWorldClient("u", nil, nil)
	w.handleBattlefieldStatus(buildWaitQueue(6))

	got := w.BattlefieldStatuses()
	got[0].Status = 99
	if w.BattlefieldStatuses()[0].Status == 99 {
		t.Fatal("caller mutated the client's history")
	}
}

func TestParseBattlegroundJoinResult_Refusal(t *testing.T) {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, ErrArenaTeamPartySize)

	r, err := ParseBattlegroundJoinResult(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Refused() || r.Result != ErrArenaTeamPartySize {
		t.Fatalf("result=%d refused=%v", r.Result, r.Refused())
	}
	if got := BattlegroundJoinResultName(r.Result); got != "ERR_ARENA_TEAM_PARTY_SIZE" {
		t.Fatalf("name=%s", got)
	}
}

func TestParseBattlegroundJoinResult_PositiveIsJoined(t *testing.T) {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, int32(6))

	r, err := ParseBattlegroundJoinResult(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if r.Refused() {
		t.Fatal("bg id 6 reported as a refusal")
	}
	if got := BattlegroundJoinResultName(r.Result); got != "JOINED_BG_6" {
		t.Fatalf("name=%s", got)
	}
}

func TestParseBattlegroundJoinResult_JoinFailedCarriesGUID(t *testing.T) {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, ErrBattlegroundJoinFailed)
	_ = binary.Write(buf, binary.LittleEndian, uint64(0x1234))

	r, err := ParseBattlegroundJoinResult(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if r.PlayerGUID != 0x1234 {
		t.Fatalf("guid=%#x", r.PlayerGUID)
	}
}

func TestParseArenaError_CarriesTeamType(t *testing.T) {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, uint32(0))
	buf.WriteByte(uint8(ArenaTeam3v3))

	teamType, err := ParseArenaError(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if teamType != ArenaTeam3v3 {
		t.Fatalf("teamType=%d", teamType)
	}
}

func TestJoinResultAndArenaError_AreRemembered(t *testing.T) {
	w := NewWorldClient("u", nil, nil)
	if _, ok := w.LastBattlegroundJoinResult(); ok {
		t.Fatal("join result reported before any packet")
	}

	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, ErrBattlegroundQueuedForRated)
	w.handleGroupJoinedBattleground(buf.Bytes())

	r, ok := w.LastBattlegroundJoinResult()
	if !ok || r.Result != ErrBattlegroundQueuedForRated {
		t.Fatalf("ok=%v result=%d", ok, r.Result)
	}

	w.handleArenaError([]byte{0, 0, 0, 0, uint8(ArenaTeam2v2)})
	if got := w.LastArenaError(); got != ArenaTeam2v2 {
		t.Fatalf("arena error team type=%d", got)
	}
}
