package store

import (
	"context"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

const (
	parentSid = "CA0000000000000000000000000000d001"
	childSid  = "CA0000000000000000000000000000d002"
)

// recordedParent stores a recording whose call is parentSid.
func recordedParent(t *testing.T, s *Store) int64 {
	t.Helper()

	id := recordSimpleLeg(t, s, "dlg-parent", time.Now())
	mustExec(t, s, `UPDATE calls SET call_sid = ?, disp_status = 'in-progress' WHERE id = ?`, parentSid, id)

	return id
}

// A <Dial> child leg's callbacks carry its own CallSid plus
// ParentCallSid. The child is its own call, linked to the recorded
// parent; the parent's outcome is left alone.
func TestDialLegLinksToItsParent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	parent := recordedParent(t, s)

	child, matched, err := s.ApplyDisposition(ctx, DispositionNotification{
		CallSid: childSid, ParentCallSid: parentSid, CallStatus: "completed", DurationSec: 42, To: "+15550001111",
	})
	if err != nil || matched || child == parent {
		t.Fatalf("child disposition = %d matched %v, %v (parent %d)", child, matched, err, parent)
	}

	c, _ := s.GetCall(ctx, child)
	if c.ParentCallSid != parentSid || c.ParentID != parent {
		t.Errorf("child links to %q (row %d), want %q (row %d)", c.ParentCallSid, c.ParentID, parentSid, parent)
	}

	p, _ := s.GetCall(ctx, parent)
	if p.DispStatus != "in-progress" || p.ParentCallSid != "" || p.ParentID != 0 {
		t.Errorf("parent changed: status %q parent %q/%d", p.DispStatus, p.ParentCallSid, p.ParentID)
	}

	legs, err := s.DialLegs(ctx, parentSid)
	if err != nil || len(legs) != 1 || legs[0].ID != child || legs[0].DispDurationSec != 42 {
		t.Fatalf("DialLegs = %+v, %v", legs, err)
	}

	// Searching for the parent's CallSid finds the leg it dialed too.
	if calls, n, _ := s.ListCalls(ctx, ListFilter{Query: parentSid}); n != 2 || len(calls) != 2 {
		t.Errorf("search by parent CallSid = %d rows, want parent and child", n)
	}
}

