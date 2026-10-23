package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

// wantErr fails unless err is non-nil and mentions substr.
func wantErr(t *testing.T, err error, substr string) {
	t.Helper()

	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("err = %v, want one containing %q", err, substr)
	}
}

func mustExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()

	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// withRawConn runs fn against the store's single SQLite connection
// (Open caps the pool at one), so per-connection hooks registered
// here apply to every later store call.
func withRawConn(t *testing.T, s *Store, fn func(*sqlite3.SQLiteConn)) {
	t.Helper()

	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}

	defer func() { _ = conn.Close() }()

	if err := conn.Raw(func(dc any) error {
		c, ok := dc.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("driver conn is %T", dc)
		}

		fn(c)

		return nil
	}); err != nil {
		t.Fatalf("Raw: %v", err)
	}
}

// denyWhen makes SQLite refuse to prepare any statement for which
// pred reports true (see sqlite3_set_authorizer).
func denyWhen(t *testing.T, s *Store, pred func(op int, arg1, arg2 string) bool) {
	t.Helper()

	withRawConn(t, s, func(c *sqlite3.SQLiteConn) {
		c.RegisterAuthorizer(func(op int, arg1, arg2, _ string) int {
			if pred(op, arg1, arg2) {
				return sqlite3.SQLITE_DENY
			}

			return sqlite3.SQLITE_OK
		})
	})
}

// denyAfter denies op once it has been authorized n times.
func denyAfter(t *testing.T, s *Store, op, n int, arg1 string) {
	t.Helper()

	var seen atomic.Int32

	denyWhen(t, s, func(o int, a1, _ string) bool {
		if o != op || (arg1 != "" && a1 != arg1) {
			return false
		}

		return int(seen.Add(1)) > n
	})
}

// failCommits turns every COMMIT on the store's connection into a
// rollback, so Commit reports an error.
func failCommits(t *testing.T, s *Store) {
	t.Helper()

	withRawConn(t, s, func(c *sqlite3.SQLiteConn) {
		c.RegisterCommitHook(func() int { return 1 })
	})
}

// addBoom creates a one-row table whose value makes abs() raise
// "integer overflow" at step time, for views that must fail
// mid-iteration rather than at prepare time.
func addBoom(t *testing.T, s *Store) {
	t.Helper()

	mustExec(t, s, `CREATE TABLE boom (x INTEGER)`)
	mustExec(t, s, `INSERT INTO boom VALUES (?)`, int64(math.MinInt64))
}

func closedStore(t *testing.T) *Store {
	t.Helper()

	s := openTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	return s
}

func recordSimpleLeg(t *testing.T, s *Store, callID string, started time.Time) int64 {
	t.Helper()

	ctx := context.Background()
	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: callID, Label: "mixed", WAVPath: "/rec/" + callID + ".wav",
		DurationSec: 1, StartedAt: started,
	}); err != nil {
		t.Fatalf("RecordLeg %s: %v", callID, err)
	}

	var id int64
	if err := s.db.QueryRow(`SELECT id FROM calls WHERE sip_call_id = ?`, callID).Scan(&id); err != nil {
		t.Fatalf("lookup %s: %v", callID, err)
	}

	return id
}

