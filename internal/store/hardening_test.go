package store

import (
	"context"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

// A second SIPREC dialog carrying an already-owned CallSid (re-record
// after a transfer, an SRC restart) must record cleanly under its
// own SIP Call-ID instead of failing the whole finalize on the
// UNIQUE(call_sid) index — previously the WAV was left with no row.
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
	if total != 2 || len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (one per dialog)", total)
	}

	owners := 0
	for _, c := range calls {
		if c.CallSid == callSid {
			owners++
		}
	}

	if owners != 1 {
		t.Errorf("rows owning the CallSid = %d, want exactly 1 (the first dialog)", owners)
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
