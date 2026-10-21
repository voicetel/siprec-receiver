package siprecsrv

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"github.com/voicetel/siprec-receiver/internal/rtp"
)

func TestValidCallID(t *testing.T) {
	valid := []string{
		"a",
		"test-call-1",
		"123e4567-e89b-12d3-a456-426614174000",
		"123e4567-e89b-12d3-a456-426614174000@fs.example.com",
		"a!#$%&'*+-.^_`|~@host",
		strings.Repeat("a", maxCallIDLength),
	}
	for _, id := range valid {
		if !validCallID(id) {
			t.Errorf("validCallID(%q) = false, want true", id)
		}
		if got, ok := requestCallID(requestWithCallID(id)); !ok || got != id {
			t.Errorf("requestCallID(%q) = %q, %v", id, got, ok)
		}
	}

	invalid := []string{
		"",
		" ",
		"bad call id",
		"bad\ncall-id",
		"bad\rcall-id",
		"bad\tcall-id",
		"bad\x7fcall-id",
		"callé",
		strings.Repeat("a", maxCallIDLength+1),
	}
	for _, id := range invalid {
		if validCallID(id) {
			t.Errorf("validCallID(%q) = true, want false", id)
		}
		if _, ok := requestCallID(requestWithCallID(id)); ok {
			t.Errorf("requestCallID(%q) accepted an invalid ID", id)
		}
	}

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "srs", Host: "127.0.0.1"})
	if _, ok := requestCallID(req); ok {
		t.Error("requestCallID accepted a missing Call-ID")
	}
	if got := callIDOf(req); got != "" {
		t.Errorf("callIDOf(missing) = %q, want empty (no sentinel)", got)
	}
}

func requestWithCallID(id string) *sip.Request {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "srs", Host: "127.0.0.1"})
	req.AppendHeader(sip.NewHeader("Call-ID", id))

	return req
}

// recordingServerTx captures handler responses without a network
// transaction (sipgo's client would generate a Call-ID for a request
// that lacks one, so missing-header cases are exercised directly).
type recordingServerTx struct {
	responses []*sip.Response
	done      chan struct{}
}

func newRecordingServerTx() *recordingServerTx {
	return &recordingServerTx{done: make(chan struct{})}
}

func (tx *recordingServerTx) Terminate() {}
func (tx *recordingServerTx) OnTerminate(sip.FnTxTerminate) bool {
	return true
}
func (tx *recordingServerTx) Done() <-chan struct{} { return tx.done }
func (tx *recordingServerTx) Err() error            { return nil }
func (tx *recordingServerTx) Respond(res *sip.Response) error {
	tx.responses = append(tx.responses, res)

	return nil
}
func (tx *recordingServerTx) Acks() <-chan *sip.Request { return nil }
func (tx *recordingServerTx) OnCancel(sip.FnTxCancel) bool {
	return false
}

func (tx *recordingServerTx) status(t *testing.T) int {
	t.Helper()

	if len(tx.responses) != 1 {
		t.Fatalf("handler responses = %d, want 1", len(tx.responses))
	}

	return tx.responses[0].StatusCode
}

func directRequest(method sip.RequestMethod, callID string, includeCallID bool, body []byte, contentType string) *sip.Request {
	req := sip.NewRequest(method, sip.Uri{
		User: "srs", Host: "127.0.0.1", Port: 5070,
		UriParams: sip.NewParams(),
	})
	if body != nil {
		req.SetBody(body)
		req.AppendHeader(sip.NewHeader("Content-Type", contentType))
	}
	req.AppendHeader(sip.NewHeader("From", "<sip:caller@example.com>;tag=tag-a"))
	req.AppendHeader(sip.NewHeader("To", "<sip:srs@127.0.0.1:5070>"))
	if includeCallID {
		req.AppendHeader(sip.NewHeader("Call-ID", callID))
	}
	req.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("1 %s", method)))
	req.SetSource("127.0.0.1:5060")

	return req
}

// Missing or malformed Call-IDs must never create or join a session:
// previously they collapsed onto the shared "unknown" key.
func TestCallID_MissingOrInvalidRejected(t *testing.T) {
	finalizer := &fakeFinalizer{}
	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(45200, 45300))
	srv.Finalizer = finalizer

	body, contentType := inviteBody("123e4567-e89b-12d3-a456-426614174000")

	for _, tc := range []struct {
		name    string
		callID  string
		include bool
	}{
		{"missing", "", false},
		{"space", "bad call id", true},
		{"too long", strings.Repeat("a", maxCallIDLength+1), true},
	} {
		tx := newRecordingServerTx()
		srv.onInvite(directRequest(sip.INVITE, tc.callID, tc.include, body, contentType), tx)

		if got := tx.status(t); got != sip.StatusBadRequest {
			t.Errorf("%s: INVITE status = %d, want 400", tc.name, got)
		}
	}

	if got := len(srv.SessionsSnapshot()); got != 0 {
		t.Fatalf("sessions after rejected INVITEs = %d, want 0", got)
	}

	// A real dialog, then malformed BYEs against it: both rejected,
	// neither finalizes the recording.
	addr := startTestServer(t, srv)
	_, portStr, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	c := newHardeningClient(t, port)
	const callID = "dlg-callid-guard"
	if res := c.send(sip.INVITE, 1, callID, "tag-a", body, contentType); res.StatusCode != sip.StatusOK {
		t.Fatalf("valid INVITE = %d, want 200", res.StatusCode)
	}

	for _, tc := range []struct {
		name    string
		callID  string
		include bool
	}{
		{"missing", "", false},
		{"space", "bad call id", true},
		{"too long", strings.Repeat("b", maxCallIDLength+1), true},
	} {
		tx := newRecordingServerTx()
		srv.onBye(directRequest(sip.BYE, tc.callID, tc.include, nil, ""), tx)

		if got := tx.status(t); got != sip.StatusBadRequest {
			t.Errorf("%s: BYE status = %d, want 400", tc.name, got)
		}
	}

	time.Sleep(50 * time.Millisecond)

	if legs := finalizer.snapshot(); len(legs) != 0 {
		t.Fatalf("malformed BYEs finalized %d legs", len(legs))
	}

	if got := len(srv.SessionsSnapshot()); got != 1 {
		t.Fatalf("sessions after malformed BYEs = %d, want 1", got)
	}

	if res := c.send(sip.BYE, 2, callID, "tag-a", nil, ""); res.StatusCode != sip.StatusOK {
		t.Fatalf("valid BYE = %d, want 200", res.StatusCode)
	}
}