// The Dial-leg AMD callback names no parent. Its row is linked when the
// leg's status callback (which does) arrives, in either order, and a
// later event without a parent never unlinks it.
func TestDialLegAMDLinksOnceTheStatusCallbackArrives(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	parent := recordedParent(t, s)

	id, _, err := s.ApplyAMD(ctx, AMDNotification{CallSid: childSid, AnsweredBy: "machine_start"})
	if err != nil {
		t.Fatal(err)
	}

	if c, _ := s.GetCall(ctx, id); c.ParentCallSid != "" {
		t.Fatalf("parent before any callback named it: %q", c.ParentCallSid)
	}

	if _, _, err := s.ApplyDisposition(ctx, DispositionNotification{
		CallSid: childSid, ParentCallSid: parentSid, CallStatus: "completed",
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.ApplyAMD(ctx, AMDNotification{CallSid: childSid, AnsweredBy: "human"}); err != nil {
		t.Fatal(err)
	}

	c, _ := s.GetCall(ctx, id)
	if c.ParentCallSid != parentSid || c.ParentID != parent || c.AMDAnsweredBy != "human" {
		t.Errorf("child = parent %q/%d amd %q", c.ParentCallSid, c.ParentID, c.AMDAnsweredBy)
	}
}

// An AMD callback that does name the parent links a new row, and one
// on an existing row fills the link.
func TestAMDCarriesTheParent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.ApplyAMD(ctx, AMDNotification{CallSid: childSid, ParentCallSid: parentSid, AnsweredBy: "human"})
	if err != nil {
		t.Fatal(err)
	}

	// The parent is not stored here.
	if c, _ := s.GetCall(ctx, id); c.ParentCallSid != parentSid || c.ParentID != 0 {
		t.Errorf("inserted child = %q/%d", c.ParentCallSid, c.ParentID)
	}

	other, _, err := s.ApplyDisposition(ctx, DispositionNotification{CallSid: "CA0000000000000000000000000000d003", CallStatus: "ringing"})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.ApplyAMD(ctx, AMDNotification{
		CallSid: "CA0000000000000000000000000000d003", ParentCallSid: parentSid, AnsweredBy: "human",
	}); err != nil {
		t.Fatal(err)
	}

	if c, _ := s.GetCall(ctx, other); c.ParentCallSid != parentSid {
		t.Errorf("updated row parent = %q", c.ParentCallSid)
	}
}

// A sender that names a call as its own parent links nothing.
func TestCallNeverParentsItself(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.ApplyDisposition(ctx, DispositionNotification{CallSid: childSid, ParentCallSid: childSid, CallStatus: "completed"})
	if err != nil {
		t.Fatal(err)
	}

	if c, _ := s.GetCall(ctx, id); c.ParentCallSid != "" {
		t.Errorf("self parent stored: %q", c.ParentCallSid)
	}

	if legs, _ := s.DialLegs(ctx, childSid); len(legs) != 0 {
		t.Errorf("a call is its own dial leg: %+v", legs)
	}
}

// The fetched VoiceML record links a leg no callback linked, without
// replacing a link a callback already made.
func TestFetchedRecordLinksTheParent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.ApplyAMD(ctx, AMDNotification{CallSid: childSid, AnsweredBy: "human"})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SaveVoiceMLRecord(ctx, id, VoiceMLRecord{Sid: childSid, ParentCallSid: parentSid}, `{}`); err != nil {
		t.Fatal(err)
	}

	if err := s.SaveVoiceMLRecord(ctx, id, VoiceMLRecord{Sid: childSid, ParentCallSid: "CA0000000000000000000000000000ffff"}, `{}`); err != nil {
		t.Fatal(err)
	}

	if c, _ := s.GetCall(ctx, id); c.ParentCallSid != parentSid {
		t.Errorf("parent after fetches = %q, want %q", c.ParentCallSid, parentSid)
	}
}

// Rows folded together keep the parent link either of them had.
func TestMergeKeepsTheParentLink(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, _, err := s.ApplyDisposition(ctx, DispositionNotification{
		FsUUID: "fs-dial", ParentCallSid: parentSid, CallStatus: "ringing",
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.ApplyAMD(ctx, AMDNotification{CallSid: childSid, AnsweredBy: "machine_start"}); err != nil {
		t.Fatal(err)
	}

	// The CallSid's row survives; the link comes from the FsUUID's.
	id, matched, err := s.ApplyAMD(ctx, AMDNotification{FsUUID: "fs-dial", CallSid: childSid, AnsweredBy: "human"})
	if err != nil || !matched {
		t.Fatalf("merge = %v, %v", matched, err)
	}

	if n := countCalls(t, s); n != 1 {
		t.Fatalf("%d rows after merge", n)
	}

	if c, _ := s.GetCall(ctx, id); c.ParentCallSid != parentSid {
		t.Errorf("merged parent = %q", c.ParentCallSid)
	}
}

func TestDialLegsErrors(t *testing.T) {
	s := openTestStore(t)

	if legs, err := s.DialLegs(context.Background(), ""); legs != nil || err != nil {
		t.Errorf("DialLegs(\"\") = %v, %v", legs, err)
	}

	_ = s.Close()

	_, err := s.DialLegs(context.Background(), parentSid)
	wantErr(t, err, "list dial legs")
}

func TestMigrate_ParentIndexFails(t *testing.T) {
	s := openTestStore(t)
	mustExec(t, s, `DROP INDEX idx_calls_parent_call_sid`)

	denyWhen(t, s, func(op int, arg1, _ string) bool {
		return op == sqlite3.SQLITE_CREATE_INDEX && arg1 == "idx_calls_parent_call_sid"
	})
	wantErr(t, s.migrate(context.Background()), "migrate call indexes")
}