func TestClosedStore_EveryMethodReportsError(t *testing.T) {
	s := closedStore(t)
	ctx := context.Background()
	leg := siprecsrv.FinalizedLeg{CallID: "dlg-closed", WAVPath: "/rec/closed.wav"}

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"GetSetting", func() error { _, _, err := s.GetSetting(ctx, "k"); return err }, "get setting"},
		{"SetSetting", func() error { return s.SetSetting(ctx, "k", "v") }, "set setting"},
		{"EffectiveRetentionDays", func() error {
			days, fromUI, err := s.EffectiveRetentionDays(ctx, 9)
			if days != 9 || fromUI {
				return fmt.Errorf("days=%d fromUI=%v, want env default: %w", days, fromUI, err)
			}

			return err
		}, "get setting"},
		{"PurgeExpired", func() error { _, _, err := s.PurgeExpired(ctx, 30); return err }, "retention select paths"},
		{"DBStats", func() error { _, err := s.DBStats(ctx); return err }, "db stats"},
		{"Checkpoint", func() error { return s.Checkpoint(ctx) }, "wal checkpoint"},
		{"Optimize", func() error { return s.Optimize(ctx) }, "optimize"},
		{"VacuumIfNeeded", func() error { _, err := s.VacuumIfNeeded(ctx, 0.1); return err }, "db stats"},
		{"KnownWAVPaths", func() error { _, err := s.KnownWAVPaths(ctx); return err }, "known wav paths"},
		{"AdoptOrphan", func() error {
			return s.AdoptOrphan(ctx, "dlg", "x", "/rec/x.wav", time.Now(), 1)
		}, "adopt orphan call"},
		{"RecoverOrphans", func() error { _, err := s.RecoverOrphans(ctx, t.TempDir()); return err }, "known wav paths"},
		{"DeleteCallsStartedBefore", func() error {
			_, _, err := s.DeleteCallsStartedBefore(ctx, time.Now())
			return err
		}, "retention select paths"},
		{"columnExists", func() error { _, err := s.columnExists(ctx, "calls", "id"); return err }, "pragma calls"},
		{"migrate", func() error { return s.migrate(ctx) }, "legacy table check"},
		{"RecordLeg", func() error { return s.RecordLeg(ctx, leg) }, "record leg begin"},
		{"Finalize", func() error { return Finalizer{S: s}.Finalize(ctx, leg) }, "record leg begin"},
		{"ApplyAMD", func() error { _, _, err := s.ApplyAMD(ctx, AMDNotification{CallSid: "CA1"}); return err }, "amd begin"},
		{"ApplyDisposition", func() error {
			_, _, err := s.ApplyDisposition(ctx, DispositionNotification{CallSid: "CA1"})
			return err
		}, "disposition begin"},
		{"ListCalls", func() error { _, _, err := s.ListCalls(ctx, ListFilter{}); return err }, "count calls"},
		{"GetCall", func() error { _, err := s.GetCall(ctx, 1); return err }, "scan call"},
		{"GetLeg", func() error { _, err := s.GetLeg(ctx, 1); return err }, "get leg"},
		{"DeleteCall", func() error { _, err := s.DeleteCall(ctx, 1); return err }, "scan call"},
		{"UpsertUser", func() error { return s.UpsertUser(ctx, User{Email: "a@b.c"}) }, "upsert user"},
		{"GetUser", func() error { _, err := s.GetUser(ctx, "a@b.c"); return err }, "get user"},
		{"ListUsers", func() error { _, err := s.ListUsers(ctx); return err }, "list users"},
		{"DeleteUser", func() error { _, err := s.DeleteUser(ctx, "a@b.c"); return err }, "delete user"},
		{"SaveVoiceMLRecord", func() error { return s.SaveVoiceMLRecord(ctx, 1, VoiceMLRecord{}, "{}") }, "save cb record"},
		{"UpsertVoiceMLAccount", func() error {
			return s.UpsertVoiceMLAccount(ctx, VoiceMLAccount{AccountSid: "AC1"})
		}, "upsert cb account"},
		{"GetVoiceMLAccount", func() error { _, err := s.GetVoiceMLAccount(ctx, "AC1"); return err }, "get cb account"},
		{"ListVoiceMLAccounts", func() error { _, err := s.ListVoiceMLAccounts(ctx); return err }, "list cb accounts"},
		{"DeleteVoiceMLAccount", func() error { _, err := s.DeleteVoiceMLAccount(ctx, "AC1"); return err }, "delete cb account"},
		{"Audit", func() error { return s.Audit(ctx, "a", "act", "t", "d") }, "audit act"},
		{"ListAudit", func() error { _, err := s.ListAudit(ctx, 0); return err }, "list audit"},
		{"SetGoogleSubIfEmpty", func() error { _, err := s.SetGoogleSubIfEmpty(ctx, "a@b.c", "sub"); return err }, "pin google sub"},
		{"BumpSessionEpoch", func() error { return s.BumpSessionEpoch(ctx, "a@b.c") }, "bump session epoch"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			wantErr(t, err, tc.want)

			if !strings.Contains(err.Error(), "database is closed") {
				t.Errorf("err = %v, want the closed-database cause", err)
			}
		})
	}
}

func TestOpen_UnopenablePathFails(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "missing-dir", "x.db"))
	if s != nil {
		t.Errorf("store = %v, want nil on failure", s)
	}

	wantErr(t, err, "legacy table check")
}

func TestMigrate_LegacyTableCheckSecondQueryFails(t *testing.T) {
	s := openTestStore(t)

	denyAfter(t, s, sqlite3.SQLITE_SELECT, 1, "")
	wantErr(t, s.migrate(context.Background()), "legacy table check")
}

func TestMigrate_RenameLegacyAccountsTableFails(t *testing.T) {
	s := openTestStore(t)

	mustExec(t, s, `ALTER TABLE voiceml_accounts RENAME TO cb_accounts`)
	denyWhen(t, s, func(op int, _, _ string) bool { return op == sqlite3.SQLITE_ALTER_TABLE })
	wantErr(t, s.migrate(context.Background()), "rename cb_accounts")
}

func TestMigrate_MergesLegacyAccountsIntoExistingTable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertVoiceMLAccount(ctx, VoiceMLAccount{AccountSid: "ACboth", APIKey: "new-key"}); err != nil {
		t.Fatalf("UpsertVoiceMLAccount: %v", err)
	}

	mustExec(t, s, `CREATE TABLE cb_accounts AS SELECT * FROM voiceml_accounts`)
	mustExec(t, s, `UPDATE cb_accounts SET api_key = 'legacy-key'`)
	mustExec(t, s, `INSERT INTO cb_accounts VALUES ('AClegacy', 'only-legacy', 'admin', 1, 1)`)

	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	both, err := s.GetVoiceMLAccount(ctx, "ACboth")
	if err != nil || both == nil || both.APIKey != "new-key" {
		t.Errorf("existing account = %+v, %v; the current table must win", both, err)
	}

	legacy, err := s.GetVoiceMLAccount(ctx, "AClegacy")
	if err != nil || legacy == nil || legacy.APIKey != "only-legacy" {
		t.Errorf("legacy-only account = %+v, %v; want it merged", legacy, err)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'cb_accounts'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("cb_accounts still present (n=%d, err=%v)", n, err)
	}
}

func TestMigrate_MergeLegacyAccountsFails(t *testing.T) {
	s := openTestStore(t)

	mustExec(t, s, `CREATE TABLE cb_accounts (only_column TEXT)`)
	wantErr(t, s.migrate(context.Background()), "merge cb_accounts")
}

func TestMigrate_LegacyColumnProbeFails(t *testing.T) {
	s := openTestStore(t)

	mustExec(t, s, `ALTER TABLE calls ADD COLUMN cb_record_json TEXT NOT NULL DEFAULT ''`)
	denyAfter(t, s, sqlite3.SQLITE_PRAGMA, 1, "table_info")
	wantErr(t, s.migrate(context.Background()), "pragma calls")
}

func TestMigrate_LegacyColumnRenameFails(t *testing.T) {
	s := openTestStore(t)

	mustExec(t, s, `ALTER TABLE calls DROP COLUMN voiceml_record_json`)
	mustExec(t, s, `ALTER TABLE calls ADD COLUMN cb_record_json TEXT NOT NULL DEFAULT ''`)
	denyWhen(t, s, func(op int, _, _ string) bool { return op == sqlite3.SQLITE_ALTER_TABLE })
	wantErr(t, s.migrate(context.Background()), "rename column cb_record_json")
}

