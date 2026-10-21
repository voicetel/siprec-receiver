package recording

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepairWAV_PatchesCrashHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.wav")

	w, err := Open(path, 8000, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := w.Append(make([]int16, 12000)); err != nil { // 1.5 s
		t.Fatalf("Append: %v", err)
	}

	// Simulate crash: header sizes never patched. The Writer is
	// abandoned without Close (exactly the crash shape).
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	if _, err := f.WriteAt([]byte{0, 0, 0, 0}, 4); err != nil {
		t.Fatal(err)
	}

	if _, err := f.WriteAt([]byte{0, 0, 0, 0}, 40); err != nil {
		t.Fatal(err)
	}

	_ = f.Close()

	dur, err := RepairWAV(path)
	if err != nil {
		t.Fatalf("RepairWAV: %v", err)
	}

	if dur != 2 { // 1.5 s rounds up
		t.Errorf("duration = %d, want 2", dur)
	}

	// And the repaired file now has correct sizes (idempotent).
	dur2, err := RepairWAV(path)
	if err != nil || dur2 != dur {
		t.Errorf("second RepairWAV = %d, %v; want %d", dur2, err, dur)
	}
}

func TestRepairWAV_RejectsNonWAV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.wav")
	if err := os.WriteFile(path, []byte("not a wav file at all, nope, no"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := RepairWAV(path); err == nil {
		t.Error("expected error for non-WAV file")
	}

	short := filepath.Join(t.TempDir(), "short.wav")
	if err := os.WriteFile(short, []byte("RIFF"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := RepairWAV(short); err == nil {
		t.Error("expected error for truncated file")
	}
}
