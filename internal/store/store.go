// Package store is siprec-receiver's SQLite persistence: one row per
// recorded (or AMD-notified) call, one row per recorded leg (WAV file),
// and the web UI's user allowlist.
//
// A call row can be born two ways and the two must merge into one:
//
//   - Recording-first: the SRS finalizes a SIPREC session (keyed by the
//     SIPREC dialog's SIP Call-ID, optionally carrying a CallSid from
//     the INVITE Request-URI's vm-callsid parameter and the FreeSWITCH
//     parent-call UUID decoded from the RS-Metadata session_id).
//   - AMD-first: the call-notification endpoint logs a disposition
//     (keyed by CallSid, optionally carrying the SIP Call-ID or the
//     FreeSWITCH UUID) before the recording ends — or when no recording
//     ever arrives.
//
// RecordLeg and ApplyAMD each look for a counterpart row by every key
// they carry and merge instead of duplicating.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

// Call is one row of the calls table: a recorded call, an AMD-notified
// call, or both merged.
type Call struct {
	ID            int64
	SipCallID     string
	CallSid       string
	FsUUID        string
	AccountSid    string
	FromURI       string
	FromNumber    string
	ToNumber      string
	Direction     string
	StartedAt     time.Time
	EndedAt       time.Time
	DurationSec   int
	Status        string // "completed" (has recording) or "notified" (AMD only)
	AMDAnsweredBy string
	AMDDurationMs int
	AMDCallStatus string
	AMDNotifiedAt time.Time
	AMDRaw        string
	// Final call disposition (StatusCallback-shaped; see ApplyDisposition).
	DispStatus       string
	DispDurationSec  int
	DispSipCode      int
	DispErrorCode    string
	DispErrorMessage string
	DispNotifiedAt   time.Time
	DispRaw          string
	// Full call record fetched on request from VoiceML.
	VoiceMLRecordJSON string
	VoiceMLFetchedAt  time.Time
	MetadataJSON      string
	MetadataXML       string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	LegCount          int
	HasWriteError     bool  // any leg carries a WAV write error
	Legs              []Leg // populated by GetCall
}

// Leg is one recorded audio stream of a call (one WAV file). mod_siprec
// mixed mode produces one leg; separate-streams mode produces two.
type Leg struct {
	ID             int64
	CallID         int64
	Label          string
	Codec          string
	ParticipantAOR string
	WAVPath        string
	DurationSec    int
	WriteError     string // first WAV write error; "" when the leg recorded cleanly
	CreatedAt      time.Time
}

// User is one allowlisted web user. The bootstrap administrator from
// the ADMIN_EMAIL environment variable is not stored here; it always
// has admin role (see internal/auth).
type User struct {
	Email        string
	Role         string // "admin" or "user"
	CreatedBy    string
	CreatedAt    time.Time
	Disabled     bool
	GoogleSub    string // pinned Google account ID (sub claim); "" until first sign-in
	SessionEpoch int    // bump to revoke every outstanding session
}

// AMDNotification is one call-notification (answering-machine
// disposition) as accepted by the notify endpoint. Field names follow
// VoiceML's AMD webhook payload (internal/api AMDCallbackPayload:
// CallSid, AccountSid, AnsweredBy, MachineDetectionDuration, From, To,
// CallStatus, Direction), plus the SIPREC correlation keys SipCallId
// and FsUuid a poster that knows them can add.
type AMDNotification struct {
	CallSid    string
	AccountSid string
	AnsweredBy string
	DurationMs int
	From       string
	To         string
	CallStatus string
	Direction  string
	SipCallID  string
	FsUUID     string
	Raw        string // request body as received, for audit display
}

// ErrNoKey is returned by ApplyAMD when a notification carries no
// correlation key at all (no CallSid, SipCallId, or FsUuid).
var ErrNoKey = errors.New("amd notification has no correlation key (CallSid, SipCallId, or FsUuid)")

// Store wraps the SQLite handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and runs
// migrations. WAL mode so the SIP finalizer, the notify endpoint, and
// the web UI can write/read concurrently.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	db.SetMaxOpenConns(1) // SQLite serializes writers; one conn avoids lock churn.

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()

		return nil, err
	}

	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// migrateLegacyNames renames the pre-VoiceML-rename schema objects
