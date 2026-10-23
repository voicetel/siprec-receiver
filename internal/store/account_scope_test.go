package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A notification limited to one account (signed with that account's
// key) touches only calls of that account or of none: naming another
// account's call is ErrForeignAccount and rolls back, merges included.
func TestAuthAccountLimitsWhichCallsANotificationTouches(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mine := recordSimpleLeg(t, s, "dlg-mine", time.Now())
	mustExec(t, s, `UPDATE calls SET call_sid = 'CA00000000000000000000000000000a01', account_sid = 'ACone' WHERE id = ?`, mine)

	// A placeholder of another account, named by FsUUID alongside the
	// first call's CallSid: the merge would fold it in.
	if _, _, err := s.ApplyDisposition(ctx, DispositionNotification{FsUUID: "fs-theirs", AccountSid: "ACtwo", CallStatus: "ringing"}); err != nil {
		t.Fatal(err)
	}

	_, _, err := s.ApplyAMD(ctx, AMDNotification{
		CallSid: "CA00000000000000000000000000000a01", FsUUID: "fs-theirs", AnsweredBy: "human", AuthAccount: "ACone",
	})
	if !errors.Is(err, ErrForeignAccount) {
		t.Fatalf("err = %v, want ErrForeignAccount", err)
	}

	if n := countCalls(t, s); n != 2 {
		t.Errorf("%d rows after the refused merge, want 2", n)
	}

	if _, _, err := s.ApplyDisposition(ctx, DispositionNotification{
		CallSid: "CA00000000000000000000000000000a01", CallStatus: "busy", AuthAccount: "ACtwo",
	}); !errors.Is(err, ErrForeignAccount) {
		t.Fatalf("disposition err = %v, want ErrForeignAccount", err)
	}

	if c, _ := s.GetCall(ctx, mine); c.AMDAnsweredBy != "" || c.DispStatus != "" {
		t.Errorf("refused notifications changed the call: %q %q", c.AMDAnsweredBy, c.DispStatus)
	}

	// Its own account's call, and a call no account owns yet (which it
	// then owns), are fine.
	if _, matched, err := s.ApplyAMD(ctx, AMDNotification{
		CallSid: "CA00000000000000000000000000000a01", AnsweredBy: "human", AuthAccount: "ACone",
	}); err != nil || !matched {
		t.Fatalf("own call = %v, %v", matched, err)
	}

	unowned := recordSimpleLeg(t, s, "dlg-unowned", time.Now())
	mustExec(t, s, `UPDATE calls SET call_sid = 'CA00000000000000000000000000000a02' WHERE id = ?`, unowned)

	if _, _, err := s.ApplyDisposition(ctx, DispositionNotification{
		CallSid: "CA00000000000000000000000000000a02", AccountSid: "ACtwo", CallStatus: "completed", AuthAccount: "ACtwo",
	}); err != nil {
		t.Fatal(err)
	}

	if c, _ := s.GetCall(ctx, unowned); c.AccountSid != "ACtwo" || c.DispStatus != "completed" {
		t.Errorf("unowned call = %q %q", c.AccountSid, c.DispStatus)
	}
}
