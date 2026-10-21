package store

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()

	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

// modSiprecMetadata mirrors what mod_siprec (recording_session.c) puts
// on the wire: group + session with the parent call UUID base64'd into
// session_id, two participants (caller, callee), one mixed stream.
func modSiprecMetadata(t *testing.T, fsUUID string) (*siprecsrv.Recording, string) {
	t.Helper()

	raw, err := hex.DecodeString(strings.ReplaceAll(fsUUID, "-", ""))
	if err != nil || len(raw) != 16 {
		t.Fatalf("test uuid %q: %v", fsUUID, err)
	}

	sid := base64.StdEncoding.EncodeToString(raw)
	xmlBody := `<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>complete</datamode>
  <group group_id="R3JvdXA=">
    <associate-time>2026-05-05T12:00:00Z</associate-time>
  </group>
  <session session_id="` + sid + `">
    <group-ref>R3JvdXA=</group-ref>
    <start-time>2026-05-05T12:00:00Z</start-time>
  </session>
  <participant participant_id="UGFydDE=">
    <nameID aor="sip:+12165285911@fs.example.com"/>
  </participant>
  <participant participant_id="UGFydDI=">
    <nameID aor="sip:+12085179911@fs.example.com"/>
  </participant>
  <stream stream_id="U3RyZWFt" session_id="` + sid + `">
    <label>1</label>
  </stream>
  <participantsessionassoc participant_id="UGFydDE=" session_id="` + sid + `">
    <associate-time>2026-05-05T12:00:00Z</associate-time>
  </participantsessionassoc>
  <participantstreamassoc participant_id="UGFydDE=">
    <send>U3RyZWFt</send>
  </participantstreamassoc>
</recording>`

	rec, err := siprecsrv.ParseMetadata(xmlBody)
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
	}

	return rec, xmlBody
}

func TestRecordLeg_TwoLegsOneCall(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	fsUUID := "123e4567-e89b-12d3-a456-426614174000"
	rec, xmlBody := modSiprecMetadata(t, fsUUID)

	base := siprecsrv.FinalizedLeg{
		CallID:      "siprec-dialog-1@fs.example.com",
		CallSid:     "CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		FsUUID:      fsUUID,
		StartedAt:   time.Now().UTC().Add(-time.Minute),
		Metadata:    rec,
		MetadataXML: xmlBody,
	}

	for _, leg := range []struct {
		label, wav string
		dur        int
	}{
		{"12165285911", "/rec/a-12165285911.wav", 30},
		{"12085179911", "/rec/a-12085179911.wav", 30},
	} {
		l := base
		l.Label, l.WAVPath, l.DurationSec = leg.label, leg.wav, leg.dur

		if err := s.RecordLeg(ctx, l); err != nil {
			t.Fatalf("RecordLeg %s: %v", leg.label, err)
		}
	}

	calls, total, err := s.ListCalls(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}

	if total != 1 || len(calls) != 1 {
		t.Fatalf("calls = %d (total %d), want 1", len(calls), total)
	}

	c := calls[0]
	if c.FromNumber != "+12165285911" || c.ToNumber != "+12085179911" {
		t.Errorf("from/to = %q/%q", c.FromNumber, c.ToNumber)
	}

	if c.Status != "completed" || c.LegCount != 2 || c.FsUUID != fsUUID {
		t.Errorf("status/legs/fs_uuid = %q/%d/%q", c.Status, c.LegCount, c.FsUUID)
	}

	got, err := s.GetCall(ctx, c.ID)
	if err != nil || got == nil || len(got.Legs) != 2 {
		t.Fatalf("GetCall: %+v, %v", got, err)
	}
}