// (cb_accounts table, calls.cb_record_json / cb_fetched_at columns)
// on databases created by earlier builds. Guarded everywhere, so it
// is a no-op on fresh databases. Runs before the schema DDL.
func (s *Store) migrateLegacyNames(ctx context.Context) error {
	tableExists := func(name string) (bool, error) {
		var n int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`,
			name).Scan(&n)

		return n > 0, err
	}

	cbTable, err := tableExists("cb_accounts")
	if err != nil {
		return fmt.Errorf("store: legacy table check: %w", err)
	}

	vmTable, err := tableExists("voiceml_accounts")
	if err != nil {
		return fmt.Errorf("store: legacy table check: %w", err)
	}

	switch {
	case cbTable && !vmTable:
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE cb_accounts RENAME TO voiceml_accounts`); err != nil {
			return fmt.Errorf("store: rename cb_accounts: %w", err)
		}
	case cbTable && vmTable:
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO voiceml_accounts SELECT * FROM cb_accounts;
			 DROP TABLE cb_accounts;`); err != nil {
			return fmt.Errorf("store: merge cb_accounts: %w", err)
		}
	}

	for _, rename := range [][2]string{
		{"cb_record_json", "voiceml_record_json"},
		{"cb_fetched_at", "voiceml_fetched_at"},
	} {
		oldCol, newCol := rename[0], rename[1]

		hasOld, err := s.columnExists(ctx, "calls", oldCol)
		if err != nil || !hasOld {
			continue
		}

		hasNew, err := s.columnExists(ctx, "calls", newCol)
		if err != nil {
			return err
		}

		if !hasNew {
			if _, err := s.db.ExecContext(ctx,
				`ALTER TABLE calls RENAME COLUMN `+oldCol+` TO `+newCol); err != nil {
				return fmt.Errorf("store: rename column %s: %w", oldCol, err)
			}
		}
	}

	return nil
}

// columnExists reports whether table has column (PRAGMA table_info).
func (s *Store) columnExists(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, fmt.Errorf("store: pragma %s: %w", table, err)
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid       int
			name, typ string
			notNull   int
			dflt, pk  any
		)

		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("store: pragma scan: %w", err)
		}

		if name == column {
			return true, nil
		}
	}

	return false, rows.Err()
}

func (s *Store) migrate(ctx context.Context) error {
	if err := s.migrateLegacyNames(ctx); err != nil {
		return err
	}

	const schema = `
CREATE TABLE IF NOT EXISTS calls (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	sip_call_id     TEXT UNIQUE,
	call_sid        TEXT UNIQUE,
	fs_uuid         TEXT,
	account_sid     TEXT NOT NULL DEFAULT '',
	from_uri        TEXT NOT NULL DEFAULT '',
	from_number     TEXT NOT NULL DEFAULT '',
	to_number       TEXT NOT NULL DEFAULT '',
	direction       TEXT NOT NULL DEFAULT '',
	started_at      INTEGER NOT NULL DEFAULT 0,
	ended_at        INTEGER NOT NULL DEFAULT 0,
	duration_sec    INTEGER NOT NULL DEFAULT 0,
	status          TEXT NOT NULL DEFAULT 'notified',
	amd_answered_by TEXT NOT NULL DEFAULT '',
	amd_duration_ms INTEGER NOT NULL DEFAULT 0,
	amd_call_status TEXT NOT NULL DEFAULT '',
	amd_notified_at INTEGER NOT NULL DEFAULT 0,
	amd_raw         TEXT NOT NULL DEFAULT '',
	metadata_json   TEXT NOT NULL DEFAULT '',
	metadata_xml    TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_calls_fs_uuid ON calls(fs_uuid);
CREATE INDEX IF NOT EXISTS idx_calls_started ON calls(started_at DESC);
CREATE TABLE IF NOT EXISTS legs (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	call_id         INTEGER NOT NULL REFERENCES calls(id) ON DELETE CASCADE,
	label           TEXT NOT NULL,
	codec           TEXT NOT NULL DEFAULT '',
	participant_aor TEXT NOT NULL DEFAULT '',
	wav_path        TEXT NOT NULL,
	duration_sec    INTEGER NOT NULL DEFAULT 0,
	write_error     TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	UNIQUE(call_id, wav_path)
);
CREATE TABLE IF NOT EXISTS users (
	email         TEXT PRIMARY KEY,
	role          TEXT NOT NULL DEFAULT 'user',
	created_by    TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	disabled      INTEGER NOT NULL DEFAULT 0,
	google_sub    TEXT NOT NULL DEFAULT '',
	session_epoch INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS voiceml_accounts (
	account_sid TEXT PRIMARY KEY,
	api_key     TEXT NOT NULL DEFAULT '',
	created_by  TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_log (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	actor      TEXT NOT NULL,
	action     TEXT NOT NULL,
	target     TEXT NOT NULL DEFAULT '',
	detail     TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at DESC);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}

	// Columns added after v0.1.0 for databases that already exist.
	for _, col := range []struct{ name, ddl string }{
		{"write_error", `ALTER TABLE legs ADD COLUMN write_error TEXT NOT NULL DEFAULT ''`},
	} {
		if err := s.addColumnIfMissing(ctx, "legs", col.name, col.ddl); err != nil {
			return err
		}
	}

	for _, col := range []struct{ name, ddl string }{
		{"google_sub", `ALTER TABLE users ADD COLUMN google_sub TEXT NOT NULL DEFAULT ''`},
		{"session_epoch", `ALTER TABLE users ADD COLUMN session_epoch INTEGER NOT NULL DEFAULT 0`},
	} {
		if err := s.addColumnIfMissing(ctx, "users", col.name, col.ddl); err != nil {
			return err
		}
	}

	for _, col := range []struct{ name, ddl string }{
		{"disp_status", `ALTER TABLE calls ADD COLUMN disp_status TEXT NOT NULL DEFAULT ''`},
		{"disp_duration_sec", `ALTER TABLE calls ADD COLUMN disp_duration_sec INTEGER NOT NULL DEFAULT 0`},
		{"disp_sip_code", `ALTER TABLE calls ADD COLUMN disp_sip_code INTEGER NOT NULL DEFAULT 0`},
		{"disp_error_code", `ALTER TABLE calls ADD COLUMN disp_error_code TEXT NOT NULL DEFAULT ''`},
		{"disp_error_message", `ALTER TABLE calls ADD COLUMN disp_error_message TEXT NOT NULL DEFAULT ''`},
		{"disp_notified_at", `ALTER TABLE calls ADD COLUMN disp_notified_at INTEGER NOT NULL DEFAULT 0`},
		{"disp_raw", `ALTER TABLE calls ADD COLUMN disp_raw TEXT NOT NULL DEFAULT ''`},
		{"voiceml_record_json", `ALTER TABLE calls ADD COLUMN voiceml_record_json TEXT NOT NULL DEFAULT ''`},
		{"voiceml_fetched_at", `ALTER TABLE calls ADD COLUMN voiceml_fetched_at INTEGER NOT NULL DEFAULT 0`},
	} {
		if err := s.addColumnIfMissing(ctx, "calls", col.name, col.ddl); err != nil {
			return err
		}
	}

	// Schema version stamp. Migrations above are idempotent
	// (IF NOT EXISTS + addColumnIfMissing), so existing databases
	// converge regardless of the version they started at; the stamp
	// exists so a future non-idempotent migration has a baseline to
	// gate on.
	if _, err := s.db.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
		return fmt.Errorf("store: stamp user_version: %w", err)
	}

	return nil
}

// addColumnIfMissing runs ddl when table lacks column (SQLite has no
// ADD COLUMN IF NOT EXISTS).
func (s *Store) addColumnIfMissing(ctx context.Context, table, column, ddl string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return fmt.Errorf("store: pragma %s: %w", table, err)
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid       int
			name, typ string
			notNull   int
			dflt, pk  any
		)

		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("store: pragma scan: %w", err)
		}

		if name == column {
			return nil
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: pragma rows: %w", err)
	}

	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("store: add column %s.%s: %w", table, column, err)
	}

	return nil
}

