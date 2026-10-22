package siprecsrv

import (
	"fmt"
	"strings"
	"testing"

	"github.com/emiago/sipgo/sip"
)

// openDialog INVITEs one recording and returns the SRS's To-tag.
func openDialog(t *testing.T, srv *Server, callID string, extra ...sip.Header) *sip.Response {
	t.Helper()

	req := covRequest(sip.INVITE, callID, covFrom, "127.0.0.1:5099", covSDP("40000 RTP/AVP 0\r\na=label:1\r\na=sendonly"), "application/sdp")
	for _, h := range extra {
		req.AppendHeader(h)
	}

	tx := newRecordingTx()
	srv.onInvite(req, tx)

	res := tx.only(t)
	if res.StatusCode != sip.StatusOK {
		t.Fatalf("INVITE = %d %s", res.StatusCode, res.Reason)
	}

	return res
}

// inDialogReq builds request method in dialog callID with the SRS's tag.
func inDialogReq(method sip.RequestMethod, callID, tag string, cseq int, body []byte, ct string, extra ...sip.Header) *sip.Request {
	req := covRequest(method, callID, covFrom, "127.0.0.1:5099", body, ct)
	req.RemoveHeader("To")
	req.AppendHeader(sip.NewHeader("To", "<sip:srs@127.0.0.1:5070>;tag="+tag))
	req.RemoveHeader("CSeq")
	req.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("%d %s", cseq, method)))

	for _, h := range extra {
		req.AppendHeader(h)
	}

	return req
}

func toTag(res *sip.Response) string {
	tag, _ := res.To().Params.Get("tag")

	return tag
}

func respond(t *testing.T, handler func(*sip.Request, sip.ServerTransaction), req *sip.Request) *sip.Response {
	t.Helper()

	tx := newRecordingTx()
	handler(req, tx)

	return tx.only(t)
}

func header(res *sip.Response, name string) string {
	if h := res.GetHeader(name); h != nil {
		return h.Value()
	}

	return ""
}

// A session-timer refresh by UPDATE (RFC 4028) is answered 200 with the
// interval echoed and the SRC left as refresher; sipgo used to answer
// UPDATE 405, which tears the dialog down at expiry.
func TestUpdate_SessionRefresh(t *testing.T) {
	srv, _ := newCovServer(t, 47100, 47104)

	inv := openDialog(t, srv, "dlg-timer", sip.NewHeader("Session-Expires", "1800"), sip.NewHeader("Supported", "timer, 100rel"))
	if got := header(inv, "Session-Expires"); got != "1800;refresher=uac" {
		t.Errorf("INVITE 200 Session-Expires = %q", got)
	}

	res := respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-timer", toTag(inv), 2, nil, "",
		sip.NewHeader("Session-Expires", "1800;refresher=uac"), sip.NewHeader("Supported", "timer")))
	if res.StatusCode != sip.StatusOK || header(res, "Session-Expires") != "1800;refresher=uac" ||
		header(res, "Require") != "timer" {
		t.Fatalf("refresh UPDATE = %d, Session-Expires %q Require %q", res.StatusCode,
			header(res, "Session-Expires"), header(res, "Require"))
	}

	// No Supported: timer, no Require: timer.
	res = respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-timer", toTag(inv), 3, nil, "",
		sip.NewHeader("Session-Expires", "600")))
	if res.StatusCode != sip.StatusOK || header(res, "Require") != "" {
		t.Errorf("UPDATE without timer support = %d, Require %q", res.StatusCode, header(res, "Require"))
	}

	// Below the 90 s floor: 422 with Min-SE; garbage: 400.
	res = respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-timer", toTag(inv), 4, nil, "",
		sip.NewHeader("Session-Expires", "30")))
	if res.StatusCode != 422 || header(res, "Min-SE") != "90" {
		t.Errorf("Session-Expires 30 = %d, Min-SE %q", res.StatusCode, header(res, "Min-SE"))
	}

	bad := covRequest(sip.INVITE, "dlg-bad-se2", covFrom, "", covSDP("40000 RTP/AVP 0"), "application/sdp")
	bad.AppendHeader(sip.NewHeader("Session-Expires", "soon"))

	if res := respond(t, srv.onInvite, bad); res.StatusCode != sip.StatusBadRequest {
		t.Errorf("Session-Expires soon = %d, want 400", res.StatusCode)
	}
}