func TestMigrate_SchemaDDLFails(t *testing.T) {
	s := openTestStore(t)

	mustExec(t, s, `DROP TABLE audit_log`)
	denyWhen(t, s, func(op int, _, _ string) bool { return op == sqlite3.SQLITE_CREATE_TABLE })
	wantErr(t, s.migrate(context.Background()), "store: migrate")
}

func TestMigrate_ColumnProbeFailsPerTable(t *testing.T) {
	for _, table := range []string{"legs", "users", "calls"} {
		t.Run(table, func(t *testing.T) {
			s := openTestStore(t)

			denyWhen(t, s, func(op int, arg1, arg2 string) bool {
				return op == sqlite3.SQLITE_PRAGMA && arg1 == "table_info" && arg2 == table
			})
			wantErr(t, s.migrate(context.Background()), "pragma "+table)
		})
	}
}

func TestMigrate_AddMissingColumnFails(t *testing.T) {
	s := openTestStore(t)

	mustExec(t, s, `ALTER TABLE legs DROP COLUMN write_error`)
	denyWhen(t, s, func(op int, _, _ string) bool { return op == sqlite3.SQLITE_ALTER_TABLE })
	wantErr(t, s.migrate(context.Background()), "add column legs.write_error")
}

func TestMigrate_RestoresDroppedColumns(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mustExec(t, s, `ALTER TABLE users DROP COLUMN session_epoch`)
	mustExec(t, s, `ALTER TABLE calls DROP COLUMN disp_raw`)

	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, c := range [][2]string{{"users", "session_epoch"}, {"calls", "disp_raw"}} {
		ok, err := s.columnExists(ctx, c[0], c[1])
		if err != nil || !ok {
			t.Errorf("%s.%s restored = %v, %v", c[0], c[1], ok, err)
		}
	}

	ok, err := s.columnExists(ctx, "calls", "no_such_column")
	if err != nil || ok {
		t.Errorf("columnExists(no_such_column) = %v, %v", ok, err)
	}
}

func TestMigrate_StampVersionFails(t *testing.T) {
	s := openTestStore(t)

	denyWhen(t, s, func(op int, arg1, _ string) bool {
		return op == sqlite3.SQLITE_PRAGMA && arg1 == "user_version"
	})
	wantErr(t, s.migrate(context.Background()), "stamp user_version")
}

func TestRecordLeg_StatementFailures(t *testing.T) {
	isOp := func(want int, table, column string) func(int, string, string) bool {
		return func(op int, arg1, arg2 string) bool {
			return op == want && (table == "" || arg1 == table) && (column == "" || arg2 == column)
		}
	}

	// seedRow gives the leg's dialog an existing row, so RecordLeg takes
	// the update path.
	seedRow := func(t *testing.T, s *Store) { recordSimpleLeg(t, s, "dlg", time.Now()) }

	cases := []struct {
		name string
		seed func(*testing.T, *Store)
		leg  siprecsrv.FinalizedLeg
		deny func(int, string, string) bool
		want string
	}{
		{"find rows", nil, siprecsrv.FinalizedLeg{CallID: "dlg", CallSid: "CA1"},
			isOp(sqlite3.SQLITE_SELECT, "", ""), "find call rows"},
		{"find by dialog", nil, siprecsrv.FinalizedLeg{CallID: "dlg"},
			isOp(sqlite3.SQLITE_READ, "legs", "sip_call_id"), "find call rows"},
		{"find by fs_uuid", nil, siprecsrv.FinalizedLeg{CallID: "dlg", FsUUID: "uuid-1"},
			isOp(sqlite3.SQLITE_READ, "calls", "fs_uuid"), "find call rows"},
		{"insert call", nil, siprecsrv.FinalizedLeg{CallID: "dlg"},
			isOp(sqlite3.SQLITE_INSERT, "calls", ""), "insert call dlg"},
		{"update call", seedRow, siprecsrv.FinalizedLeg{CallID: "dlg"},
			isOp(sqlite3.SQLITE_UPDATE, "calls", "metadata_xml"), "update call 1"},
		{"adopt call_sid", func(t *testing.T, s *Store) {
			mustExec(t, s, `INSERT INTO calls (fs_uuid, status, started_at, created_at, updated_at)
				VALUES ('uuid-1', 'notified', 1, 1, 1)`)
		}, siprecsrv.FinalizedLeg{CallID: "dlg", CallSid: "CA1", FsUUID: "uuid-1"},
			isOp(sqlite3.SQLITE_UPDATE, "calls", "call_sid"), "adopt call_sid"},
		{"insert leg", nil, siprecsrv.FinalizedLeg{CallID: "dlg", Label: "mixed"},
			isOp(sqlite3.SQLITE_INSERT, "legs", ""), "insert leg mixed"},
		{"duration", nil, siprecsrv.FinalizedLeg{CallID: "dlg"},
			isOp(sqlite3.SQLITE_UPDATE, "calls", "duration_sec"), "call 1 duration"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			if tc.seed != nil {
				tc.seed(t, s)
			}

			tc.leg.WAVPath = "/rec/fail.wav"
			denyWhen(t, s, tc.deny)
			wantErr(t, s.RecordLeg(context.Background(), tc.leg), tc.want)
		})
	}
}