func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}

	return t.UnixMilli()
}

func fromMillis(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}

	return time.UnixMilli(v).UTC()
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}

	return v
}

// escapeLike escapes LIKE wildcards in user search text (queries use
// ESCAPE '\'), so "%" and "_" match literally instead of acting as
// wildcards.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// RecordLeg persists one finalized recording leg, creating or updating
// the parent call row (keyed by the SIPREC dialog Call-ID) and merging
// any AMD-first placeholder row that shares the CallSid or FsUUID.
// Called once per leg by the SRS Finalizer; safe to call for every leg
// of a multi-leg call.
func (s *Store) RecordLeg(ctx context.Context, leg siprecsrv.FinalizedLeg) error {
	now := time.Now().UTC()

	metaJSON := ""
	fromNumber, toNumber := "", ""

	if leg.Metadata != nil {
		if b, err := json.Marshal(leg.Metadata); err == nil {
			metaJSON = string(b)
		}

		aors := leg.Metadata.ParticipantAORs()
		if len(aors) > 0 {
			fromNumber = aorUser(aors[0])
		}

		if len(aors) > 1 {
			toNumber = aorUser(aors[1])
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: record leg begin: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	// If a DIFFERENT dialog's row already owns this CallSid (a second
	// SIPREC session for the same call — re-record after a transfer,
	// an SRC restart), this session must not claim or collide with
	// that row: record it under its own SIP Call-ID without the
	// CallSid key. The first dialog keeps the CallSid correlation.
	insertCallSid := leg.CallSid

	if leg.CallSid != "" {
		var ownerSipID string

		oerr := tx.QueryRowContext(ctx,
			`SELECT COALESCE(sip_call_id, '') FROM calls WHERE call_sid = ?`,
			leg.CallSid).Scan(&ownerSipID)
		switch {
		case oerr == nil && ownerSipID != "" && ownerSipID != leg.CallID:
			insertCallSid = ""
		case oerr != nil && !errors.Is(oerr, sql.ErrNoRows):
			return fmt.Errorf("store: check call_sid owner: %w", oerr)
		}
	}

	// Claim any AMD-first placeholder row for this call (matched by
	// CallSid or FsUUID) by stamping it with our SIP Call-ID, so the
	// upsert below refreshes that row — AMD fields and all — instead
	// of colliding with its UNIQUE call_sid.
	if leg.CallSid != "" && insertCallSid != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE calls SET sip_call_id = ? WHERE call_sid = ? AND sip_call_id IS NULL`,
			leg.CallID, leg.CallSid); err != nil {
			return fmt.Errorf("store: claim amd placeholder by call_sid: %w", err)
		}
	}

	if leg.FsUUID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE calls SET sip_call_id = ? WHERE sip_call_id IS NULL AND id =
				(SELECT id FROM calls WHERE fs_uuid = ? AND sip_call_id IS NULL LIMIT 1)`,
			leg.CallID, leg.FsUUID); err != nil {
			return fmt.Errorf("store: claim amd placeholder by fs_uuid: %w", err)
		}
	}

	// Create or refresh the recording-owned fields. AMD-owned fields
	// are deliberately not touched here; the merge step below adopts
	// them from a placeholder row if one exists.
	_, err = tx.ExecContext(ctx, `
INSERT INTO calls (sip_call_id, call_sid, fs_uuid, account_sid, from_uri,
	from_number, to_number, started_at, ended_at, duration_sec, status,
	metadata_json, metadata_xml, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'completed', ?, ?, ?, ?)
ON CONFLICT(sip_call_id) DO UPDATE SET
	call_sid      = COALESCE(calls.call_sid, excluded.call_sid),
	fs_uuid       = CASE WHEN excluded.fs_uuid != '' THEN excluded.fs_uuid ELSE calls.fs_uuid END,
	account_sid   = CASE WHEN excluded.account_sid != '' THEN excluded.account_sid ELSE calls.account_sid END,
	from_uri      = CASE WHEN excluded.from_uri != '' THEN excluded.from_uri ELSE calls.from_uri END,
	from_number   = CASE WHEN excluded.from_number != '' THEN excluded.from_number ELSE calls.from_number END,
	to_number     = CASE WHEN excluded.to_number != '' THEN excluded.to_number ELSE calls.to_number END,
	ended_at      = excluded.ended_at,
	duration_sec  = MAX(calls.duration_sec, excluded.duration_sec),
	status        = 'completed',
	metadata_json = CASE WHEN excluded.metadata_json != '' THEN excluded.metadata_json ELSE calls.metadata_json END,
	metadata_xml  = CASE WHEN excluded.metadata_xml != '' THEN excluded.metadata_xml ELSE calls.metadata_xml END,
	updated_at    = excluded.updated_at;`,
		leg.CallID, nullIfEmpty(insertCallSid), leg.FsUUID, leg.AccountSid, leg.FromURI,
		fromNumber, toNumber, millis(leg.StartedAt), millis(now), leg.DurationSec,
		metaJSON, leg.MetadataXML, millis(now), millis(now))
	if err != nil {
		return fmt.Errorf("store: upsert call %s: %w", leg.CallID, err)
	}

	var callPK int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM calls WHERE sip_call_id = ?`, leg.CallID).Scan(&callPK); err != nil {
		return fmt.Errorf("store: lookup call %s: %w", leg.CallID, err)
	}

	// Adopt AMD data from a placeholder row created by an earlier
	// notification (matched by CallSid or FsUUID), then remove it.
	if err := mergePlaceholder(ctx, tx, callPK, leg.CallSid, leg.FsUUID); err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO legs (call_id, label, codec, participant_aor, wav_path, duration_sec, write_error, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(call_id, wav_path) DO UPDATE SET
	label = excluded.label, codec = excluded.codec,
	participant_aor = excluded.participant_aor, duration_sec = excluded.duration_sec,
	write_error = excluded.write_error;`,
		callPK, leg.Label, leg.Codec, participantForLabel(leg.Metadata, leg.Label),
		leg.WAVPath, leg.DurationSec, leg.WriteErr, millis(now))
	if err != nil {
		return fmt.Errorf("store: insert leg %s: %w", leg.Label, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: record leg commit: %w", err)
	}

	return nil
}

