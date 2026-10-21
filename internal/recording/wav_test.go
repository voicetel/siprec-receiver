package recording

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriterRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wav")

	w, err := Open(path, 8000, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// One second of samples.
	pcm := make([]int16, 8000)
	for i := range pcm {
		pcm[i] = int16(i % 2000)
	}

	if n, err := w.Append(pcm); err != nil || n != 8000 {
		t.Fatalf("Append = %d, %v", n, err)
	}

	if got := w.DurationSec(); got != 1 {
		t.Errorf("DurationSec = %d, want 1", got)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Close is idempotent.
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if len(data) != 44+16000 {
		t.Errorf("file size = %d, want %d", len(data), 44+16000)
	}

	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Errorf("missing RIFF/WAVE header: %q", data[:12])
	}
}

func TestOpenInvalidParams(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "x.wav"), 0, 1); err == nil {
		t.Error("expected error for sampleRate 0")
	}
}