// A mid-iteration failure while reading candidate rows fails the
// finalize rather than resolving against a partial set.
func TestRecordLeg_CandidateRowsFail(t *testing.T) {
	s := openTestStore(t)

	mustExec(t, s, `INSERT INTO calls (fs_uuid, status, started_at, created_at, updated_at)
		VALUES ('uuid-boom', 'notified', 1, 1, 1)`)
	addBoom(t, s)
	mustExec(t, s, `ALTER TABLE calls RENAME TO calls_real`)
	// fs_uuid evaluates abs(boom.x), which overflows at step time.
	mustExec(t, s, `CREATE VIEW calls AS SELECT id, call_sid, status, sip_call_id,
		fs_uuid || substr(abs(boom.x), 1, 0) AS fs_uuid FROM calls_real, boom`)

	wantErr(t, s.RecordLeg(context.Background(), siprecsrv.FinalizedLeg{
		CallID: "dlg", FsUUID: "uuid-boom", WAVPath: "/rec/x.wav",
	}), "find call rows")
}

func TestRecordLeg_CommitFails(t *testing.T) {
	s := openTestStore(t)

	failCommits(t, s)
	wantErr(t, s.RecordLeg(context.Background(), siprecsrv.FinalizedLeg{
		CallID: "dlg", WAVPath: "/rec/x.wav",
	}), "record leg commit")
}

// seedFsPlaceholders inserts two AMD-only rows sharing one FsUUID:
// RecordLeg claims one and must fold the other into it.
func seedFsPlaceholders(t *testing.T, s *Store, fsUUID string) {
	t.Helper()

	mustExec(t, s, `
INSERT INTO calls (fs_uuid, amd_answered_by, amd_raw, started_at, status, created_at, updated_at)
VALUES (?, '', '', 1, 'notified', 1, 1), (?, 'machine_start', '{"a":1}', 2, 'notified', 1, 1)`,
		fsUUID, fsUUID)
}

func TestRecordLeg_MergesSecondFsUUIDPlaceholder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	seedFsPlaceholders(t, s, "uuid-merge")

	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-merge", FsUUID: "uuid-merge", WAVPath: "/rec/m.wav", StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	calls, total, err := s.ListCalls(ctx, ListFilter{})
	if err != nil || total != 1 || len(calls) != 1 {
		t.Fatalf("calls = %d (%v), want the placeholders merged into 1", total, err)
	}

	if c := calls[0]; c.SipCallID != "dlg-merge" || c.AMDAnsweredBy != "machine_start" || c.Status != "completed" {
		t.Errorf("merged call = sip %q amd %q status %q", c.SipCallID, c.AMDAnsweredBy, c.Status)
	}
}

func TestRecordLeg_MergeWriteFailures(t *testing.T) {
	cases := []struct {
		name string
		deny func(int, string, string) bool
		want string
	}{
		{"load", func(op int, arg1, arg2 string) bool {
			return op == sqlite3.SQLITE_READ && arg1 == "calls" && arg2 == "voiceml_fetched_at"
		}, "load call"},
		{"move legs", func(op int, arg1, _ string) bool {
			return op == sqlite3.SQLITE_UPDATE && arg1 == "legs"
		}, "move legs 2 -> 1"},
		{"delete", func(op int, arg1, _ string) bool {
			return op == sqlite3.SQLITE_DELETE && arg1 == "calls"
		}, "delete merged call 2"},
		{"fold", func(op int, arg1, arg2 string) bool {
			return op == sqlite3.SQLITE_UPDATE && arg1 == "calls" && arg2 == "amd_raw"
		}, "merge call 2 into 1"},
		{"duration", func(op int, arg1, arg2 string) bool {
			return op == sqlite3.SQLITE_UPDATE && arg1 == "calls" && arg2 == "duration_sec"
		}, "call 1 duration"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()

			seedFsPlaceholders(t, s, "uuid-merge")
			denyWhen(t, s, tc.deny)
			wantErr(t, s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
				CallID: "dlg-merge", FsUUID: "uuid-merge", WAVPath: "/rec/m.wav",
			}), tc.want)

			denyWhen(t, s, func(int, string, string) bool { return false })

			if _, total, _ := s.ListCalls(ctx, ListFilter{}); total != 2 {
				t.Errorf("calls after failed merge = %d, want both placeholders rolled back intact", total)
			}
		})
	}
}

func TestFinalizer_RecordsLeg(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := (Finalizer{S: s}).Finalize(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-fin", Label: "mixed", WAVPath: "/rec/fin.wav", DurationSec: 4, StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	calls, total, err := s.ListCalls(ctx, ListFilter{})
	if err != nil || total != 1 || calls[0].SipCallID != "dlg-fin" || calls[0].LegCount != 1 {
		t.Fatalf("after Finalize: calls=%+v total=%d err=%v", calls, total, err)
	}
}

// Both notify paths share the same lookup/update/insert/commit shape.
type notifier struct {
	name   string
	prefix string
	apply  func(s *Store, callSid, sipCallID, fsUUID string) (int64, bool, error)
}

func notifiers() []notifier {
	ctx := context.Background()

	return []notifier{
		{"AMD", "amd", func(s *Store, callSid, sipCallID, fsUUID string) (int64, bool, error) {
			return s.ApplyAMD(ctx, AMDNotification{
				CallSid: callSid, SipCallID: sipCallID, FsUUID: fsUUID, AnsweredBy: "human",
			})
		}},
		{"Disposition", "disposition", func(s *Store, callSid, sipCallID, fsUUID string) (int64, bool, error) {
			return s.ApplyDisposition(ctx, DispositionNotification{
				CallSid: callSid, SipCallID: sipCallID, FsUUID: fsUUID, CallStatus: "completed",
				AnsweredBy: "human",
			})
		}},
	}
}

func TestNotify_NoKeyRejected(t *testing.T) {
	s := openTestStore(t)

	for _, n := range notifiers() {
		if _, _, err := n.apply(s, "", "", ""); !errors.Is(err, ErrNoKey) {
			t.Errorf("%s: err = %v, want ErrNoKey", n.name, err)
		}
	}
}

func TestNotify_MatchesByFsUUIDAndStampsUnownedCallSid(t *testing.T) {
	for _, n := range notifiers() {
		t.Run(n.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()

			if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
				CallID: "dlg-fs", FsUUID: "uuid-fs", WAVPath: "/rec/fs.wav", StartedAt: time.Now(),
			}); err != nil {
				t.Fatalf("RecordLeg: %v", err)
			}

			id, matched, err := n.apply(s, "CAunowned", "", "uuid-fs")
			if err != nil || !matched {
				t.Fatalf("apply: id=%d matched=%v err=%v", id, matched, err)
			}

			c, err := s.GetCall(ctx, id)
			if err != nil || c == nil || c.CallSid != "CAunowned" || c.SipCallID != "dlg-fs" ||
				c.AMDAnsweredBy != "human" {
				t.Fatalf("call = %+v, %v; want CallSid stamped onto the recording row", c, err)
			}
		})
	}
}