// mergePlaceholder folds an AMD-first placeholder row (a different row
// sharing callSid or fsUUID) into the recording row callPK and deletes
// the placeholder. No-op when there is none.
func mergePlaceholder(ctx context.Context, tx *sql.Tx, callPK int64, callSid, fsUUID string) error {
	var otherID int64

	var amdBy, amdStatus, amdRaw string

	var amdDurMs int

	var amdAt, startedAt int64

	find := func(query string, arg string) error {
		return tx.QueryRowContext(ctx, query, arg, callPK).Scan(
			&otherID, &amdBy, &amdDurMs, &amdStatus, &amdRaw, &amdAt, &startedAt)
	}

	err := sql.ErrNoRows
	if callSid != "" {
		err = find(`SELECT id, amd_answered_by, amd_duration_ms, amd_call_status, amd_raw,
			amd_notified_at, started_at FROM calls
			WHERE call_sid = ? AND id != ? AND sip_call_id IS NULL ORDER BY started_at DESC LIMIT 1`, callSid)
	}

	if errors.Is(err, sql.ErrNoRows) && fsUUID != "" {
		err = find(`SELECT id, amd_answered_by, amd_duration_ms, amd_call_status, amd_raw,
			amd_notified_at, started_at FROM calls
			WHERE fs_uuid = ? AND id != ? AND sip_call_id IS NULL ORDER BY started_at DESC LIMIT 1`, fsUUID)
	}

	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("store: find amd placeholder: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE calls SET amd_answered_by = ?, amd_duration_ms = ?, amd_call_status = ?,
	amd_raw = ?, amd_notified_at = ?, updated_at = ?
WHERE id = ? AND amd_answered_by = '';`,
		amdBy, amdDurMs, amdStatus, amdRaw, amdAt, millis(time.Now().UTC()), callPK); err != nil {
		return fmt.Errorf("store: adopt amd placeholder: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM calls WHERE id = ?`, otherID); err != nil {
		return fmt.Errorf("store: delete amd placeholder: %w", err)
	}

	return nil
}

// ApplyAMD logs one AMD disposition, updating the matching call row
// (by CallSid, then SIP Call-ID, then FsUUID) or inserting an
// AMD-only placeholder row. Returns the row id and whether an existing
// row was matched.
func (s *Store) ApplyAMD(ctx context.Context, n AMDNotification) (int64, bool, error) {
	if n.CallSid == "" && n.SipCallID == "" && n.FsUUID == "" {
		return 0, false, ErrNoKey
	}

	now := time.Now().UTC()

	// One transaction: the store's single connection serializes
	// writers, so lookup + write cannot interleave with a concurrent
	// notification for the same new key and trip the UNIQUE indexes.
	tx, terr := s.db.BeginTx(ctx, nil)
	if terr != nil {
		return 0, false, fmt.Errorf("store: amd begin: %w", terr)
	}

	defer func() { _ = tx.Rollback() }()

	var id int64

	find := func(query string, arg string) error {
		return tx.QueryRowContext(ctx, query, arg).Scan(&id)
	}

	err := sql.ErrNoRows
	if n.CallSid != "" {
		err = find(`SELECT id FROM calls WHERE call_sid = ?`, n.CallSid)
	}

	if errors.Is(err, sql.ErrNoRows) && n.SipCallID != "" {
		err = find(`SELECT id FROM calls WHERE sip_call_id = ?`, n.SipCallID)
	}

	if errors.Is(err, sql.ErrNoRows) && n.FsUUID != "" {
		err = find(`SELECT id FROM calls WHERE fs_uuid = ? ORDER BY started_at DESC LIMIT 1`, n.FsUUID)
	}

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("store: amd lookup: %w", err)
	}

	if err == nil {
		// Never steal a CallSid another row owns, and never stamp a
		// SIP Call-ID onto a row whose CallSid names a different call.
		effCallSid := n.CallSid
		if effCallSid != "" {
			var ownerID int64
			if oerr := tx.QueryRowContext(ctx,
				`SELECT id FROM calls WHERE call_sid = ?`, effCallSid).Scan(&ownerID); oerr == nil && ownerID != id {
				effCallSid = ""
			}
		}

		_, uerr := tx.ExecContext(ctx, `
UPDATE calls SET
	amd_answered_by = ?, amd_duration_ms = ?, amd_call_status = ?,
	amd_raw = ?, amd_notified_at = ?,
	call_sid    = COALESCE(call_sid, ?),
	sip_call_id = CASE WHEN COALESCE(call_sid, '') = '' OR call_sid = ?
		THEN COALESCE(sip_call_id, ?) ELSE sip_call_id END,
	fs_uuid     = CASE WHEN fs_uuid = '' THEN ? ELSE fs_uuid END,
	account_sid = CASE WHEN account_sid = '' THEN ? ELSE account_sid END,
	from_number = CASE WHEN from_number = '' THEN ? ELSE from_number END,
	to_number   = CASE WHEN to_number = '' THEN ? ELSE to_number END,
	direction   = CASE WHEN direction = '' THEN ? ELSE direction END,
	updated_at  = ?
WHERE id = ?;`,
			n.AnsweredBy, n.DurationMs, n.CallStatus, n.Raw, millis(now),
			nullIfEmpty(effCallSid), n.CallSid, nullIfEmpty(n.SipCallID), n.FsUUID,
			n.AccountSid, n.From, n.To, n.Direction, millis(now), id)
		if uerr != nil {
			return 0, false, fmt.Errorf("store: amd update %d: %w", id, uerr)
		}

		if cerr := tx.Commit(); cerr != nil {
			return 0, false, fmt.Errorf("store: amd commit: %w", cerr)
		}

		return id, true, nil
	}

	res, ierr := tx.ExecContext(ctx, `
INSERT INTO calls (sip_call_id, call_sid, fs_uuid, account_sid, from_number,
	to_number, direction, started_at, status, amd_answered_by, amd_duration_ms,
	amd_call_status, amd_notified_at, amd_raw, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'notified', ?, ?, ?, ?, ?, ?, ?);`,
		nullIfEmpty(n.SipCallID), nullIfEmpty(n.CallSid), n.FsUUID, n.AccountSid,
		n.From, n.To, n.Direction, millis(now), n.AnsweredBy, n.DurationMs,
		n.CallStatus, millis(now), n.Raw, millis(now), millis(now))
	if ierr != nil {
		return 0, false, fmt.Errorf("store: amd insert: %w", ierr)
	}

	newID, ierr := res.LastInsertId()
	if ierr != nil {
		return 0, false, fmt.Errorf("store: amd insert id: %w", ierr)
	}

	if cerr := tx.Commit(); cerr != nil {
		return 0, false, fmt.Errorf("store: amd commit: %w", cerr)
	}

	return newID, false, nil
}

