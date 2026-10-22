package store

import (
	"context"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

func countCalls(t *testing.T, s *Store) int {
	t.Helper()

	_, n, err := s.ListCalls(context.Background(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}

	return n
}

func seq(n int64) *int64 { return &n }

// An AMD webhook keyed by CallSid and a disposition keyed by FsUUID each
// made a placeholder; the recording carries both keys. Every row it names
// folds into one instead of the second claim failing UNIQUE(sip_call_id)
// and losing the recording's row.
func TestRecordLeg_FoldsEveryPlaceholderItNames(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const callSid, fsUUID = "CA0123456789abcdef0123456789abcdef", "01234567-89ab-cdef-0123-456789abcdef"

	if _, _, err := s.ApplyAMD(ctx, AMDNotification{CallSid: callSid, AnsweredBy: "machine_start"}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.ApplyDisposition(ctx, DispositionNotification{FsUUID: fsUUID, CallStatus: "completed", DurationSec: 42}); err != nil {
		t.Fatal(err)
	}

	if n := countCalls(t, s); n != 2 {
		t.Fatalf("placeholders = %d, want 2", n)
	}

	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-both", CallSid: callSid, FsUUID: fsUUID, WAVPath: "/rec/both.wav",
		DurationSec: 40, StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	calls, n, _ := s.ListCalls(ctx, ListFilter{})
	if n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}

	c := calls[0]
	if c.SipCallID != "dlg-both" || c.CallSid != callSid || c.FsUUID != fsUUID || c.Status != "completed" ||
		c.AMDAnsweredBy != "machine_start" || c.DispStatus != "completed" || c.DispDurationSec != 42 ||
		c.LegCount != 1 || c.DurationSec != 40 {
		t.Errorf("merged call = %+v", c)
	}
}

// A recording row without a CallSid and an AMD placeholder holding the
// CallSid are joined by a notification carrying both keys, instead of
// answering 500 on UNIQUE(sip_call_id) and leaving them split.
func TestNotify_JoinsRecordingAndPlaceholder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	recID := recordSimpleLeg(t, s, "dlg-split", time.Now())

	const callSid = "CAfedcba9876543210fedcba9876543210"

	if _, _, err := s.ApplyAMD(ctx, AMDNotification{CallSid: callSid, AnsweredBy: "human"}); err != nil {
		t.Fatal(err)
	}

	id, matched, err := s.ApplyDisposition(ctx, DispositionNotification{
		CallSid: callSid, SipCallID: "dlg-split", CallStatus: "completed",
	})
	if err != nil || !matched || id != recID {
		t.Fatalf("ApplyDisposition: id=%d matched=%v err=%v, want the recording row %d", id, matched, err, recID)
	}

	c, err := s.GetCall(ctx, recID)
	if err != nil || c.CallSid != callSid || c.AMDAnsweredBy != "human" || c.DispStatus != "completed" ||
		c.SipCallID != "dlg-split" || len(c.Legs) != 1 {
		t.Fatalf("joined call = %+v, %v", c, err)
	}

	if n := countCalls(t, s); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}

// A notification's SIP Call-ID is a lookup key, never stamped onto a row:
// VoiceML sends the carrier leg's Call-ID, and stamping it used to make
// the later recording (same CallSid, its own dialog) land on a second
// row without the CallSid.
func TestNotify_SipCallIDIsOnlyALookupKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const callSid = "CA00000000000000000000000000000077"

	if _, _, err := s.ApplyAMD(ctx, AMDNotification{CallSid: callSid, AnsweredBy: "human"}); err != nil {
		t.Fatal(err)
	}

	id, matched, err := s.ApplyDisposition(ctx, DispositionNotification{
		CallSid: callSid, SipCallID: "abc123@0.0.0.0", CallStatus: "failed",
	})
	if err != nil || !matched {
		t.Fatalf("ApplyDisposition: %v %v", matched, err)
	}

	if c, _ := s.GetCall(ctx, id); c.SipCallID != "" {
		t.Errorf("sip_call_id = %q, want it left for the recording", c.SipCallID)
	}

	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-real", CallSid: callSid, WAVPath: "/rec/real.wav", DurationSec: 3, StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	c, _ := s.GetCall(ctx, id)
	if countCalls(t, s) != 1 || c.SipCallID != "dlg-real" || c.CallSid != callSid || len(c.Legs) != 1 {
		t.Errorf("call = %+v, want the recording on the CallSid's row", c)
	}
}

