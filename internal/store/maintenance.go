package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"
)

// This file is the store's settings + maintenance surface: the
// admin-configurable retention setting, the expired-call purge, and
// the SQLite upkeep (checkpoint / optimize / vacuum) the background
// maintenance timer in main drives.

// Setting keys.
const (
	SettingRetentionDays  = "retention_days"
	SettingLastPurgeAt    = "last_purge_at"
	SettingLastPurgeCalls = "last_purge_calls"
	SettingLastPurgeFiles = "last_purge_files"
)

// GetSetting returns a settings value and whether it was present.
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string

	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("store: get setting %s: %w", key, err)
	}

	return v, true, nil
}

// SetSetting writes a settings value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at;`,
		key, value, millis(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("store: set setting %s: %w", key, err)
	}

	return nil
}

// EffectiveRetentionDays resolves the active purge horizon: the
// admin UI's stored value wins once set; until then the
// RETENTION_DAYS environment default applies. fromUI reports which
// source won. An unparseable stored value falls back to the env
// default.
func (s *Store) EffectiveRetentionDays(ctx context.Context, envDefault int) (days int, fromUI bool, err error) {
	v, present, err := s.GetSetting(ctx, SettingRetentionDays)
	if err != nil {
		return envDefault, false, err
	}

	if !present {
		return envDefault, false, nil
	}

	n, perr := strconv.Atoi(v)
	if perr != nil || n < 0 {
		return envDefault, false, nil
	}

	return n, true, nil
}

// PurgeExpired deletes every call that started more than
// retentionDays ago — database rows and their WAV files — and
// records the run in settings for the admin UI. A retentionDays of
// 0 (or less) disables the purge and deletes nothing. AMD-only and
// disposition-only rows age by their notification time, which is
// their started_at. Each call goes as DeleteCall removes it; one whose
// files cannot be removed is kept, whole, for the next run.
func (s *Store) PurgeExpired(ctx context.Context, retentionDays int) (calls int, files int, err error) {
	if retentionDays <= 0 {
		return 0, 0, nil
	}

	ids, err := s.expiredCallIDs(ctx, time.Now().AddDate(0, 0, -retentionDays))
	if err != nil {
		return 0, 0, err
	}

	for _, id := range ids {
		paths, derr := s.DeleteCall(ctx, id)
		if derr != nil {
			log.Printf("[store] retention purge: %v (kept for the next run)", derr)

			continue
		}

		if paths != nil {
			calls++
			files += len(paths)
		}
	}

	_ = s.SetSetting(ctx, SettingLastPurgeAt,
		strconv.FormatInt(time.Now().Unix(), 10))
	_ = s.SetSetting(ctx, SettingLastPurgeCalls, strconv.Itoa(calls))
	_ = s.SetSetting(ctx, SettingLastPurgeFiles, strconv.Itoa(files))

	return calls, files, nil
}

// LastPurge returns when the purge last ran and what it removed
// (zero values when it never ran).
func (s *Store) LastPurge(ctx context.Context) (at time.Time, calls int, files int) {
	if v, ok, _ := s.GetSetting(ctx, SettingLastPurgeAt); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			at = time.Unix(n, 0).UTC()
		}
	}

	if v, ok, _ := s.GetSetting(ctx, SettingLastPurgeCalls); ok {
		calls, _ = strconv.Atoi(v)
	}

	if v, ok, _ := s.GetSetting(ctx, SettingLastPurgeFiles); ok {
		files, _ = strconv.Atoi(v)
	}

	return at, calls, files
}

// DBStats is a small SQLite health snapshot for the settings page.
type DBStats struct {
	PageCount   int64
	Freelist    int64
	PageSize    int64
	ApproxBytes int64
}

// DBStats reads page/freelist counters.
func (s *Store) DBStats(ctx context.Context) (DBStats, error) {
	var st DBStats

	for _, q := range []struct {
		query string
		dst   *int64
	}{
		{`PRAGMA page_count`, &st.PageCount},
		{`PRAGMA freelist_count`, &st.Freelist},
		{`PRAGMA page_size`, &st.PageSize},
	} {
		if err := s.db.QueryRowContext(ctx, q.query).Scan(q.dst); err != nil {
			return DBStats{}, fmt.Errorf("store: db stats %s: %w", q.query, err)
		}
	}

	st.ApproxBytes = st.PageCount * st.PageSize

	return st, nil
}

// Checkpoint truncates the WAL back into the main database file.
func (s *Store) Checkpoint(ctx context.Context) error {
	var busy, logPages, checkpointed int

	err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &logPages, &checkpointed)
	if err != nil {
		return fmt.Errorf("store: wal checkpoint: %w", err)
	}

	return nil
}

// Optimize runs SQLite's incremental query-planner maintenance.
func (s *Store) Optimize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		return fmt.Errorf("store: optimize: %w", err)
	}

	return nil
}

// VacuumIfNeeded rebuilds the database file when free pages exceed
// ratio of the total (and the file is big enough to bother).
// Returns whether a VACUUM ran.
func (s *Store) VacuumIfNeeded(ctx context.Context, ratio float64) (bool, error) {
	st, err := s.DBStats(ctx)
	if err != nil {
		return false, err
	}

	const minPages = 1000
	if st.PageCount < minPages || st.PageCount == 0 {
		return false, nil
	}

	if float64(st.Freelist)/float64(st.PageCount) <= ratio {
		return false, nil
	}

	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return false, fmt.Errorf("store: vacuum: %w", err)
	}

	return true, nil
}
