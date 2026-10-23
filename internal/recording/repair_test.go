package recording

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
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

// Any WAV but the exact one the Writer emits is refused and left
// byte-for-byte alone: patching its offsets 4 and 40 would corrupt it.
func TestRepairWAV_LeavesOtherWAVsAlone(t *testing.T) {
	for name, mutate := range map[string]func(h []byte){
		"LIST chunk before data": func(h []byte) { copy(h[36:40], "LIST") },
		"no fmt chunk at 12":     func(h []byte) { copy(h[12:16], "JUNK") },
		"larger fmt chunk":       func(h []byte) { binary.LittleEndian.PutUint32(h[16:], 18) },
		"not PCM":                func(h []byte) { binary.LittleEndian.PutUint16(h[20:], 3) },
		"stereo":                 func(h []byte) { binary.LittleEndian.PutUint16(h[22:], 2) },
		"44.1 kHz":               func(h []byte) { binary.LittleEndian.PutUint32(h[24:], 44100) },
		"byte rate":              func(h []byte) { binary.LittleEndian.PutUint32(h[28:], 8000) },
		"block align":            func(h []byte) { binary.LittleEndian.PutUint16(h[32:], 4) },
		"8-bit":                  func(h []byte) { binary.LittleEndian.PutUint16(h[34:], 8) },
	} {
		h := crashHeader()
		mutate(h)

		path := filepath.Join(t.TempDir(), "other.wav")
		body := append(h, make([]byte, 64)...)

		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := RepairWAV(path)
		if err == nil || !strings.Contains(err.Error(), "not a header this recorder writes") {
			t.Errorf("%s: err = %v", name, err)
		}

		if got, _ := os.ReadFile(path); !bytes.Equal(got, body) {
			t.Errorf("%s: the file was modified", name)
		}
	}
}