func TestNotify_StatementFailures(t *testing.T) {
	for _, n := range notifiers() {
		t.Run(n.name+"/lookup", func(t *testing.T) {
			s := openTestStore(t)

			denyWhen(t, s, func(op int, _, _ string) bool { return op == sqlite3.SQLITE_SELECT })

			_, _, err := n.apply(s, "", "", "uuid-x")
			wantErr(t, err, "find call rows")
		})

		t.Run(n.name+"/update", func(t *testing.T) {
			s := openTestStore(t)
			id := recordSimpleLeg(t, s, "dlg-u", time.Now())

			denyWhen(t, s, func(op int, arg1, _ string) bool { return op == sqlite3.SQLITE_UPDATE && arg1 == "calls" })

			_, _, err := n.apply(s, "", "dlg-u", "")
			wantErr(t, err, fmt.Sprintf("%s update %d", n.prefix, id))
		})

		t.Run(n.name+"/update commit", func(t *testing.T) {
			s := openTestStore(t)
			recordSimpleLeg(t, s, "dlg-c", time.Now())

			failCommits(t, s)

			_, _, err := n.apply(s, "", "dlg-c", "")
			wantErr(t, err, n.prefix+" commit")
		})

		t.Run(n.name+"/insert", func(t *testing.T) {
			s := openTestStore(t)

			denyWhen(t, s, func(op int, arg1, _ string) bool { return op == sqlite3.SQLITE_INSERT && arg1 == "calls" })

			_, _, err := n.apply(s, "CAnew", "", "")
			wantErr(t, err, n.prefix+" insert")
		})

		t.Run(n.name+"/insert commit", func(t *testing.T) {
			s := openTestStore(t)

			failCommits(t, s)

			_, _, err := n.apply(s, "CAnew", "", "")
			wantErr(t, err, n.prefix+" commit")
		})
	}
}

func TestApplyDisposition_PlaceholderCarriesAMDVerdict(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, matched, err := s.ApplyDisposition(ctx, DispositionNotification{
		CallSid: "CAdispamd", CallStatus: "completed", AnsweredBy: "machine_end_beep",
	})
	if err != nil || matched {
		t.Fatalf("ApplyDisposition: id=%d matched=%v err=%v", id, matched, err)
	}

	c, err := s.GetCall(ctx, id)
	if err != nil || c == nil || c.AMDAnsweredBy != "machine_end_beep" || c.AMDNotifiedAt.IsZero() ||
		c.Status != "notified" {
		t.Fatalf("placeholder = %+v, %v", c, err)
	}
}

func TestListCalls_FailuresAfterCount(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		s := openTestStore(t)
		recordSimpleLeg(t, s, "dlg-q", time.Now())

		denyWhen(t, s, func(op int, arg1, arg2 string) bool {
			return op == sqlite3.SQLITE_READ && arg1 == "legs" && arg2 == "write_error"
		})

		_, _, err := s.ListCalls(context.Background(), ListFilter{})
		wantErr(t, err, "list calls")
	})

	t.Run("scan", func(t *testing.T) {
		s := openTestStore(t)
		recordSimpleLeg(t, s, "dlg-s", time.Now())
		mustExec(t, s, `UPDATE calls SET started_at = 'not-a-number'`)

		_, _, err := s.ListCalls(context.Background(), ListFilter{})
		wantErr(t, err, "scan call")
	})

	t.Run("rows", func(t *testing.T) {
		s := openTestStore(t)
		recordSimpleLeg(t, s, "dlg-r", time.Now())
		addBoom(t, s)
		mustExec(t, s, `DROP TABLE legs`)
		mustExec(t, s, `CREATE VIEW legs AS SELECT calls.id AS call_id, abs(boom.x) AS write_error, 0 AS no_audio FROM calls, boom`)

		_, _, err := s.ListCalls(context.Background(), ListFilter{})
		wantErr(t, err, "list calls rows")
	})
}

func TestListCalls_LimitClampedAndPaged(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)

	for i := range 3 {
		recordSimpleLeg(t, s, fmt.Sprintf("dlg-page-%d", i), base.Add(time.Duration(i)*time.Minute))
	}

	page, total, err := s.ListCalls(ctx, ListFilter{Limit: 1, Offset: 1})
	if err != nil || total != 3 || len(page) != 1 || page[0].SipCallID != "dlg-page-1" {
		t.Fatalf("page = %+v total=%d err=%v", page, total, err)
	}

	all, _, err := s.ListCalls(ctx, ListFilter{Limit: 10_000})
	if err != nil || len(all) != 3 || all[0].SipCallID != "dlg-page-2" {
		t.Fatalf("clamped list = %+v err=%v", all, err)
	}
}