// UPDATE can carry SDP that keeps the media shape (pause/resume) or
// RS-Metadata (RFC 7866 §9.2); a media-shape change needs a re-INVITE.
func TestUpdate_SDPAndMetadata(t *testing.T) {
	srv, _ := newCovServer(t, 47110, 47114)

	inv := openDialog(t, srv, "dlg-upd")
	tag := toTag(inv)

	res := respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-upd", tag, 2,
		covSDP("40000 RTP/AVP 0\r\na=label:1\r\na=inactive"), "application/sdp"))
	if res.StatusCode != sip.StatusOK || !strings.Contains(string(res.Body()), "a=inactive") {
		t.Fatalf("pause UPDATE = %d\n%s", res.StatusCode, res.Body())
	}

	if s := srv.SessionsSnapshot(); len(s) != 1 || !s[0].Paused {
		t.Errorf("session not paused by UPDATE: %+v", s)
	}

	// A re-INVITE without SDP is re-offered the current SDP.
	res = respond(t, srv.onInvite, inDialogReq(sip.INVITE, "dlg-upd", tag, 3, nil, ""))
	if res.StatusCode != sip.StatusOK || !strings.Contains(string(res.Body()), "a=inactive") ||
		header(res, "Content-Type") != "application/sdp" {
		t.Errorf("SDP-less re-INVITE = %d %q\n%s", res.StatusCode, header(res, "Content-Type"), res.Body())
	}

	meta := `<?xml version="1.0" encoding="UTF-8"?><recording xmlns="urn:ietf:params:xml:ns:recording:1"><datamode>partial</datamode></recording>`

	res = respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-upd", tag, 4, []byte(meta), "application/rs-metadata+xml"))
	if res.StatusCode != sip.StatusOK || len(res.Body()) != 0 {
		t.Errorf("metadata UPDATE = %d", res.StatusCode)
	}

	srv.shared.mu.Lock()
	gotXML := srv.shared.sessions["dlg-upd"].metadataXML
	srv.shared.mu.Unlock()

	if gotXML != meta {
		t.Errorf("metadata not replaced by UPDATE: %q", gotXML)
	}

	for name, tc := range map[string]struct {
		body []byte
		ct   string
		want int
	}{
		"media change": {covSDP("40002 RTP/AVP 0\r\na=label:1"), "application/sdp", sip.StatusNotAcceptableHere},
		"bad SDP":      {[]byte("v=0\r\n"), "application/sdp", sip.StatusBadRequest},
		"bad body":     {[]byte("x"), "multipart/mixed", sip.StatusBadRequest},
	} {
		if res := respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-upd", tag, 10, tc.body, tc.ct)); res.StatusCode != tc.want {
			t.Errorf("%s UPDATE = %d, want %d", name, res.StatusCode, tc.want)
		}
	}
}

// UPDATE is in-dialog only: unknown or foreign dialogs get 481, a stale
// CSeq 500, a dialog still being set up 491.
func TestUpdate_DialogRules(t *testing.T) {
	srv, _ := newCovServer(t, 47120, 47124)

	inv := openDialog(t, srv, "dlg-rules")
	tag := toTag(inv)

	if res := respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-nobody", tag, 2, nil, "")); res.StatusCode != 481 {
		t.Errorf("unknown dialog = %d, want 481", res.StatusCode)
	}

	if res := respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-rules", "wrong-tag", 2, nil, "")); res.StatusCode != 481 {
		t.Errorf("foreign To-tag = %d, want 481", res.StatusCode)
	}

	if res := respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-rules", tag, 1, nil, "")); res.StatusCode != 500 {
		t.Errorf("stale CSeq = %d, want 500", res.StatusCode)
	}

	srv.shared.sessions["dlg-pend"] = &session{callID: "dlg-pend", pending: true}
	if res := respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-pend", "", 2, nil, "")); res.StatusCode != sip.StatusRequestPending {
		t.Errorf("pending dialog = %d, want 491", res.StatusCode)
	}
	delete(srv.shared.sessions, "dlg-pend")

	noID := inDialogReq(sip.UPDATE, "dlg-rules", tag, 2, nil, "")
	noID.RemoveHeader("Call-ID")

	if res := respond(t, srv.onUpdate, noID); res.StatusCode != sip.StatusBadRequest {
		t.Errorf("no Call-ID = %d, want 400", res.StatusCode)
	}

	if res := respond(t, srv.onInvite, inDialogReq(sip.INVITE, "dlg-gone", tag, 2, nil, "")); res.StatusCode != 481 {
		t.Errorf("SDP-less re-INVITE of an unknown dialog = %d, want 481", res.StatusCode)
	}

	srv.Shutdown()

	if res := respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-rules", tag, 5, nil, "")); res.StatusCode != sip.StatusServiceUnavailable {
		t.Errorf("UPDATE while stopping = %d, want 503", res.StatusCode)
	}
}

func TestOptionsAdvertisesUpdateAndTimer(t *testing.T) {
	srv, _ := newCovServer(t, 47130, 47130)

	res := respond(t, srv.onOptions, covRequest(sip.OPTIONS, "opt-1", covFrom, "", nil, ""))
	if !strings.Contains(header(res, "Allow"), "UPDATE") || header(res, "Supported") != "timer" {
		t.Errorf("OPTIONS Allow %q Supported %q", header(res, "Allow"), header(res, "Supported"))
	}
}
