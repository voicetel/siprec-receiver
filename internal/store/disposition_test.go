package store

import (
	"context"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

func TestApplyDisposition_AfterRecording(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-d1", CallSid: "CAd1111111111111111111111111111111",
		WAVPath: "/rec/d1.wav", DurationSec: 60, StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	id, matched, err := s.ApplyDisposition(ctx, DispositionNotification{
		CallSid: "CAd1111111111111111111111111111111", CallStatus: "completed",
		DurationSec: 62, SipCode: 200, AnsweredBy: "machine_end_beep",
	})
	if err != nil || !matched {
		t.Fatalf("ApplyDisposition: id=%d matched=%v err=%v", id, matched, err)
	}

	call, _ := s.GetCall(ctx, id)
	if call.DispStatus != "completed" || call.DispDurationSec != 62 || call.DispSipCode != 200 {
		t.Errorf("disp = %q/%d/%d", call.DispStatus, call.DispDurationSec, call.DispSipCode)
	}

	// The terminal callback's AnsweredBy fills the AMD side too.
	if call.AMDAnsweredBy != "machine_end_beep" {
		t.Errorf("amd answered_by = %q", call.AMDAnsweredBy)
	}

	// Recording duration (audio) is untouched by call duration.
	if call.DurationSec != 60 {
		t.Errorf("duration_sec = %d, want recording's 60", call.DurationSec)
	}
}

func TestApplyDisposition_BeforeRecordingAdopted(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, _, err := s.ApplyDisposition(ctx, DispositionNotification{
		CallSid: "CAd2222222222222222222222222222222", CallStatus: "busy",
		SipCode: 486, ErrorCode: "13224", ErrorMessage: "busy here",
	}); err != nil {
		t.Fatalf("ApplyDisposition placeholder: %v", err)
	}

	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-d2", CallSid: "CAd2222222222222222222222222222222",
		WAVPath: "/rec/d2.wav", DurationSec: 3, StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	calls, total, _ := s.ListCalls(ctx, ListFilter{})
	if total != 1 || len(calls) != 1 {
		t.Fatalf("calls after merge = %d, want 1", total)
	}

	if calls[0].DispStatus != "busy" || calls[0].DispErrorCode != "13224" ||
		calls[0].Status != "completed" {
		t.Errorf("merged disp/status = %q/%q err=%q",
			calls[0].DispStatus, calls[0].Status, calls[0].DispErrorCode)
	}
}

func TestSaveCBRecord_DenormalizesWithoutOverwriting(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-d3", CallSid: "CAd3333333333333333333333333333333",
		WAVPath: "/rec/d3.wav", DurationSec: 55, StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	calls, _, _ := s.ListCalls(ctx, ListFilter{})

	err := s.SaveCBRecord(ctx, calls[0].ID, CBRecord{
		Sid: "CAd3333333333333333333333333333333", AccountSid: "ACtest",
		From: "+12165285911", To: "+12085179911", Status: "completed",
		Direction: "outbound-api", AnsweredBy: "human", DurationSec: 57,
	}, "{\n  \"sid\": \"CAd333\"\n}")
	if err != nil {
		t.Fatalf("SaveCBRecord: %v", err)
	}

	call, _ := s.GetCall(ctx, calls[0].ID)
	if call.AccountSid != "ACtest" || call.FromNumber != "+12165285911" ||
		call.DispStatus != "completed" || call.DispDurationSec != 57 ||
		call.AMDAnsweredBy != "human" || call.CBRecordJSON == "" ||
		call.CBFetchedAt.IsZero() {
		t.Errorf("after save: acct=%q from=%q disp=%q/%d amd=%q json=%v fetched=%v",
			call.AccountSid, call.FromNumber, call.DispStatus,
			call.DispDurationSec, call.AMDAnsweredBy,
			call.CBRecordJSON != "", call.CBFetchedAt.IsZero())
	}

	if call.DurationSec != 55 {
		t.Errorf("recording duration overwritten: %d", call.DurationSec)
	}
}

func TestCBAccountsCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if a, _ := s.GetCBAccount(ctx, "ACtest"); a != nil {
		t.Fatal("unexpected account before upsert")
	}

	if err := s.UpsertCBAccount(ctx, CBAccount{
		AccountSid: "ACtest", APIKey: "key-1", CreatedBy: "admin@example.com",
	}); err != nil {
		t.Fatalf("UpsertCBAccount: %v", err)
	}

	a, _ := s.GetCBAccount(ctx, "ACtest")
	if a == nil || a.APIKey != "key-1" {
		t.Fatalf("GetCBAccount = %+v", a)
	}

	if err := s.UpsertCBAccount(ctx, CBAccount{AccountSid: "ACtest", APIKey: "key-2"}); err != nil {
		t.Fatalf("replace key: %v", err)
	}

	a, _ = s.GetCBAccount(ctx, "ACtest")
	if a.APIKey != "key-2" {
		t.Errorf("key after replace = %q", a.APIKey)
	}

	list, _ := s.ListCBAccounts(ctx)
	if len(list) != 1 {
		t.Fatalf("ListCBAccounts = %d", len(list))
	}

	deleted, _ := s.DeleteCBAccount(ctx, "ACtest")
	if !deleted {
		t.Error("DeleteCBAccount reported false")
	}
}