func TestGetCall_Paths(t *testing.T) {
	ctx := context.Background()

	t.Run("absent", func(t *testing.T) {
		s := openTestStore(t)

		c, err := s.GetCall(ctx, 404)
		if c != nil || err != nil {
			t.Fatalf("GetCall(404) = %+v, %v; want nil, nil", c, err)
		}
	})

	t.Run("legs query", func(t *testing.T) {
		s := openTestStore(t)
		id := recordSimpleLeg(t, s, "dlg-lq", time.Now())

		denyWhen(t, s, func(op int, arg1, arg2 string) bool {
			return op == sqlite3.SQLITE_READ && arg1 == "legs" && arg2 == "label"
		})

		_, err := s.GetCall(ctx, id)
		wantErr(t, err, fmt.Sprintf("list legs %d", id))
	})

	t.Run("leg scan", func(t *testing.T) {
		s := openTestStore(t)
		id := recordSimpleLeg(t, s, "dlg-ls", time.Now())
		mustExec(t, s, `UPDATE legs SET created_at = 'not-a-number'`)

		_, err := s.GetCall(ctx, id)
		wantErr(t, err, "scan leg")
	})

	t.Run("legs rows", func(t *testing.T) {
		s := openTestStore(t)
		id := recordSimpleLeg(t, s, "dlg-lr", time.Now())
		addBoom(t, s)
		mustExec(t, s, `ALTER TABLE legs RENAME TO legs_real`)
		mustExec(t, s, `CREATE VIEW legs AS SELECT legs_real.id, call_id, label, codec, participant_aor,
			wav_path, duration_sec, write_error, no_audio, abs(boom.x) AS created_at FROM legs_real, boom`)

		_, err := s.GetCall(ctx, id)
		wantErr(t, err, "legs rows")
	})
}

func TestGetLeg_FoundAndAbsent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	callID := recordSimpleLeg(t, s, "dlg-leg", time.Now())

	c, err := s.GetCall(ctx, callID)
	if err != nil || c == nil || len(c.Legs) != 1 {
		t.Fatalf("GetCall = %+v, %v", c, err)
	}

	l, err := s.GetLeg(ctx, c.Legs[0].ID)
	if err != nil || l == nil || l.CallID != callID || l.WAVPath != "/rec/dlg-leg.wav" ||
		l.Label != "mixed" || l.CreatedAt.IsZero() {
		t.Fatalf("GetLeg = %+v, %v", l, err)
	}

	l, err = s.GetLeg(ctx, c.Legs[0].ID+1000)
	if l != nil || err != nil {
		t.Fatalf("GetLeg(absent) = %+v, %v; want nil, nil", l, err)
	}
}

func TestDeleteCall_AbsentAndDeleteFails(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	paths, err := s.DeleteCall(ctx, 404)
	if paths != nil || err != nil {
		t.Fatalf("DeleteCall(absent) = %v, %v; want nil, nil", paths, err)
	}

	id := recordSimpleLeg(t, s, "dlg-del", time.Now())
	mustExec(t, s, `CREATE TRIGGER block_call_delete BEFORE DELETE ON calls
		BEGIN SELECT RAISE(ABORT, 'call delete blocked'); END`)

	_, err = s.DeleteCall(ctx, id)
	wantErr(t, err, fmt.Sprintf("delete call %d", id))

	if c, _ := s.GetCall(ctx, id); c == nil {
		t.Error("call vanished despite the failed delete")
	}
}

func TestUsers_DisabledAbsentAndIdentity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if u, err := s.GetUser(ctx, "nobody@example.com"); u != nil || err != nil {
		t.Fatalf("GetUser(absent) = %+v, %v", u, err)
	}

	if err := s.UpsertUser(ctx, User{Email: "Off@Example.com", Role: "superuser", Disabled: true}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	u, err := s.GetUser(ctx, "off@example.com")
	if err != nil || u == nil || !u.Disabled || u.Role != "user" {
		t.Fatalf("GetUser = %+v, %v; want disabled with unknown role coerced to user", u, err)
	}

	pinned, err := s.SetGoogleSubIfEmpty(ctx, "OFF@example.com", "sub-1")
	if err != nil || !pinned {
		t.Fatalf("first pin = %v, %v", pinned, err)
	}

	pinned, err = s.SetGoogleSubIfEmpty(ctx, "off@example.com", "sub-2")
	if err != nil || pinned {
		t.Fatalf("second pin = %v, %v; want refused", pinned, err)
	}

	before, _ := s.GetUser(ctx, "off@example.com")

	if err := s.BumpSessionEpoch(ctx, "off@example.com"); err != nil {
		t.Fatalf("BumpSessionEpoch: %v", err)
	}

	u, _ = s.GetUser(ctx, "off@example.com")
	if u.GoogleSub != "sub-1" || u.SessionEpoch != before.SessionEpoch+1 {
		t.Errorf("identity = sub %q epoch %d, want sub-1 / %d", u.GoogleSub, u.SessionEpoch, before.SessionEpoch+1)
	}

	if deleted, err := s.DeleteUser(ctx, "nobody@example.com"); deleted || err != nil {
		t.Errorf("DeleteUser(absent) = %v, %v", deleted, err)
	}
}