// ListFilter narrows ListCalls. Empty fields match everything.
// StartedBefore is exclusive; StartedAfter is inclusive.
type ListFilter struct {
	Query         string // substring match on numbers / SIDs / UUID / dispositions
	From          string // substring match on from_number
	To            string // substring match on to_number
	AMD           string // exact amd_answered_by match when non-empty
	Disposition   string // exact disp_status match when non-empty
	StartedAfter  time.Time
	StartedBefore time.Time
	Limit         int
	Offset        int
}

// ListCalls returns calls newest-first plus the total match count.
func (s *Store) ListCalls(ctx context.Context, f ListFilter) ([]Call, int, error) {
	where := `WHERE 1=1`
	args := []any{}

	if f.Query != "" {
		where += ` AND (from_number LIKE ? ESCAPE '\' OR to_number LIKE ? ESCAPE '\'
			OR call_sid LIKE ? ESCAPE '\' OR sip_call_id LIKE ? ESCAPE '\'
			OR fs_uuid LIKE ? ESCAPE '\' OR account_sid LIKE ? ESCAPE '\'
			OR amd_answered_by LIKE ? ESCAPE '\' OR disp_status LIKE ? ESCAPE '\')`
		like := "%" + escapeLike(f.Query) + "%"
		args = append(args, like, like, like, like, like, like, like, like)
	}

	if f.From != "" {
		where += ` AND from_number LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(f.From)+"%")
	}

	if f.To != "" {
		where += ` AND to_number LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(f.To)+"%")
	}

	if f.AMD != "" {
		where += ` AND amd_answered_by = ?`
		args = append(args, f.AMD)
	}

	if f.Disposition != "" {
		where += ` AND disp_status = ?`
		args = append(args, f.Disposition)
	}

	if !f.StartedAfter.IsZero() {
		where += ` AND started_at >= ?`
		args = append(args, millis(f.StartedAfter))
	}

	if !f.StartedBefore.IsZero() {
		where += ` AND started_at < ?`
		args = append(args, millis(f.StartedBefore))
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM calls `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count calls: %w", err)
	}

	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := s.db.QueryContext(ctx, callCols+` FROM calls `+where+
		` ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list calls: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []Call

	for rows.Next() {
		c, err := scanCall(rows)
		if err != nil {
			return nil, 0, err
		}

		out = append(out, c)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: list calls rows: %w", err)
	}

	return out, total, nil
}

const callCols = `SELECT id, COALESCE(sip_call_id,''), COALESCE(call_sid,''), COALESCE(fs_uuid,''),
	account_sid, from_uri, from_number, to_number, direction, started_at, ended_at,
	duration_sec, status, amd_answered_by, amd_duration_ms, amd_call_status,
	amd_notified_at, amd_raw, metadata_json, metadata_xml,
	disp_status, disp_duration_sec, disp_sip_code, disp_error_code,
	disp_error_message, disp_notified_at, disp_raw, voiceml_record_json,
	voiceml_fetched_at, created_at, updated_at,
	(SELECT COUNT(*) FROM legs WHERE legs.call_id = calls.id),
	(SELECT COUNT(*) FROM legs WHERE legs.call_id = calls.id AND legs.write_error != '')`

func scanCall(src interface{ Scan(...any) error }) (Call, error) {
	var c Call

	var started, ended, amdAt, dispAt, vmAt, created, updated int64

	var writeErrLegs int

	if err := src.Scan(&c.ID, &c.SipCallID, &c.CallSid, &c.FsUUID, &c.AccountSid,
		&c.FromURI, &c.FromNumber, &c.ToNumber, &c.Direction, &started, &ended,
		&c.DurationSec, &c.Status, &c.AMDAnsweredBy, &c.AMDDurationMs, &c.AMDCallStatus,
		&amdAt, &c.AMDRaw, &c.MetadataJSON, &c.MetadataXML,
		&c.DispStatus, &c.DispDurationSec, &c.DispSipCode, &c.DispErrorCode,
		&c.DispErrorMessage, &dispAt, &c.DispRaw, &c.VoiceMLRecordJSON, &vmAt,
		&created, &updated, &c.LegCount, &writeErrLegs); err != nil {
		return Call{}, fmt.Errorf("store: scan call: %w", err)
	}

	c.HasWriteError = writeErrLegs > 0

	c.StartedAt, c.EndedAt, c.AMDNotifiedAt = fromMillis(started), fromMillis(ended), fromMillis(amdAt)
	c.DispNotifiedAt, c.VoiceMLFetchedAt = fromMillis(dispAt), fromMillis(vmAt)
	c.CreatedAt, c.UpdatedAt = fromMillis(created), fromMillis(updated)

	return c, nil
}

