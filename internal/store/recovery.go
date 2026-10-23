package store

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/voicetel/siprec-receiver/internal/recording"
)

// KnownWAVPaths returns every WAV path referenced by a leg row, as a
// set, normalized to absolute paths for comparison with a directory
// listing.
func (s *Store) KnownWAVPaths(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT wav_path FROM legs`)
	if err != nil {
		return nil, fmt.Errorf("store: known wav paths: %w", err)
	}

	defer func() { _ = rows.Close() }()

	out := map[string]bool{}

	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: scan wav path: %w", err)
		}

		if abs, err := filepath.Abs(p); err == nil {
			out[abs] = true
		} else {
			out[p] = true
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: known wav paths rows: %w", err)
	}

	return out, nil
}

// AdoptOrphan inserts a completed call + single leg for a WAV file
// recovered from disk (see RecoverOrphans), both or neither. sipCallID
// is the best available correlation key (the file stem); direction is
// marked "recovered-orphan" so the UI can tell these apart.
func (s *Store) AdoptOrphan(ctx context.Context, sipCallID, label, wavPath string, startedAt time.Time, durationSec int) error {
	now := time.Now().UTC()
	endedAt := startedAt.Add(time.Duration(durationSec) * time.Second)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: adopt orphan begin: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
INSERT INTO calls (sip_call_id, direction, started_at, ended_at, duration_sec,
	status, created_at, updated_at)
VALUES (?, 'recovered-orphan', ?, ?, ?, 'completed', ?, ?)
ON CONFLICT(sip_call_id) DO NOTHING;`,
		sipCallID, millis(startedAt), millis(endedAt), durationSec, millis(now), millis(now))
	if err != nil {
		return fmt.Errorf("store: adopt orphan call: %w", err)
	}

	if n, _ := res.RowsAffected(); n == 0 {
		return nil // already adopted
	}

	callPK, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: adopt orphan id: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO legs (call_id, label, wav_path, duration_sec, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(call_id, wav_path) DO NOTHING;`,
		callPK, label, wavPath, durationSec, millis(now))
	if err != nil {
		return fmt.Errorf("store: adopt orphan leg: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: adopt orphan commit: %w", err)
	}

	return nil
}

// RecoverOrphans adopts every *.wav in dir that no leg row references.
// Such files are what a crash or kill mid-recording leaves behind:
// the SIPREC session never sent BYE, so nothing finalized them. Each
// file's header is repaired (crashed files carry placeholder sizes),
// its duration is computed from the audio actually on disk, and it
// becomes a normal completed call whose start time is the file's
// modification time. Unrecoverable files (too short, not WAV) are
// logged and left in place. Returns how many files were adopted.
func (s *Store) RecoverOrphans(ctx context.Context, dir string) (int, error) {
	known, err := s.KnownWAVPaths(ctx)
	if err != nil {
		return 0, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("store: read recording dir: %w", err)
	}

	adopted := 0

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".wav") {
			continue
		}

		path := filepath.Join(dir, e.Name())

		abs, err := filepath.Abs(path)
		if err != nil || known[abs] || known[path] {
			continue
		}

		info, err := e.Info()
		if err != nil {
			log.Printf("[recovery] stat %s: %v (skipped)", path, err)

			continue
		}

		dur, err := recording.RepairWAV(path)
		if err != nil {
			log.Printf("[recovery] %v (left in place)", err)

			continue
		}

		callID, label := parseOrphanFilename(e.Name())

		if err := s.AdoptOrphan(ctx, callID, label, path, info.ModTime(), dur); err != nil {
			log.Printf("[recovery] adopt %s: %v", path, err)

			continue
		}

		adopted++
		log.Printf("[recovery] adopted orphan %s (call=%s label=%s duration=%ds)",
			path, callID, label, dur)
	}

	return adopted, nil
}

// parseOrphanFilename splits a recorded WAV's name
// ("<sanitised Call-ID>-<label>.wav") back into its parts. Sanitising
// replaced "/" "\" ":" with "_" and "@" with "_at_"; only the "@"
// mapping is safely reversible, so the recovered Call-ID is
// best-effort (it still groups and displays correctly).
func parseOrphanFilename(name string) (callID, label string) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))

	label = "recovered"
	callID = stem

	if i := strings.LastIndex(stem, "-"); i > 0 && i < len(stem)-1 {
		callID, label = stem[:i], stem[i+1:]
	}

	callID = strings.ReplaceAll(callID, "_at_", "@")

	return callID, label
}

// expiredCallIDs lists the calls that started before cutoff (the
// RETENTION_DAYS sweep). AMD-only rows age out by their notification
// time, which is their started_at; a row with no start never expires.
func (s *Store) expiredCallIDs(ctx context.Context, cutoff time.Time) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM calls WHERE started_at > 0 AND started_at < ? ORDER BY id`, millis(cutoff))
	if err != nil {
		return nil, fmt.Errorf("store: retention select: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var ids []int64

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: retention scan: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: retention rows: %w", err)
	}

	return ids, nil
}