func TestApplyAMD_AfterRecordingUpdates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec, _ := modSiprecMetadata(t, "123e4567-e89b-12d3-a456-426614174000")
	err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-2", FsUUID: "123e4567-e89b-12d3-a456-426614174000",
		WAVPath: "/rec/b.wav", DurationSec: 10, Metadata: rec,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	id, matched, err := s.ApplyAMD(ctx, AMDNotification{
		SipCallID: "dlg-2", AnsweredBy: "machine_start", DurationMs: 2400,
		CallStatus: "completed",
	})
	if err != nil || !matched {
		t.Fatalf("ApplyAMD: id=%d matched=%v err=%v", id, matched, err)
	}

	call, _ := s.GetCall(ctx, id)
	if call.AMDAnsweredBy != "machine_start" || call.AMDDurationMs != 2400 {
		t.Errorf("amd = %q/%d", call.AMDAnsweredBy, call.AMDDurationMs)
	}
}

func TestApplyAMD_BeforeRecordingMergesByCallSid(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// AMD lands first (placeholder keyed by CallSid).
	amdID, matched, err := s.ApplyAMD(ctx, AMDNotification{
		CallSid: "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", AnsweredBy: "human",
		From: "+12165285911", To: "+12085179911",
	})
	if err != nil || matched {
		t.Fatalf("ApplyAMD placeholder: id=%d matched=%v err=%v", amdID, matched, err)
	}

	// Recording arrives carrying the same CallSid via cb-callsid.
	rec, _ := modSiprecMetadata(t, "123e4567-e89b-12d3-a456-426614174001")
	err = s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-3", CallSid: "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		FsUUID:  "123e4567-e89b-12d3-a456-426614174001",
		WAVPath: "/rec/c.wav", DurationSec: 5, Metadata: rec,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	calls, total, _ := s.ListCalls(ctx, ListFilter{})
	if total != 1 || len(calls) != 1 {
		t.Fatalf("after merge calls = %d, want 1 (placeholder must be adopted)", total)
	}

	if calls[0].AMDAnsweredBy != "human" || calls[0].Status != "completed" {
		t.Errorf("merged amd/status = %q/%q", calls[0].AMDAnsweredBy, calls[0].Status)
	}
}

func TestApplyAMD_NoKeyRejected(t *testing.T) {
	s := openTestStore(t)

	if _, _, err := s.ApplyAMD(context.Background(), AMDNotification{AnsweredBy: "human"}); err == nil {
		t.Fatal("expected ErrNoKey")
	}
}

func TestUsersCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertUser(ctx, User{Email: " Person@Example.com ", Role: "user",
		CreatedBy: "admin@example.com", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	u, err := s.GetUser(ctx, "person@example.com")
	if err != nil || u == nil || u.Role != "user" {
		t.Fatalf("GetUser: %+v, %v", u, err)
	}

	if err := s.UpsertUser(ctx, User{Email: "person@example.com", Role: "admin"}); err != nil {
		t.Fatalf("UpsertUser role change: %v", err)
	}

	u, _ = s.GetUser(ctx, "person@example.com")
	if u.Role != "admin" {
		t.Errorf("role after upsert = %q", u.Role)
	}

	users, _ := s.ListUsers(ctx)
	if len(users) != 1 {
		t.Fatalf("ListUsers = %d", len(users))
	}

	deleted, _ := s.DeleteUser(ctx, "person@example.com")
	if !deleted {
		t.Error("DeleteUser reported false")
	}
}

func TestDeleteCallReturnsPaths(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec, _ := modSiprecMetadata(t, "123e4567-e89b-12d3-a456-426614174002")
	_ = s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-4", FsUUID: "123e4567-e89b-12d3-a456-426614174002",
		WAVPath: "/rec/d.wav", DurationSec: 3, Metadata: rec, StartedAt: time.Now().UTC(),
	})

	calls, _, _ := s.ListCalls(ctx, ListFilter{})
	paths, err := s.DeleteCall(ctx, calls[0].ID)
	if err != nil || len(paths) != 1 || paths[0] != "/rec/d.wav" {
		t.Fatalf("DeleteCall paths=%v err=%v", paths, err)
	}

	if _, total, _ := s.ListCalls(ctx, ListFilter{}); total != 0 {
		t.Errorf("calls after delete = %d", total)
	}
}