// A placeholder born from a SipCallId-only notification is found by that
// dialog's recording, and a second placeholder for the same key reuses it.
func TestNotify_SipCallIDPlaceholderClaimedByRecording(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	pid, matched, err := s.ApplyAMD(ctx, AMDNotification{SipCallID: "dlg-first-amd", AnsweredBy: "fax"})
	if err != nil || matched {
		t.Fatalf("ApplyAMD: %v %v", matched, err)
	}

	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-first-amd", WAVPath: "/rec/f.wav", DurationSec: 2, StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	c, _ := s.GetCall(ctx, pid)
	if countCalls(t, s) != 1 || c.AMDAnsweredBy != "fax" || c.Status != "completed" {
		t.Errorf("call = %+v", c)
	}
}

// A row carrying a different CallSid is another call: an event naming it
// only through a shared FsUUID or dialog must not swallow it.
func TestResolve_LeavesRowsOfAnotherCallAlone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const fsUUID = "fs-shared"

	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-a", CallSid: "CA000000000000000000000000000000aa", FsUUID: fsUUID,
		WAVPath: "/rec/a.wav", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	id, matched, err := s.ApplyAMD(ctx, AMDNotification{
		CallSid: "CA000000000000000000000000000000bb", FsUUID: fsUUID, SipCallID: "dlg-a", AnsweredBy: "human",
	})
	if err != nil || matched {
		t.Fatalf("ApplyAMD: id=%d matched=%v err=%v, want a new row", id, matched, err)
	}

	c, _ := s.GetCall(ctx, id)
	if countCalls(t, s) != 2 || c.CallSid != "CA000000000000000000000000000000bb" || c.SipCallID != "" {
		t.Errorf("new row = %+v (its dialog key is held by the other call)", c)
	}

	// The same conflict on the recording side: a dialog whose Call-ID the
	// other call's row already holds still records, on a new row.
	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-a", CallSid: "CA000000000000000000000000000000cc", WAVPath: "/rec/c.wav", StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("RecordLeg with a conflicting CallSid: %v", err)
	}

	if n := countCalls(t, s); n != 3 {
		t.Errorf("calls = %d, want 3", n)
	}
}

// Merging follows the disposition ordering rules: the newer or terminal
// outcome wins whichever row survives, and empty fields fill.
func TestMerge_DispositionOrderAndFill(t *testing.T) {
	cases := []struct {
		name               string
		first, second      DispositionNotification
		want               string
		wantVoiceMLFetched bool
	}{
		// The dialog's row survives; the FsUUID row is folded into it.
		{"the folded row's later outcome wins",
			DispositionNotification{FsUUID: "fs-1", CallStatus: "completed", SequenceNumber: seq(3)},
			DispositionNotification{SipCallID: "dlg-m", CallStatus: "ringing", SequenceNumber: seq(1)},
			"completed", true},
		{"the survivor's terminal outcome is kept",
			DispositionNotification{FsUUID: "fs-1", CallStatus: "ringing", SequenceNumber: seq(1)},
			DispositionNotification{SipCallID: "dlg-m", CallStatus: "busy", SequenceNumber: seq(5)},
			"busy", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()

			first, _, err := s.ApplyDisposition(ctx, tc.first)
			if err != nil {
				t.Fatal(err)
			}

			if _, _, err := s.ApplyDisposition(ctx, tc.second); err != nil {
				t.Fatal(err)
			}

			if tc.wantVoiceMLFetched {
				// The fetched record sits on the row that gets folded.
				if err := s.SaveVoiceMLRecord(ctx, first, VoiceMLRecord{Status: "completed"}, `{"x":1}`); err != nil {
					t.Fatal(err)
				}
			}

			if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
				CallID: "dlg-m", FsUUID: "fs-1", WAVPath: "/rec/m.wav", StartedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}

			calls, n, _ := s.ListCalls(ctx, ListFilter{})
			if n != 1 || calls[0].DispStatus != tc.want {
				t.Fatalf("calls = %+v, want one with disposition %s", calls, tc.want)
			}

			if got := calls[0].VoiceMLRecordJSON != ""; got != tc.wantVoiceMLFetched {
				t.Errorf("fetched record kept = %v, want %v", got, tc.wantVoiceMLFetched)
			}
		})
	}
}

