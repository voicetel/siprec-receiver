package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// Databases written before the VoiceML rename carry a cb_accounts
// table and cb_record_json / cb_fetched_at columns. Opening one must
// migrate the names without losing keys or fetched records.
func TestOpen_MigratesLegacyCBNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}

	_, err = raw.Exec(`
CREATE TABLE cb_accounts (
	account_sid TEXT PRIMARY KEY, api_key TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL);
INSERT INTO cb_accounts VALUES ('ACtest', 'key-legacy', 'admin@example.com', 1, 1);
CREATE TABLE calls (
	id INTEGER PRIMARY KEY AUTOINCREMENT, sip_call_id TEXT UNIQUE,
	call_sid TEXT UNIQUE, fs_uuid TEXT, account_sid TEXT NOT NULL DEFAULT '',
	from_uri TEXT NOT NULL DEFAULT '', from_number TEXT NOT NULL DEFAULT '',
	to_number TEXT NOT NULL DEFAULT '', direction TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL DEFAULT 0, ended_at INTEGER NOT NULL DEFAULT 0,
	duration_sec INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'notified',
	amd_answered_by TEXT NOT NULL DEFAULT '', amd_duration_ms INTEGER NOT NULL DEFAULT 0,
	amd_call_status TEXT NOT NULL DEFAULT '', amd_notified_at INTEGER NOT NULL DEFAULT 0,
	amd_raw TEXT NOT NULL DEFAULT '', metadata_json TEXT NOT NULL DEFAULT '',
	metadata_xml TEXT NOT NULL DEFAULT '',
	cb_record_json TEXT NOT NULL DEFAULT '', cb_fetched_at INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
INSERT INTO calls (sip_call_id, call_sid, cb_record_json, cb_fetched_at, created_at, updated_at)
VALUES ('dlg-legacy', 'CAlegacy', '{"sid":"CAlegacy"}', 1234567, 1, 1);`)
	if err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}

	_ = raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy db: %v", err)
	}

	defer func() { _ = s.Close() }()

	ctx := context.Background()

	acct, err := s.GetVoiceMLAccount(ctx, "ACtest")
	if err != nil || acct == nil || acct.APIKey != "key-legacy" {
		t.Fatalf("migrated account = %+v, %v", acct, err)
	}

	calls, total, _ := s.ListCalls(ctx, ListFilter{})
	if total != 1 || len(calls) != 1 {
		t.Fatalf("calls = %d", total)
	}

	if calls[0].VoiceMLRecordJSON != `{"sid":"CAlegacy"}` || calls[0].VoiceMLFetchedAt.IsZero() {
		t.Errorf("migrated record = %q fetched %v",
			calls[0].VoiceMLRecordJSON, calls[0].VoiceMLFetchedAt)
	}

	// And the migrated DB keeps working for new writes.
	if err := s.UpsertVoiceMLAccount(ctx, VoiceMLAccount{
		AccountSid: "ACnew", APIKey: "k", CreatedBy: "a@b.c",
	}); err != nil {
		t.Fatalf("upsert after migration: %v", err)
	}

}