func TestListFunctions_ScanAndRowsFailures(t *testing.T) {
	ctx := context.Background()

	type tableCase struct {
		name    string
		seed    func(t *testing.T, s *Store)
		table   string
		view    string // replacement view over <table>_real and boom
		list    func(s *Store) error
		scanErr string
		rowsErr string
	}

	cases := []tableCase{
		{
			name: "users",
			seed: func(t *testing.T, s *Store) {
				if err := s.UpsertUser(ctx, User{Email: "u@example.com"}); err != nil {
					t.Fatal(err)
				}
			},
			table: "users",
			view: `SELECT email, role, created_by, abs(boom.x) AS created_at, disabled,
				google_sub, session_epoch FROM users_real, boom`,
			list:    func(s *Store) error { _, err := s.ListUsers(ctx); return err },
			scanErr: "scan user", rowsErr: "list users rows",
		},
		{
			name: "voiceml accounts",
			seed: func(t *testing.T, s *Store) {
				if err := s.UpsertVoiceMLAccount(ctx, VoiceMLAccount{AccountSid: "AC1", APIKey: "k"}); err != nil {
					t.Fatal(err)
				}
			},
			table: "voiceml_accounts",
			view: `SELECT account_sid, api_key, created_by, abs(boom.x) AS created_at, updated_at
				FROM voiceml_accounts_real, boom`,
			list:    func(s *Store) error { _, err := s.ListVoiceMLAccounts(ctx); return err },
			scanErr: "scan cb account", rowsErr: "list cb accounts rows",
		},
		{
			name: "audit",
			seed: func(t *testing.T, s *Store) {
				if err := s.Audit(ctx, "a", "act", "t", "d"); err != nil {
					t.Fatal(err)
				}
			},
			table: "audit_log",
			view: `SELECT audit_log_real.id, actor, action, target, detail, abs(boom.x) AS created_at
				FROM audit_log_real, boom`,
			list:    func(s *Store) error { _, err := s.ListAudit(ctx, 10_000); return err },
			scanErr: "scan audit", rowsErr: "list audit rows",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/scan", func(t *testing.T) {
			s := openTestStore(t)
			tc.seed(t, s)
			mustExec(t, s, `UPDATE `+tc.table+` SET created_at = 'not-a-number'`)
			wantErr(t, tc.list(s), tc.scanErr)
		})

		t.Run(tc.name+"/rows", func(t *testing.T) {
			s := openTestStore(t)
			tc.seed(t, s)
			addBoom(t, s)
			mustExec(t, s, `ALTER TABLE `+tc.table+` RENAME TO `+tc.table+`_real`)
			mustExec(t, s, `CREATE VIEW `+tc.table+` AS `+tc.view)
			wantErr(t, tc.list(s), tc.rowsErr)
		})
	}
}

func TestListAudit_DefaultLimit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i := range 3 {
		if err := s.Audit(ctx, "a", fmt.Sprintf("act-%d", i), "", ""); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := s.ListAudit(ctx, 0)
	if err != nil || len(entries) != 3 || entries[0].Action != "act-2" || entries[0].CreatedAt.IsZero() {
		t.Fatalf("ListAudit(0) = %+v, %v", entries, err)
	}
}

