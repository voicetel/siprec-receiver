package siprecsrv

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/voicetel/siprec-receiver/internal/recording"
	"github.com/voicetel/siprec-receiver/internal/rtp"
)

// wavLeg is a leg recording to a real WAV in dir.
func wavLeg(t *testing.T, pool *rtp.PortPool, dir, label string) *leg {
	t.Helper()

	path := filepath.Join(dir, label+".wav")

	w, err := recording.Open(path, sipRecordingSampleRate, 1)
	if err != nil {
		t.Fatal(err)
	}

	l := acquireLeg(t, pool, label, w)
	l.wavPath = path

	return l
}

func exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// A declared stream that never carries audio while another does (the
// second stream of mod_siprec's mixed fallback for a multichannel call)
// leaves no leg row and no file.
func TestSilentLegBesideAHeardOneIsDropped(t *testing.T) {
	srv, pool := newCovServer(t, 47320, 47324)
	f := &fakeFinalizer{}
	srv.SetFinalizer(f)

	dir := t.TempDir()
	heard, silent := wavLeg(t, pool, dir, "mixed"), wavLeg(t, pool, dir, "unused")
	heard.writer.append(make([]int16, 160))

	srv.shared.finalizeSession(context.Background(), &session{callID: "dlg-fallback", legs: []*leg{heard, silent}})

	legs := f.snapshot()
	if len(legs) != 1 || legs[0].Label != "mixed" || legs[0].NoAudio {
		t.Fatalf("finalized %+v, want only the heard leg", legs)
	}

	if !exists(heard.wavPath) || exists(silent.wavPath) {
		t.Errorf("files: heard %v, silent %v; want only the heard one kept", exists(heard.wavPath), exists(silent.wavPath))
	}

	if pool.Available() != 3 {
		t.Errorf("pool available = %d, want every port back", pool.Available())
	}
}

// When no leg heard anything (RTP never reached the SRS), the legs are
// kept and marked, so the call is still on record with the reason.
func TestAllSilentLegsAreKeptAndMarked(t *testing.T) {
	srv, pool := newCovServer(t, 47330, 47334)
	f := &fakeFinalizer{}
	srv.SetFinalizer(f)

	dir := t.TempDir()
	a, b := wavLeg(t, pool, dir, "a"), wavLeg(t, pool, dir, "b")

	srv.shared.finalizeSession(context.Background(), &session{callID: "dlg-blocked", legs: []*leg{a, b}})

	legs := f.snapshot()
	if len(legs) != 2 || !legs[0].NoAudio || !legs[1].NoAudio {
		t.Fatalf("finalized %+v, want both legs marked NoAudio", legs)
	}

	if !exists(a.wavPath) || !exists(b.wavPath) {
		t.Error("a kept leg lost its file")
	}
}

// An empty WAV that cannot be removed is logged; the leg is still not
// recorded.
func TestSilentLegRemoveFailure(t *testing.T) {
	srv, pool := newCovServer(t, 47340, 47344)
	f := &fakeFinalizer{}
	srv.SetFinalizer(f)

	dir := t.TempDir()
	heard, silent := wavLeg(t, pool, dir, "heard"), wavLeg(t, pool, dir, "silent")
	heard.writer.append(make([]int16, 160))

	// A non-empty directory where the file should be: os.Remove fails.
	silent.wavPath = filepath.Join(dir, "busy")
	if err := os.MkdirAll(filepath.Join(silent.wavPath, "x"), 0o750); err != nil {
		t.Fatal(err)
	}

	srv.shared.finalizeSession(context.Background(), &session{callID: "dlg-busy", legs: []*leg{heard, silent}})

	if legs := f.snapshot(); len(legs) != 1 || legs[0].Label != "heard" {
		t.Errorf("finalized %+v, want only the heard leg", legs)
	}
}
