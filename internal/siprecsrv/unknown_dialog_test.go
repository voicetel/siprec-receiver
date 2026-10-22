package siprecsrv

import (
	"testing"

	"github.com/emiago/sipgo/sip"
)

// An INVITE with a To-tag is in-dialog. One for a dialog this SRS does
// not have (it ended, or the process restarted) gets 481 instead of
// opening a fresh, metadata-less recording that the SRC's fork, still
// aimed at the old port, never feeds.
func TestReinviteForUnknownDialogIs481(t *testing.T) {
	srv, _ := newCovServer(t, 47060, 47062)

	body, ct := inviteBody("123e4567-e89b-12d3-a456-426614174000")
	req := covRequest(sip.INVITE, "cov-unknown-reinvite", covFrom, "127.0.0.1:5060", body, ct)
	req.RemoveHeader("To")
	req.AppendHeader(sip.NewHeader("To", "<sip:srs@127.0.0.1:5070>;tag=gone"))

	tx := newRecordingTx()
	srv.onInvite(req, tx)

	if code := tx.only(t).StatusCode; code != sip.StatusCallTransactionDoesNotExists {
		t.Fatalf("re-INVITE for an unknown dialog = %d, want 481", code)
	}

	if n := len(srv.SessionsSnapshot()); n != 0 {
		t.Errorf("sessions = %d, want none opened", n)
	}
}
