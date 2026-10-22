package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

// A second SIPREC dialog for the same CallSid (SIPREC restarted on the
// call: VoiceML's second <Start><Siprec> replaces the first) records
// into the same call: both dialogs' legs, the CallSid and its
// notifications on one row, and the recorded time summed.
func TestRecordLeg_SecondDialogSameCallSid(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const callSid = "CAeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	for _, dlg := range []string{"dlg-first", "dlg-second"} {
		if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
			CallID: dlg, CallSid: callSid, WAVPath: "/rec/" + dlg + ".wav",
			DurationSec: 5, StartedAt: time.Now(),
		}); err != nil {
			t.Fatalf("RecordLeg %s: %v", dlg, err)
		}
	}

	calls, total, _ := s.ListCalls(ctx, ListFilter{})
	if total != 1 || len(calls) != 1 {
		t.Fatalf("calls = %d, want 1 (both dialogs on the CallSid's row)", total)
	}

	c, err := s.GetCall(ctx, calls[0].ID)
	if err != nil || c.CallSid != callSid || len(c.Legs) != 2 || c.DurationSec != 10 {
		t.Fatalf("call = %+v (%v), want CallSid, 2 legs, 10 s", c, err)
	}

	// A notification keyed by the second dialog's Call-ID finds it.
	id, matched, err := s.ApplyAMD(ctx, AMDNotification{SipCallID: "dlg-second", AnsweredBy: "human"})
	if err != nil || !matched || id != c.ID {
		t.Fatalf("AMD by second dialog: id=%d matched=%v err=%v, want call %d", id, matched, err, c.ID)
	}

	// Searching by the second dialog's Call-ID finds the call too.
	if _, n, _ := s.ListCalls(ctx, ListFilter{Query: "dlg-second"}); n != 1 {
		t.Errorf("search by second dialog = %d calls, want 1", n)
	}
}

// Two separate-stream legs of one dialog run side by side: the call
// lasts as long as the longer one, not their sum.
func TestRecordLeg_SeparateStreamLegsDoNotSum(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i, d := range []int{7, 6} {
		if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
			CallID: "dlg-sep", Label: fmt.Sprint("leg", i), WAVPath: fmt.Sprintf("/rec/sep%d.wav", i),
			DurationSec: d, StartedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	calls, _, _ := s.ListCalls(ctx, ListFilter{})
	if len(calls) != 1 || calls[0].DurationSec != 7 {
		t.Fatalf("calls = %+v, want one call of 7 s", calls)
	}
}

// Search wildcards must match literally: "%" is user text, not SQL.
func TestListCalls_LikeEscaping(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec, _ := modSiprecMetadata(t, "123e4567-e89b-12d3-a456-426614174003")
	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-like", WAVPath: "/rec/like.wav", DurationSec: 1,
		StartedAt: time.Now(), Metadata: rec,
	}); err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	for _, q := range []string{"%", "_", "100%"} {
		if _, total, _ := s.ListCalls(ctx, ListFilter{Query: q}); total != 0 {
			t.Errorf("query %q matched %d calls, want 0 (wildcards must be literal)", q, total)
		}
	}

	if _, total, _ := s.ListCalls(ctx, ListFilter{Query: "2165"}); total != 1 {
		t.Errorf("plain substring query matched %d, want 1", total)
	}
}

// Audit entries round-trip newest-first.
func TestAuditLog(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Audit(ctx, "admin@example.com", "user.upsert", "viewer@example.com", "role=user"); err != nil {
		t.Fatalf("Audit: %v", err)
	}

	if err := s.Audit(ctx, "admin@example.com", "recording.delete", "42", "1 file(s) removed"); err != nil {
		t.Fatalf("Audit: %v", err)
	}

	entries, err := s.ListAudit(ctx, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("ListAudit = %d, %v", len(entries), err)
	}

	if entries[0].Action != "recording.delete" || entries[1].Action != "user.upsert" {
		t.Errorf("order = %q, %q", entries[0].Action, entries[1].Action)
	}
}

// A user removed and added again starts at a fresh random epoch, so a
// cookie revoked before the removal (or minted before it) stays dead.
func TestUpsertUser_ReAddedUserGetsFreshEpoch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	add := func() int {
		t.Helper()

		if err := s.UpsertUser(ctx, User{Email: "re@example.com", Role: "user", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}

		u, _ := s.GetUser(ctx, "re@example.com")

		return u.SessionEpoch
	}

	first := add()
	if first <= 0 {
		t.Fatalf("new user epoch = %d, want a positive random value", first)
	}

	if again := add(); again != first {
		t.Errorf("re-saving an existing user changed its epoch %d -> %d", first, again)
	}

	if _, err := s.DeleteUser(ctx, "re@example.com"); err != nil {
		t.Fatal(err)
	}

	if readded := add(); readded == first || readded == first+1 {
		t.Errorf("re-added user epoch %d matches an old cookie's (%d)", readded, first)
	}
}