func TestMergedStart(t *testing.T) {
	rec := func(at int64) mergeRow { return mergeRow{status: "completed", startedAt: at} }
	note := func(at int64) mergeRow { return mergeRow{status: "notified", startedAt: at} }

	cases := []struct {
		name       string
		keep, drop mergeRow
		want       int64
	}{
		{"recording wins over an earlier webhook", rec(9), note(5), 9},
		{"folded recording wins over the survivor's webhook time", note(5), rec(9), 9},
		{"two recordings: earliest", rec(9), rec(7), 7},
		{"two placeholders: earliest", note(5), note(7), 5},
		{"survivor without a time", note(0), note(7), 7},
		{"folded row without a time", rec(9), rec(0), 9},
	}

	for _, tc := range cases {
		if got := mergedStart(tc.keep, tc.drop); got != tc.want {
			t.Errorf("%s: mergedStart = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A recording claiming an AMD placeholder takes the recording's start
// time, not the time the webhook arrived (which, for a terminal callback
// processed before the BYE, equals the end of the call).
func TestRecordLeg_ClaimedPlaceholderTakesRecordingStart(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const callSid = "CA0000000000000000000000000000ab12"

	id, _, err := s.ApplyAMD(ctx, AMDNotification{CallSid: callSid, AnsweredBy: "human"})
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now().Add(-90 * time.Second).UTC().Truncate(time.Millisecond)

	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-start", CallSid: callSid, WAVPath: "/rec/s.wav", DurationSec: 90, StartedAt: started,
	}); err != nil {
		t.Fatal(err)
	}

	c, _ := s.GetCall(ctx, id)
	if !c.StartedAt.Equal(started) {
		t.Errorf("started_at = %s, want the recording's start %s", c.StartedAt, started)
	}

	// A later leg (another dialog) that started earlier moves it back; one
	// that started later does not.
	earlier := started.Add(-time.Minute)
	for _, at := range []time.Time{started.Add(time.Minute), earlier} {
		if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
			CallID: "dlg-start-" + at.Format("150405.000"), CallSid: callSid,
			WAVPath: "/rec/" + at.Format("150405.000") + ".wav", StartedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if c, _ := s.GetCall(ctx, id); !c.StartedAt.Equal(earlier) {
		t.Errorf("started_at = %s, want the earliest dialog's %s", c.StartedAt, earlier)
	}
}

// Two recording rows of one call left split by an earlier build (same
// FsUUID, no CallSid) are joined by the next event naming them: legs
// move, the recorded time sums per dialog, and the row stays completed.
func TestResolve_JoinsLegacySplitRecordings(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i, dlg := range []string{"dlg-old-1", "dlg-old-2"} {
		if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
			CallID: dlg, WAVPath: "/rec/" + dlg + ".wav", DurationSec: 3 + i, StartedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	mustExec(t, s, `UPDATE calls SET fs_uuid = 'fs-legacy'`)

	id, matched, err := s.ApplyAMD(ctx, AMDNotification{FsUUID: "fs-legacy", AnsweredBy: "human"})
	if err != nil || !matched {
		t.Fatalf("ApplyAMD: %v %v", matched, err)
	}

	c, _ := s.GetCall(ctx, id)
	if countCalls(t, s) != 1 || len(c.Legs) != 2 || c.DurationSec != 7 || c.Status != "completed" ||
		c.SipCallID != "dlg-old-1" {
		t.Errorf("joined call = %+v", c)
	}
}

func TestMigrate_LegDialogsFail(t *testing.T) {
	s := openTestStore(t)

	denyWhen(t, s, func(op int, arg1, _ string) bool {
		return op == sqlite3.SQLITE_UPDATE && arg1 == "legs"
	})
	wantErr(t, s.migrate(context.Background()), "migrate leg dialogs")
}

func TestApplyDisposition_OrderLookupFails(t *testing.T) {
	s := openTestStore(t)
	recordSimpleLeg(t, s, "dlg-order", time.Now())

	denyWhen(t, s, func(op int, arg1, arg2 string) bool {
		return op == sqlite3.SQLITE_READ && arg1 == "calls" && arg2 == "disp_event_at"
	})

	_, _, err := s.ApplyDisposition(context.Background(), DispositionNotification{SipCallID: "dlg-order", CallStatus: "completed"})
	wantErr(t, err, "disposition order lookup")
}

// Legs recorded before legs carried their dialog are attributed to their
// call's dialog on upgrade.
func TestMigrate_BackfillsLegDialogs(t *testing.T) {
	s := openTestStore(t)
	recordSimpleLeg(t, s, "dlg-backfill", time.Now())
	mustExec(t, s, `UPDATE legs SET sip_call_id = ''`)

	if err := s.migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	var dlg string
	if err := s.db.QueryRow(`SELECT sip_call_id FROM legs`).Scan(&dlg); err != nil || dlg != "dlg-backfill" {
		t.Errorf("backfilled leg dialog = %q, %v", dlg, err)
	}
}
