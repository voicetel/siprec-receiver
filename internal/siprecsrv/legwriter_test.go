package siprecsrv

import (
	"path/filepath"
	"testing"

	"github.com/voicetel/siprec-receiver/internal/recording"
)

// TestLegWriter_CapturesFirstWriteError: a leg whose WAV writes start
// failing (disk full in production; a closed writer here) must surface
// the error via Err() — previously the RTP callback dropped it and the
// recording silently truncated.
func TestLegWriter_CapturesFirstWriteError(t *testing.T) {
	w, err := recording.Open(filepath.Join(t.TempDir(), "x.wav"), 8000, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	lw := &legWriter{w: w, call: "test-call leg 1"}
	lw.append([]int16{1, 2, 3})

	if err := lw.Err(); err != nil {
		t.Fatalf("Err after good append: %v", err)
	}

	// Close the writer underneath; the next append fails.
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lw.append([]int16{4, 5, 6})

	if err := lw.Err(); err == nil {
		t.Fatal("Err is nil after failed append — write errors are still being dropped")
	}

	// duration/close stay safe after the failure.
	_ = lw.durationSec()
	_ = lw.close()
}
