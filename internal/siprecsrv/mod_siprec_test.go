package siprecsrv

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/voicetel/siprec-receiver/internal/rtp"
)

// modSiprecMetadataXML is the shape mod_siprec's siprec_metadata.c
// emits: group, session (parent-call UUID base64'd into session_id),
// two participants, one mixed stream.
func modSiprecMetadataXML(fsUUID string) string {
	raw, _ := hex.DecodeString(strings.ReplaceAll(fsUUID, "-", ""))
	sid := base64.StdEncoding.EncodeToString(raw)

	return `<?xml version="1.0" encoding="UTF-8"?>
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
}

func TestParseMetadata_ModSiprecShape(t *testing.T) {
	const fsUUID = "123e4567-e89b-12d3-a456-426614174000"

	rec, err := ParseMetadata(modSiprecMetadataXML(fsUUID))
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
	}

	if len(rec.Groups) != 1 || rec.Groups[0].ID != "R3JvdXA=" {
		t.Errorf("groups = %+v", rec.Groups)
	}

	if len(rec.Sessions) != 1 || rec.Sessions[0].GroupRef != "R3JvdXA=" {
		t.Errorf("sessions = %+v", rec.Sessions)
	}

	if len(rec.PSessAssoc) != 1 {
		t.Errorf("participantsessionassoc = %d, want 1", len(rec.PSessAssoc))
	}

	got, ok := rec.FsUUID()
	if !ok || got != fsUUID {
		t.Errorf("FsUUID = %q, %v; want %q", got, ok, fsUUID)
	}

	aors := rec.ParticipantAORs()
	if len(aors) != 2 || aors[0] != "sip:+12165285911@fs.example.com" {
		t.Errorf("aors = %v", aors)
	}
}

func TestFsUUID_NonBase64SessionID(t *testing.T) {
	rec, _ := ParseMetadata(fsMetadataSample) // session_id "1234abcd"
	if _, ok := rec.FsUUID(); ok {
		t.Error("FsUUID must fail for a non-base64-UUID session_id")
	}
}

func buildModSiprecBody(fsUUID string) sipBody {
	const boundary = "mod-siprec-boundary"

	sdp := "v=0\r\n" +
		"o=FreeSWITCH 1746547200 1746547200 IN IP4 10.0.0.1\r\n" +
		"s=FreeSWITCH\r\n" +
		"c=IN IP4 10.0.0.1\r\n" +
		"t=0 0\r\n" +
		"m=audio 30000 RTP/AVP 0 8\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:8 PCMA/8000\r\n" +
		"a=label:1\r\n" +
		"a=sendonly\r\n"

	var b strings.Builder
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: application/sdp\r\n\r\n")
	b.WriteString(sdp)
	b.WriteString("\r\n--" + boundary + "\r\n")
	b.WriteString("Content-Type: application/rs-metadata+xml\r\n")
	b.WriteString("Content-Disposition: recording-session\r\n\r\n")
	b.WriteString(modSiprecMetadataXML(fsUUID))
	b.WriteString("\r\n--" + boundary + "--\r\n")

	return sipBody{body: []byte(b.String()), contentType: `multipart/mixed; boundary="` + boundary + `"`}
}

// TestServer_ModSiprecPauseResumeKeepsOneRecording drives the
// mod_siprec lifecycle: INVITE (mixed stream) -> pause re-INVITE
// (a=inactive, SDP only) -> resume re-INVITE (a=sendonly) -> BYE.
// The pause/resume must not finalize or split the recording (RFC 7866
// §6.4); exactly one Finalize for the one leg at BYE.
func TestServer_ModSiprecPauseResumeKeepsOneRecording(t *testing.T) {
	dir := t.TempDir()
	port := freeTCPPort(t)
	listenAddr := fmt.Sprintf("127.0.0.1:%d", port)

	pool := rtp.NewPortPool(43000, 43100)
	finalizer := &fakeFinalizer{}
	srv := New("127.0.0.1", dir, pool)
	srv.Finalizer = finalizer

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = srv.Serve(ctx, "tcp", listenAddr) }()
	waitForTCPListening(t, listenAddr)

	uac, err := sipgo.NewUA(sipgo.WithUserAgent("mod-siprec-sim"))
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	defer func() { _ = uac.Close() }()

	cli, err := sipgo.NewClient(uac)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = cli.Close() }()

	const fsUUID = "123e4567-e89b-12d3-a456-426614174000"
	const callID = "mod-siprec-test-1"

	target := sip.Uri{User: "srs", Host: "127.0.0.1", Port: port, UriParams: sip.NewParams()}
	target.UriParams.Add("transport", "tcp")
	target.UriParams.Add("vm-callsid", "CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	toTag := ""
	send := func(method sip.RequestMethod, cseq int, body []byte, ct string) int {
		req := sip.NewRequest(method, target)
		if body != nil {
			req.SetBody(body)
			req.AppendHeader(sip.NewHeader("Content-Type", ct))
		}
		req.AppendHeader(sip.NewHeader("From", "<sip:+12165285911@fs.example.com>;tag=src-tag"))
		req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:srs@127.0.0.1:%d>", port)))
		if toTag != "" {
			to := req.To()
			to.Params.Add("tag", toTag)
			req.RemoveHeader("To")
			req.AppendHeader(to)
		}
		req.AppendHeader(sip.NewHeader("Call-ID", callID))
		req.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("%d %s", cseq, method)))

		if method == sip.INVITE {
			req.AppendHeader(sip.NewHeader("Require", "siprec"))
		}

		tx, terr := cli.TransactionRequest(ctx, req)
		if terr != nil {
			t.Fatalf("%s TransactionRequest: %v", method, terr)
		}
		defer tx.Terminate()

		select {
		case res := <-tx.Responses():
			if method == sip.INVITE && res.StatusCode == 200 {
				toTag, _ = res.To().Params.Get("tag")
			}
			return res.StatusCode
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for %s response", method)

			return 0
		}
	}

	invite := buildModSiprecBody(fsUUID)
	if code := send(sip.INVITE, 1, invite.body, invite.contentType); code != sip.StatusOK {
		t.Fatalf("INVITE status = %d, want 200", code)
	}

	time.Sleep(50 * time.Millisecond)

	if got := len(srv.SessionsSnapshot()); got != 1 {
		t.Fatalf("sessions after INVITE = %d, want 1", got)
	}

	pauseSDP := "v=0\r\no=FreeSWITCH 1746547200 1746547201 IN IP4 10.0.0.1\r\n" +
		"s=FreeSWITCH\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n" +
		"m=audio 30000 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=label:1\r\na=inactive\r\n"
	if code := send(sip.INVITE, 2, []byte(pauseSDP), "application/sdp"); code != sip.StatusOK {
		t.Fatalf("pause re-INVITE status = %d, want 200", code)
	}

	resumeSDP := strings.ReplaceAll(pauseSDP, "a=inactive", "a=sendonly")
	if code := send(sip.INVITE, 3, []byte(resumeSDP), "application/sdp"); code != sip.StatusOK {
		t.Fatalf("resume re-INVITE status = %d, want 200", code)
	}

	time.Sleep(50 * time.Millisecond)

	if legs := finalizer.snapshot(); len(legs) != 0 {
		t.Fatalf("finalizer called %d time(s) before BYE — pause/resume split the recording", len(legs))
	}

	if got := len(srv.SessionsSnapshot()); got != 1 {
		t.Fatalf("sessions after resume = %d, want 1", got)
	}

	if code := send(sip.BYE, 4, nil, ""); code != sip.StatusOK {
		t.Fatalf("BYE status = %d, want 200", code)
	}

	time.Sleep(50 * time.Millisecond)

	legs := finalizer.snapshot()
	if len(legs) != 1 {
		t.Fatalf("finalizer legs = %d, want 1 (mixed mode)", len(legs))
	}

	if legs[0].FsUUID != fsUUID {
		t.Errorf("finalized FsUUID = %q, want %q", legs[0].FsUUID, fsUUID)
	}

	if legs[0].CallSid != "CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("finalized CallSid = %q", legs[0].CallSid)
	}
}
