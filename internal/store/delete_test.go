package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

// recordWAVLeg records a call whose leg's WAV exists at path.
func recordWAVLeg(t *testing.T, s *Store, callID, path string) int64 {
	t.Helper()

	if err := os.WriteFile(path, []byte("RIFF"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.RecordLeg(context.Background(), siprecsrv.FinalizedLeg{
		CallID: callID, Label: "a", WAVPath: path, DurationSec: 1, StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	var id int64
	if err := s.db.QueryRow(`SELECT id FROM calls WHERE sip_call_id = ?`, callID).Scan(&id); err != nil {
		t.Fatal(err)
	}

	return id
}

// Rows and files go together.
func TestDeleteCallRemovesRowsAndFiles(t *testing.T) {
	s := openTestStore(t)
	wav := filepath.Join(t.TempDir(), "a.wav")
	id := recordWAVLeg(t, s, "dlg-del-files", wav)

	paths, err := s.DeleteCall(context.Background(), id)
	if err != nil || len(paths) != 1 || paths[0] != wav {
		t.Fatalf("DeleteCall = %v, %v", paths, err)
	}

	if _, err := os.Stat(wav); !os.IsNotExist(err) {
		t.Errorf("WAV still on disk: %v", err)
	}

	if known, _ := s.KnownWAVPaths(context.Background()); len(known) != 0 {
		t.Errorf("leg rows left: %v", known)
	}
}

// A file that cannot be removed keeps the call and its legs: deleting
// the rows alone would leave the file to come back at the next start as
// a recovered orphan.
func TestDeleteCallKeepsTheCallWhenAFileStays(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	// A non-empty directory where the WAV should be: os.Remove fails.
	stuck := filepath.Join(dir, "stuck.wav")
	id := recordWAVLeg(t, s, "dlg-del-stuck", filepath.Join(dir, "plain.wav"))
	mustExec(t, s, `UPDATE legs SET wav_path = ? WHERE call_id = ?`, stuck, id)

	if err := os.MkdirAll(filepath.Join(stuck, "x"), 0o750); err != nil {
		t.Fatal(err)
	}

	_, err := s.DeleteCall(ctx, id)
	wantErr(t, err, "stuck.wav")

	if c, _ := s.GetCall(ctx, id); c == nil || len(c.Legs) != 1 {
		t.Fatalf("call after the failed delete = %+v", c)
	}

	// The orphan scan does not adopt what is still referenced.
	if n, _ := s.RecoverOrphans(ctx, dir); n != 0 {
		t.Errorf("RecoverOrphans adopted %d", n)
	}
}

func TestDeleteCallFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("leg paths query", func(t *testing.T) {
		s := openTestStore(t)
		id := recordSimpleLeg(t, s, "dlg-lp", time.Now())
		denyWhen(t, s, func(op int, arg1, arg2 string) bool {
			return op == sqlite3.SQLITE_READ && arg1 == "legs" && arg2 == "wav_path"
		})

		_, err := s.DeleteCall(ctx, id)
		wantErr(t, err, "leg paths of call")
	})

	t.Run("leg path scan", func(t *testing.T) {
		s := openTestStore(t)
		id := recordSimpleLeg(t, s, "dlg-ls", time.Now())
		mustExec(t, s, `ALTER TABLE legs RENAME TO legs_real`)
		mustExec(t, s, `CREATE VIEW legs AS SELECT id, call_id, NULL AS wav_path FROM legs_real`)

		_, err := s.DeleteCall(ctx, id)
		wantErr(t, err, "scan leg path")
	})

	t.Run("leg path rows", func(t *testing.T) {
		s := openTestStore(t)
		id := recordSimpleLeg(t, s, "dlg-lr", time.Now())
		addBoom(t, s)
		mustExec(t, s, `ALTER TABLE legs RENAME TO legs_real`)
		mustExec(t, s, `CREATE VIEW legs AS SELECT legs_real.id, call_id, abs(boom.x) AS wav_path FROM legs_real, boom`)

		_, err := s.DeleteCall(ctx, id)
		wantErr(t, err, "leg paths rows")
	})

	t.Run("commit", func(t *testing.T) {
		s := openTestStore(t)
		id := recordSimpleLeg(t, s, "dlg-commit", time.Now())
		failCommits(t, s)

		_, err := s.DeleteCall(ctx, id)
		wantErr(t, err, "commit")
	})
}
