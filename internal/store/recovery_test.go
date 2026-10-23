package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/recording"
	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

// crashWAV writes a WAV the way a killed process leaves it: valid
// samples on disk, header sizes still zeroed (Writer.Close never ran).
func crashWAV(t *testing.T, path string, seconds int) {
	t.Helper()

	w, err := recording.Open(path, 8000, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := w.Append(make([]int16, 8000*seconds)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Simulate the crash: zero the size fields Close would patch.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	zero := []byte{0, 0, 0, 0}
	if _, err := f.WriteAt(zero, 4); err != nil {
		t.Fatalf("zero riff size: %v", err)
	}

	if _, err := f.WriteAt(zero, 40); err != nil {
		t.Fatalf("zero data size: %v", err)
	}

	_ = f.Close()
}

func TestRecoverOrphans_AdoptsCrashedRecording(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	orphan := filepath.Join(dir, "dlg-9_at_fs.example.com-12165285911.wav")
	crashWAV(t, orphan, 2)

	// A junk file must be left alone, not adopted or deleted.
	junk := filepath.Join(dir, "not-a-wav.wav")
	if err := os.WriteFile(junk, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := s.RecoverOrphans(ctx, dir)
	if err != nil {
		t.Fatalf("RecoverOrphans: %v", err)
	}

	if n != 1 {
		t.Fatalf("adopted = %d, want 1", n)
	}

	calls, total, _ := s.ListCalls(ctx, ListFilter{})
	if total != 1 || len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", total)
	}

	c := calls[0]
	if c.SipCallID != "dlg-9@fs.example.com" || c.Direction != "recovered-orphan" {
		t.Errorf("sip_call_id/direction = %q/%q", c.SipCallID, c.Direction)
	}

	if _, err := recording.RepairWAV(orphan); err != nil {
		t.Fatalf("adopted file not playable: %v", err)
	}

	// Second run adopts nothing new.
	n2, _ := s.RecoverOrphans(ctx, dir)
	if n2 != 0 {
		t.Errorf("second recovery adopted %d, want 0", n2)
	}
}

func TestPurgeExpiredDeletesOnlyOldCalls(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	old := time.Now().AddDate(0, 0, -40)
	recent := time.Now()

	for _, tc := range []struct {
		callID  string
		started time.Time
	}{
		{"old-1", old}, {"new-1", recent},
	} {
		wav := filepath.Join(dir, tc.callID+".wav")
		if err := os.WriteFile(wav, []byte("RIFF"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
			CallID: tc.callID, WAVPath: wav, DurationSec: 1, StartedAt: tc.started,
		}); err != nil {
			t.Fatalf("RecordLeg %s: %v", tc.callID, err)
		}
	}

	calls, files, err := s.PurgeExpired(ctx, 30)
	if err != nil || calls != 1 || files != 1 {
		t.Fatalf("PurgeExpired = %d calls %d files, %v; want 1 and 1", calls, files, err)
	}

	if _, err := os.Stat(filepath.Join(dir, "old-1.wav")); !os.IsNotExist(err) {
		t.Errorf("expired WAV still on disk: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "new-1.wav")); err != nil {
		t.Errorf("recent WAV removed: %v", err)
	}

	if _, total, _ := s.ListCalls(ctx, ListFilter{}); total != 1 {
		t.Errorf("remaining calls = %d, want 1", total)
	}
}

func TestRecordLeg_WriteErrorPersisted(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-err", WAVPath: "/rec/err.wav", DurationSec: 1,
		StartedAt: time.Now(), WriteErr: "write /rec/err.wav: no space left on device",
	})
	if err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	calls, _, _ := s.ListCalls(ctx, ListFilter{})
	if len(calls) != 1 || !calls[0].HasWriteError {
		t.Fatalf("HasWriteError not set: %+v", calls)
	}

	call, _ := s.GetCall(ctx, calls[0].ID)
	if len(call.Legs) != 1 || call.Legs[0].WriteError == "" {
		t.Errorf("leg write_error not persisted: %+v", call.Legs)
	}
}
