// Package store is siprec-receiver's SQLite persistence: one row per
// recorded (or AMD-notified) call, one row per recorded leg (WAV file),
// and the web UI's user allowlist.
//
// A call row can be born two ways and the two must merge into one:
//
//   - Recording-first: the SRS finalizes a SIPREC session (keyed by the
//     SIPREC dialog's SIP Call-ID, optionally carrying a CallSid from
//     the INVITE Request-URI's cb-callsid parameter and the FreeSWITCH
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
	MetadataJSON  string
	MetadataXML   string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LegCount      int
	HasWriteError bool  // any leg carries a WAV write error
	Legs          []Leg // populated by GetCall
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
	Email     string
	Role      string // "admin" or "user"
	CreatedBy string
	CreatedAt time.Time
	Disabled  bool
}

// AMDNotification is one call-notification (answering-machine
// disposition) as accepted by the notify endpoint. Field names follow
// callBroadcast's AMD webhook payload (internal/api AMDCallbackPayload:
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

func (s *Store) migrate(ctx context.Context) error {
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
	email      TEXT PRIMARY KEY,
	role       TEXT NOT NULL DEFAULT 'user',
	created_by TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	disabled   INTEGER NOT NULL DEFAULT 0
);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}

	// Columns added after v0.1.0 for databases that already exist.
	if err := s.addColumnIfMissing(ctx, "legs", "write_error",
		`ALTER TABLE legs ADD COLUMN write_error TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
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

	// Claim any AMD-first placeholder row for this call (matched by
	// CallSid or FsUUID) by stamping it with our SIP Call-ID, so the
	// upsert below refreshes that row — AMD fields and all — instead
	// of colliding with its UNIQUE call_sid.
	if leg.CallSid != "" {
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
		leg.CallID, nullIfEmpty(leg.CallSid), leg.FsUUID, leg.AccountSid, leg.FromURI,
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
			WHERE call_sid = ? AND id != ? AND sip_call_id IS NULL`, callSid)
	}

	if errors.Is(err, sql.ErrNoRows) && fsUUID != "" {
		err = find(`SELECT id, amd_answered_by, amd_duration_ms, amd_call_status, amd_raw,
			amd_notified_at, started_at FROM calls
			WHERE fs_uuid = ? AND id != ? AND sip_call_id IS NULL`, fsUUID)
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

	var id int64

	find := func(query string, arg string) error {
		return s.db.QueryRowContext(ctx, query, arg).Scan(&id)
	}

	err := sql.ErrNoRows
	if n.CallSid != "" {
		err = find(`SELECT id FROM calls WHERE call_sid = ?`, n.CallSid)
	}

	if errors.Is(err, sql.ErrNoRows) && n.SipCallID != "" {
		err = find(`SELECT id FROM calls WHERE sip_call_id = ?`, n.SipCallID)
	}

	if errors.Is(err, sql.ErrNoRows) && n.FsUUID != "" {
		err = find(`SELECT id FROM calls WHERE fs_uuid = ?`, n.FsUUID)
	}

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("store: amd lookup: %w", err)
	}

	if err == nil {
		_, uerr := s.db.ExecContext(ctx, `
UPDATE calls SET
	amd_answered_by = ?, amd_duration_ms = ?, amd_call_status = ?,
	amd_raw = ?, amd_notified_at = ?,
	call_sid    = COALESCE(call_sid, ?),
	sip_call_id = COALESCE(sip_call_id, ?),
	fs_uuid     = CASE WHEN fs_uuid = '' THEN ? ELSE fs_uuid END,
	account_sid = CASE WHEN account_sid = '' THEN ? ELSE account_sid END,
	from_number = CASE WHEN from_number = '' THEN ? ELSE from_number END,
	to_number   = CASE WHEN to_number = '' THEN ? ELSE to_number END,
	direction   = CASE WHEN direction = '' THEN ? ELSE direction END,
	updated_at  = ?
WHERE id = ?;`,
			n.AnsweredBy, n.DurationMs, n.CallStatus, n.Raw, millis(now),
			nullIfEmpty(n.CallSid), nullIfEmpty(n.SipCallID), n.FsUUID,
			n.AccountSid, n.From, n.To, n.Direction, millis(now), id)
		if uerr != nil {
			return 0, false, fmt.Errorf("store: amd update %d: %w", id, uerr)
		}

		return id, true, nil
	}

	res, ierr := s.db.ExecContext(ctx, `
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

	return newID, false, nil
}

// ListFilter narrows ListCalls.
type ListFilter struct {
	Query  string // substring match on numbers / SIDs / UUID
	AMD    string // exact amd_answered_by match when non-empty
	Limit  int
	Offset int
}

// ListCalls returns calls newest-first plus the total match count.
func (s *Store) ListCalls(ctx context.Context, f ListFilter) ([]Call, int, error) {
	where := `WHERE 1=1`
	args := []any{}

	if f.Query != "" {
		where += ` AND (from_number LIKE ? OR to_number LIKE ? OR call_sid LIKE ?
			OR sip_call_id LIKE ? OR fs_uuid LIKE ?)`
		like := "%" + f.Query + "%"
		args = append(args, like, like, like, like, like)
	}

	if f.AMD != "" {
		where += ` AND amd_answered_by = ?`
		args = append(args, f.AMD)
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
	amd_notified_at, amd_raw, metadata_json, metadata_xml, created_at, updated_at,
	(SELECT COUNT(*) FROM legs WHERE legs.call_id = calls.id),
	(SELECT COUNT(*) FROM legs WHERE legs.call_id = calls.id AND legs.write_error != '')`

func scanCall(src interface{ Scan(...any) error }) (Call, error) {
	var c Call

	var started, ended, amdAt, created, updated int64

	var writeErrLegs int

	if err := src.Scan(&c.ID, &c.SipCallID, &c.CallSid, &c.FsUUID, &c.AccountSid,
		&c.FromURI, &c.FromNumber, &c.ToNumber, &c.Direction, &started, &ended,
		&c.DurationSec, &c.Status, &c.AMDAnsweredBy, &c.AMDDurationMs, &c.AMDCallStatus,
		&amdAt, &c.AMDRaw, &c.MetadataJSON, &c.MetadataXML, &created, &updated,
		&c.LegCount, &writeErrLegs); err != nil {
		return Call{}, fmt.Errorf("store: scan call: %w", err)
	}

	c.HasWriteError = writeErrLegs > 0

	c.StartedAt, c.EndedAt, c.AMDNotifiedAt = fromMillis(started), fromMillis(ended), fromMillis(amdAt)
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
INSERT INTO users (email, role, created_by, created_at, disabled)
VALUES (?, ?, ?, ?, ?)
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
		`SELECT email, role, created_by, created_at, disabled FROM users WHERE email = ?`,
		NormalizeEmail(email)).Scan(&u.Email, &u.Role, &u.CreatedBy, &created, &disabled)
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
		`SELECT email, role, created_by, created_at, disabled FROM users ORDER BY email`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []User

	for rows.Next() {
		var u User

		var created int64

		var disabled int

		if err := rows.Scan(&u.Email, &u.Role, &u.CreatedBy, &created, &disabled); err != nil {
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

// Finalizer adapts Store to siprecsrv.Finalizer.
type Finalizer struct{ S *Store }

// Finalize implements siprecsrv.Finalizer.
func (f Finalizer) Finalize(ctx context.Context, leg siprecsrv.FinalizedLeg) error {
	return f.S.RecordLeg(ctx, leg)
}