// GetCall returns one call with its legs, or (nil, nil) when absent.
func (s *Store) GetCall(ctx context.Context, id int64) (*Call, error) {
	c, err := scanCall(s.db.QueryRowContext(ctx, callCols+` FROM calls WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, call_id, label, codec, participant_aor, wav_path, duration_sec, write_error, created_at
		 FROM legs WHERE call_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("store: list legs %d: %w", id, err)
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var l Leg

		var created int64

		if err := rows.Scan(&l.ID, &l.CallID, &l.Label, &l.Codec, &l.ParticipantAOR,
			&l.WAVPath, &l.DurationSec, &l.WriteError, &created); err != nil {
			return nil, fmt.Errorf("store: scan leg: %w", err)
		}

		l.CreatedAt = fromMillis(created)
		c.Legs = append(c.Legs, l)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: legs rows: %w", err)
	}

	return &c, nil
}

// GetLeg returns one leg row, or (nil, nil) when absent.
func (s *Store) GetLeg(ctx context.Context, id int64) (*Leg, error) {
	var l Leg

	var created int64

	err := s.db.QueryRowContext(ctx,
		`SELECT id, call_id, label, codec, participant_aor, wav_path, duration_sec, write_error, created_at
		 FROM legs WHERE id = ?`, id).
		Scan(&l.ID, &l.CallID, &l.Label, &l.Codec, &l.ParticipantAOR, &l.WAVPath, &l.DurationSec, &l.WriteError, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("store: get leg %d: %w", id, err)
	}

	l.CreatedAt = fromMillis(created)

	return &l, nil
}

// DeleteCall removes a call and its legs, returning the legs' WAV
// paths so the caller can remove the files.
func (s *Store) DeleteCall(ctx context.Context, id int64) ([]string, error) {
	call, err := s.GetCall(ctx, id)
	if err != nil {
		return nil, err
	}

	if call == nil {
		return nil, nil
	}

	paths := make([]string, 0, len(call.Legs))
	for _, l := range call.Legs {
		paths = append(paths, l.WAVPath)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM calls WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("store: delete call %d: %w", id, err)
	}

	return paths, nil
}

// --- Users ---------------------------------------------------------------

// NormalizeEmail lower-cases and trims an email for storage/lookup.
// Google accounts are case-insensitive in the local part in practice,
// and the allowlist must match however the admin typed it.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// UpsertUser adds a user or updates an existing user's role/disabled.
func (s *Store) UpsertUser(ctx context.Context, u User) error {
	role := u.Role
	if role != "admin" {
		role = "user"
	}

	_, err := s.db.ExecContext(ctx, `
INSERT INTO users (email, role, created_by, created_at, disabled, google_sub, session_epoch)
VALUES (?, ?, ?, ?, ?, '', 0)
ON CONFLICT(email) DO UPDATE SET role = excluded.role, disabled = excluded.disabled;`,
		NormalizeEmail(u.Email), role, u.CreatedBy, millis(u.CreatedAt), boolInt(u.Disabled))
	if err != nil {
		return fmt.Errorf("store: upsert user: %w", err)
	}

	return nil
}

// GetUser returns the user row for email, or (nil, nil) when absent.
func (s *Store) GetUser(ctx context.Context, email string) (*User, error) {
	var u User

	var created int64

	var disabled int

	err := s.db.QueryRowContext(ctx,
		`SELECT email, role, created_by, created_at, disabled, google_sub, session_epoch FROM users WHERE email = ?`,
		NormalizeEmail(email)).Scan(&u.Email, &u.Role, &u.CreatedBy, &created, &disabled,
		&u.GoogleSub, &u.SessionEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("store: get user: %w", err)
	}

	u.CreatedAt, u.Disabled = fromMillis(created), disabled != 0

	return &u, nil
}

// ListUsers returns every user, ordered by email.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT email, role, created_by, created_at, disabled, google_sub, session_epoch FROM users ORDER BY email`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []User

	for rows.Next() {
		var u User

		var created int64

		var disabled int

		if err := rows.Scan(&u.Email, &u.Role, &u.CreatedBy, &created, &disabled,
			&u.GoogleSub, &u.SessionEpoch); err != nil {
			return nil, fmt.Errorf("store: scan user: %w", err)
		}

		u.CreatedAt, u.Disabled = fromMillis(created), disabled != 0
		out = append(out, u)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list users rows: %w", err)
	}

	return out, nil
}

// DeleteUser removes a user row. Returns whether a row existed.
func (s *Store) DeleteUser(ctx context.Context, email string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE email = ?`, NormalizeEmail(email))
	if err != nil {
		return false, fmt.Errorf("store: delete user: %w", err)
	}

	n, err := res.RowsAffected()

	return n > 0, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}

	return 0
}

// aorUser extracts the user part of a SIP AOR for display:
// "sip:+12165285911@host" -> "+12165285911", "sip:alice@host" -> "alice".
func aorUser(aor string) string {
	aor = strings.TrimSpace(aor)
	if i := strings.Index(aor, ":"); i >= 0 && !strings.Contains(aor[:i], "@") {
		aor = aor[i+1:]
	}

	if at := strings.Index(aor, "@"); at >= 0 {
		return aor[:at]
	}

	return aor
}

// participantForLabel finds the participant AOR whose user part equals
// the (already sanitised) leg label the SRS resolved. Best-effort
// display metadata; "" when the label was positional.
func participantForLabel(rec *siprecsrv.Recording, label string) string {
	if rec == nil {
		return ""
	}

	for _, aor := range rec.ParticipantAORs() {
		if aorUser(aor) == label || strings.TrimPrefix(aorUser(aor), "+") == label {
			return aor
		}
	}

	return ""
}

// DispositionNotification is one final call-outcome notification as
// accepted by POST /api/notify/disposition. Field names follow
// VoiceML's StatusCallback payload (CallSid, CallStatus,
// CallDuration, SipResponseCode, ErrorCode, …).
type DispositionNotification struct {
	CallSid      string
	AccountSid   string
	CallStatus   string
	DurationSec  int
	SipCode      int
	ErrorCode    string
	ErrorMessage string
	AnsweredBy   string // terminal callbacks carry the AMD verdict too
	From         string
	To           string
	Direction    string
	SipCallID    string
	FsUUID       string
	Raw          string
}

// ApplyDisposition logs one final call outcome, with the same
// match-or-placeholder upsert semantics as ApplyAMD. A disposition
// that carries AnsweredBy also fills the AMD fields when they are
// still empty (the terminal StatusCallback repeats the verdict).
func (s *Store) ApplyDisposition(ctx context.Context, n DispositionNotification) (int64, bool, error) {
	if n.CallSid == "" && n.SipCallID == "" && n.FsUUID == "" {
		return 0, false, ErrNoKey
	}

	now := time.Now().UTC()

	tx, terr := s.db.BeginTx(ctx, nil)
	if terr != nil {
		return 0, false, fmt.Errorf("store: disposition begin: %w", terr)
	}

	defer func() { _ = tx.Rollback() }()

	var id int64

	find := func(query string, arg string) error {
		return tx.QueryRowContext(ctx, query, arg).Scan(&id)
	}

	err := sql.ErrNoRows
	if n.CallSid != "" {
		err = find(`SELECT id FROM calls WHERE call_sid = ?`, n.CallSid)
	}

	if errors.Is(err, sql.ErrNoRows) && n.SipCallID != "" {
		err = find(`SELECT id FROM calls WHERE sip_call_id = ?`, n.SipCallID)
	}

	if errors.Is(err, sql.ErrNoRows) && n.FsUUID != "" {
		err = find(`SELECT id FROM calls WHERE fs_uuid = ? ORDER BY started_at DESC LIMIT 1`, n.FsUUID)
	}

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("store: disposition lookup: %w", err)
	}

	if err == nil {
		effCallSid := n.CallSid
		if effCallSid != "" {
			var ownerID int64
			if oerr := tx.QueryRowContext(ctx,
				`SELECT id FROM calls WHERE call_sid = ?`, effCallSid).Scan(&ownerID); oerr == nil && ownerID != id {
				effCallSid = ""
			}
		}

		_, uerr := tx.ExecContext(ctx, `
UPDATE calls SET
	disp_status = ?, disp_duration_sec = ?, disp_sip_code = ?,
	disp_error_code = ?, disp_error_message = ?, disp_raw = ?,
	disp_notified_at = ?,
	amd_answered_by = CASE WHEN amd_answered_by = '' THEN ? ELSE amd_answered_by END,
	amd_call_status = CASE WHEN amd_call_status = '' THEN ? ELSE amd_call_status END,
	amd_notified_at = CASE WHEN amd_notified_at = 0 AND ? != '' THEN ? ELSE amd_notified_at END,
	call_sid    = COALESCE(call_sid, ?),
	sip_call_id = CASE WHEN COALESCE(call_sid, '') = '' OR call_sid = ?
		THEN COALESCE(sip_call_id, ?) ELSE sip_call_id END,
	fs_uuid     = CASE WHEN fs_uuid = '' THEN ? ELSE fs_uuid END,
	account_sid = CASE WHEN account_sid = '' THEN ? ELSE account_sid END,
	from_number = CASE WHEN from_number = '' THEN ? ELSE from_number END,
	to_number   = CASE WHEN to_number = '' THEN ? ELSE to_number END,
	direction   = CASE WHEN direction = '' THEN ? ELSE direction END,
	updated_at  = ?
WHERE id = ?;`,
			n.CallStatus, n.DurationSec, n.SipCode, n.ErrorCode, n.ErrorMessage,
			n.Raw, millis(now), n.AnsweredBy, n.CallStatus, n.AnsweredBy,
			millis(now), nullIfEmpty(effCallSid), n.CallSid, nullIfEmpty(n.SipCallID),
			n.FsUUID, n.AccountSid, n.From, n.To, n.Direction, millis(now), id)
		if uerr != nil {
			return 0, false, fmt.Errorf("store: disposition update %d: %w", id, uerr)
		}

		if cerr := tx.Commit(); cerr != nil {
			return 0, false, fmt.Errorf("store: disposition commit: %w", cerr)
		}

		return id, true, nil
	}

	amdAt := int64(0)
	if n.AnsweredBy != "" {
		amdAt = millis(now)
	}

	res, ierr := tx.ExecContext(ctx, `
INSERT INTO calls (sip_call_id, call_sid, fs_uuid, account_sid, from_number,
	to_number, direction, started_at, status, disp_status, disp_duration_sec,
	disp_sip_code, disp_error_code, disp_error_message, disp_notified_at,
	disp_raw, amd_answered_by, amd_call_status, amd_notified_at,
	created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'notified', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`,
		nullIfEmpty(n.SipCallID), nullIfEmpty(n.CallSid), n.FsUUID, n.AccountSid,
		n.From, n.To, n.Direction, millis(now), n.CallStatus, n.DurationSec,
		n.SipCode, n.ErrorCode, n.ErrorMessage, millis(now), n.Raw,
		n.AnsweredBy, n.CallStatus, amdAt, millis(now), millis(now))
	if ierr != nil {
		return 0, false, fmt.Errorf("store: disposition insert: %w", ierr)
	}

	newID, ierr := res.LastInsertId()
	if ierr != nil {
		return 0, false, fmt.Errorf("store: disposition insert id: %w", ierr)
	}

	if cerr := tx.Commit(); cerr != nil {
		return 0, false, fmt.Errorf("store: disposition commit: %w", cerr)
	}

	return newID, false, nil
}

// VoiceMLRecord is the subset of VoiceML's Call resource the store
// denormalizes when a fetched record is saved. Duration arrives as a
// string in Twilio-shaped JSON ("duration": "12").
type VoiceMLRecord struct {
	Sid         string
	AccountSid  string
	From        string
	To          string
	Status      string
	Direction   string
	AnsweredBy  string
	DurationSec int
}

// SaveVoiceMLRecord stores a Call record fetched from VoiceML
// (pretty-printed JSON verbatim) and fills still-empty identity /
// disposition fields from it. The recording-owned fields are never
// overwritten.
func (s *Store) SaveVoiceMLRecord(ctx context.Context, callPK int64, rec VoiceMLRecord, prettyJSON string) error {
	now := time.Now().UTC()

	_, err := s.db.ExecContext(ctx, `
UPDATE calls SET
	voiceml_record_json = ?, voiceml_fetched_at = ?,
	account_sid = CASE WHEN account_sid = '' THEN ? ELSE account_sid END,
	call_sid    = COALESCE(call_sid, ?),
	from_number = CASE WHEN from_number = '' THEN ? ELSE from_number END,
	to_number   = CASE WHEN to_number = '' THEN ? ELSE to_number END,
	direction   = CASE WHEN direction = '' THEN ? ELSE direction END,
	disp_status = CASE WHEN disp_status = '' THEN ? ELSE disp_status END,
	disp_duration_sec = CASE WHEN disp_duration_sec = 0 THEN ? ELSE disp_duration_sec END,
	amd_answered_by = CASE WHEN amd_answered_by = '' THEN ? ELSE amd_answered_by END,
	updated_at  = ?
WHERE id = ?;`,
		prettyJSON, millis(now), rec.AccountSid, nullIfEmpty(rec.Sid),
		rec.From, rec.To, rec.Direction, rec.Status, rec.DurationSec,
		rec.AnsweredBy, millis(now), callPK)
	if err != nil {
		return fmt.Errorf("store: save cb record %d: %w", callPK, err)
	}

	return nil
}

// --- VoiceML per-account API keys ----------------------------------

// VoiceMLAccount is one stored VoiceML credential: the API key for
// one AccountSid, used to fetch that account's Call records on
// request. Keys are stored so the fetch can run; they are never
// shown back in full by the UI.
type VoiceMLAccount struct {
	AccountSid string
	APIKey     string
	CreatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// UpsertVoiceMLAccount adds or replaces the API key for an account.
func (s *Store) UpsertVoiceMLAccount(ctx context.Context, a VoiceMLAccount) error {
	now := time.Now().UTC()

	_, err := s.db.ExecContext(ctx, `
INSERT INTO voiceml_accounts (account_sid, api_key, created_by, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(account_sid) DO UPDATE SET
	api_key = excluded.api_key, updated_at = excluded.updated_at;`,
		strings.TrimSpace(a.AccountSid), a.APIKey, a.CreatedBy, millis(now), millis(now))
	if err != nil {
		return fmt.Errorf("store: upsert cb account: %w", err)
	}

	return nil
}

// GetVoiceMLAccount returns the account row (including key), or (nil, nil).
func (s *Store) GetVoiceMLAccount(ctx context.Context, accountSid string) (*VoiceMLAccount, error) {
	var a VoiceMLAccount

	var created, updated int64

	err := s.db.QueryRowContext(ctx,
		`SELECT account_sid, api_key, created_by, created_at, updated_at
		 FROM voiceml_accounts WHERE account_sid = ?`, strings.TrimSpace(accountSid)).
		Scan(&a.AccountSid, &a.APIKey, &a.CreatedBy, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("store: get cb account: %w", err)
	}

	a.CreatedAt, a.UpdatedAt = fromMillis(created), fromMillis(updated)

	return &a, nil
}

// ListVoiceMLAccounts returns every configured account, ordered by SID.
func (s *Store) ListVoiceMLAccounts(ctx context.Context) ([]VoiceMLAccount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_sid, api_key, created_by, created_at, updated_at
		 FROM voiceml_accounts ORDER BY account_sid`)
	if err != nil {
		return nil, fmt.Errorf("store: list cb accounts: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []VoiceMLAccount

	for rows.Next() {
		var a VoiceMLAccount

		var created, updated int64

		if err := rows.Scan(&a.AccountSid, &a.APIKey, &a.CreatedBy, &created, &updated); err != nil {
			return nil, fmt.Errorf("store: scan cb account: %w", err)
		}

		a.CreatedAt, a.UpdatedAt = fromMillis(created), fromMillis(updated)
		out = append(out, a)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list cb accounts rows: %w", err)
	}

	return out, nil
}

// DeleteVoiceMLAccount removes an account's key. Returns whether one existed.
func (s *Store) DeleteVoiceMLAccount(ctx context.Context, accountSid string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM voiceml_accounts WHERE account_sid = ?`, strings.TrimSpace(accountSid))
	if err != nil {
		return false, fmt.Errorf("store: delete cb account: %w", err)
	}

	n, err := res.RowsAffected()

	return n > 0, err
}

// --- Audit log --------------------------------------------------------------

// AuditEntry is one recorded administrative action.
type AuditEntry struct {
	ID        int64
	Actor     string
	Action    string
	Target    string
	Detail    string
	CreatedAt time.Time
}

// Audit records an administrative action (recording deletion, user
// and key management, record fetches). Best-effort by design at call
// sites: a failed audit write is logged there, never a reason to fail
// the action itself — but the store reports errors honestly.
func (s *Store) Audit(ctx context.Context, actor, action, target, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (actor, action, target, detail, created_at)
		 VALUES (?, ?, ?, ?, ?);`,
		actor, action, target, detail, millis(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("store: audit %s: %w", action, err)
	}

	return nil
}

// ListAudit returns the newest audit entries first.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, actor, action, target, detail, created_at
		 FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []AuditEntry

	for rows.Next() {
		var e AuditEntry

		var created int64

		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &e.Detail, &created); err != nil {
			return nil, fmt.Errorf("store: scan audit: %w", err)
		}

		e.CreatedAt = fromMillis(created)
		out = append(out, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit rows: %w", err)
	}

	return out, nil
}

// --- Google identity pinning + session revocation ---------------------------

// SetGoogleSubIfEmpty pins the Google account ID (sub claim) on
// first sign-in. Returns false when a different sub is already
// pinned (the caller must reject the sign-in).
func (s *Store) SetGoogleSubIfEmpty(ctx context.Context, email, sub string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET google_sub = ? WHERE email = ? AND google_sub = '';`,
		sub, NormalizeEmail(email))
	if err != nil {
		return false, fmt.Errorf("store: pin google sub: %w", err)
	}

	n, err := res.RowsAffected()

	return n > 0, err
}

// BumpSessionEpoch revokes every outstanding session for a user:
// sessions carry the epoch they were minted at and stop validating
// as soon as it no longer matches.
func (s *Store) BumpSessionEpoch(ctx context.Context, email string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET session_epoch = session_epoch + 1 WHERE email = ?;`,
		NormalizeEmail(email))
	if err != nil {
		return fmt.Errorf("store: bump session epoch: %w", err)
	}

	return nil
}

// Finalizer adapts Store to siprecsrv.Finalizer.
type Finalizer struct{ S *Store }

// Finalize implements siprecsrv.Finalizer.
func (f Finalizer) Finalize(ctx context.Context, leg siprecsrv.FinalizedLeg) error {
	return f.S.RecordLeg(ctx, leg)
}