func TestAorUser_WithoutHost(t *testing.T) {
	for in, want := range map[string]string{
		"tel:+15551234":       "+15551234",
		" alice ":             "alice",
		"sip:bob@example.com": "bob",
		"carol@example.com":   "carol",
	} {
		if got := aorUser(in); got != want {
			t.Errorf("aorUser(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKnownWAVPaths_Failures(t *testing.T) {
	ctx := context.Background()

	t.Run("scan", func(t *testing.T) {
		s := openTestStore(t)
		mustExec(t, s, `DROP TABLE legs`)
		mustExec(t, s, `CREATE VIEW legs AS SELECT NULL AS wav_path`)

		_, err := s.KnownWAVPaths(ctx)
		wantErr(t, err, "scan wav path")
	})

	t.Run("rows", func(t *testing.T) {
		s := openTestStore(t)
		addBoom(t, s)
		mustExec(t, s, `DROP TABLE legs`)
		mustExec(t, s, `CREATE VIEW legs AS SELECT abs(x) AS wav_path FROM boom`)

		_, err := s.KnownWAVPaths(ctx)
		wantErr(t, err, "known wav paths rows")
	})
}

// A relative leg path is kept verbatim when it cannot be made
// absolute (the working directory no longer exists).
func TestKnownWAVPaths_KeepsUnresolvableRelativePath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := recordSimpleLeg(t, s, "dlg-rel", time.Now())
	mustExec(t, s, `INSERT INTO legs (call_id, label, wav_path, created_at) VALUES (?, 'b', 'relative.wav', 1)`, id)

	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Chdir(gone)

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	if _, err := filepath.Abs("relative.wav"); err == nil {
		t.Fatal("filepath.Abs succeeded in a removed working directory; precondition not met")
	}

	known, err := s.KnownWAVPaths(ctx)
	if err != nil {
		t.Fatalf("KnownWAVPaths: %v", err)
	}

	if !known["relative.wav"] || !known["/rec/dlg-rel.wav"] || len(known) != 2 {
		t.Errorf("known = %v, want the relative path verbatim plus the absolute one", known)
	}
}

func TestAdoptOrphan_AlreadyAdoptedAndLegFailure(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	recordSimpleLeg(t, s, "dlg-taken", time.Now())

	if err := s.AdoptOrphan(ctx, "dlg-taken", "x", "/rec/other.wav", time.Now(), 3); err != nil {
		t.Fatalf("AdoptOrphan(existing call): %v", err)
	}

	known, _ := s.KnownWAVPaths(ctx)
	if known["/rec/other.wav"] {
		t.Error("orphan leg attached to an already-existing call")
	}

	mustExec(t, s, `CREATE TRIGGER block_leg_insert BEFORE INSERT ON legs
		BEGIN SELECT RAISE(ABORT, 'leg insert blocked'); END`)
	wantErr(t, s.AdoptOrphan(ctx, "dlg-new", "x", "/rec/new.wav", time.Now(), 3), "adopt orphan leg")
}

func TestRecoverOrphans_SkipsAndFailures(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.RecoverOrphans(ctx, filepath.Join(t.TempDir(), "missing")); err == nil ||
		!strings.Contains(err.Error(), "read recording dir") {
		t.Fatalf("missing dir err = %v", err)
	}

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir.wav"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	crashWAV(t, filepath.Join(dir, "dlg-blocked-a.wav"), 1)
	mustExec(t, s, `CREATE TRIGGER block_leg_insert BEFORE INSERT ON legs
		BEGIN SELECT RAISE(ABORT, 'leg insert blocked'); END`)

	n, err := s.RecoverOrphans(ctx, dir)
	if err != nil || n != 0 {
		t.Fatalf("RecoverOrphans = %d, %v; want 0 adopted when adoption fails", n, err)
	}

	known, _ := s.KnownWAVPaths(ctx)
	if len(known) != 0 {
		t.Errorf("known paths = %v, want none", known)
	}
}

func TestParseOrphanFilename_NoLabel(t *testing.T) {
	for name, want := range map[string][2]string{
		"plain.wav":         {"plain", "recovered"},
		"trailing-.wav":     {"trailing-", "recovered"},
		"a_at_b.c-left.WAV": {"a@b.c", "left"},
	} {
		callID, label := parseOrphanFilename(name)
		if callID != want[0] || label != want[1] {
			t.Errorf("parseOrphanFilename(%q) = %q, %q; want %q, %q", name, callID, label, want[0], want[1])
		}
	}
}

func TestDeleteCallsStartedBefore_Failures(t *testing.T) {
	ctx := context.Background()
	old := time.Now().AddDate(0, 0, -10)

	t.Run("scan", func(t *testing.T) {
		s := openTestStore(t)
		recordSimpleLeg(t, s, "dlg-old", old)
		mustExec(t, s, `DROP TABLE legs`)
		mustExec(t, s, `CREATE VIEW legs AS SELECT id AS call_id, NULL AS wav_path FROM calls`)

		_, _, err := s.DeleteCallsStartedBefore(ctx, time.Now())
		wantErr(t, err, "retention scan path")
	})

	t.Run("rows", func(t *testing.T) {
		s := openTestStore(t)
		recordSimpleLeg(t, s, "dlg-old", old)
		addBoom(t, s)
		mustExec(t, s, `DROP TABLE legs`)
		mustExec(t, s, `CREATE VIEW legs AS SELECT calls.id AS call_id, abs(boom.x) AS wav_path FROM calls, boom`)

		_, _, err := s.DeleteCallsStartedBefore(ctx, time.Now())
		wantErr(t, err, "retention paths rows")
	})

	t.Run("delete", func(t *testing.T) {
		s := openTestStore(t)
		recordSimpleLeg(t, s, "dlg-old", old)
		mustExec(t, s, `CREATE TRIGGER block_call_delete BEFORE DELETE ON calls
			BEGIN SELECT RAISE(ABORT, 'call delete blocked'); END`)

		_, _, err := s.DeleteCallsStartedBefore(ctx, time.Now())
		wantErr(t, err, "retention delete")

		if _, total, _ := s.ListCalls(ctx, ListFilter{}); total != 1 {
			t.Errorf("calls after failed delete = %d, want 1", total)
		}
	})
}

func TestSettings_AbsentAndNegativeRetention(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if v, ok, err := s.GetSetting(ctx, "never-set"); v != "" || ok || err != nil {
		t.Fatalf("GetSetting(absent) = %q, %v, %v", v, ok, err)
	}

	if at, calls, files := s.LastPurge(ctx); !at.IsZero() || calls != 0 || files != 0 {
		t.Errorf("LastPurge before any run = %v, %d, %d", at, calls, files)
	}

	if err := s.SetSetting(ctx, SettingRetentionDays, "-5"); err != nil {
		t.Fatal(err)
	}

	if days, fromUI, err := s.EffectiveRetentionDays(ctx, 30); days != 30 || fromUI || err != nil {
		t.Errorf("negative stored retention = %d, %v, %v; want env default", days, fromUI, err)
	}

	if err := s.SetSetting(ctx, SettingLastPurgeAt, "garbage"); err != nil {
		t.Fatal(err)
	}

	if at, _, _ := s.LastPurge(ctx); !at.IsZero() {
		t.Errorf("unparseable last purge time = %v, want zero", at)
	}
}

func TestVacuumIfNeeded_LargeDatabase(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mustExec(t, s, `CREATE TABLE filler (b BLOB)`)
	mustExec(t, s, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1500)
		INSERT INTO filler SELECT zeroblob(4000) FROM n`)

	if ran, err := s.VacuumIfNeeded(ctx, 0.2); ran || err != nil {
		t.Fatalf("VacuumIfNeeded with no free pages = %v, %v", ran, err)
	}

	mustExec(t, s, `DELETE FROM filler`)

	before, err := s.DBStats(ctx)
	if err != nil || before.PageCount < 1000 || before.Freelist == 0 {
		t.Fatalf("precondition: stats = %+v, %v; want a large, mostly free database", before, err)
	}

	if ran, err := s.VacuumIfNeeded(ctx, 1.0); ran || err != nil {
		t.Fatalf("VacuumIfNeeded below ratio = %v, %v", ran, err)
	}

	mustExec(t, s, `BEGIN`)

	ran, err := s.VacuumIfNeeded(ctx, 0.2)
	if ran {
		t.Error("VACUUM reported success inside a transaction")
	}

	wantErr(t, err, "store: vacuum")
	mustExec(t, s, `ROLLBACK`)

	if ran, err := s.VacuumIfNeeded(ctx, 0.2); !ran || err != nil {
		t.Fatalf("VacuumIfNeeded = %v, %v; want a vacuum", ran, err)
	}

	after, err := s.DBStats(ctx)
	if err != nil || after.Freelist != 0 || after.PageCount >= before.PageCount {
		t.Errorf("after vacuum stats = %+v (before %+v), %v", after, before, err)
	}
}
