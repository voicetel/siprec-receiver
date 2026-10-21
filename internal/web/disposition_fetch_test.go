package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
	"github.com/voicetel/siprec-receiver/internal/store"
)

// seedCallWithIDs records a call carrying the correlation IDs the
// disposition endpoint and the VoiceML fetch key on.
func seedCallWithIDs(t *testing.T, f *fixture) int64 {
	t.Helper()

	err := f.store.RecordLeg(context.Background(), siprecsrv.FinalizedLeg{
		CallID: "dlg-disp", CallSid: "CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AccountSid: "ACtest", Label: "12165285911",
		WAVPath: "/rec/disp.wav", DurationSec: 55,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	calls, _, err := f.store.ListCalls(context.Background(), store.ListFilter{})
	if err != nil || len(calls) != 1 {
		t.Fatalf("ListCalls: %v (%d)", err, len(calls))
	}

	return calls[0].ID
}

func TestNotifyDisposition_FormAuthAndMerge(t *testing.T) {
	f := newFixture(t, adminEmail)
	callID := seedCallWithIDs(t, f)

	form := url.Values{
		"CallSid":         {"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"CallStatus":      {"completed"},
		"CallDuration":    {"62"},
		"SipResponseCode": {"200"},
		"AnsweredBy":      {"machine_start"},
	}

	resp, err := http.PostForm(f.srv.URL+"/api/notify/disposition", form)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-secret disposition = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/notify/disposition",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+secret)

	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post with secret: %v", err)
	}

	body, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK || !strings.Contains(string(body), `"matched":true`) {
		t.Fatalf("disposition = %d %s", resp2.StatusCode, body)
	}

	call, _ := f.store.GetCall(context.Background(), callID)
	if call.DispStatus != "completed" || call.DispDurationSec != 62 ||
		call.AMDAnsweredBy != "machine_start" {
		t.Errorf("stored disp/amd = %q/%d/%q",
			call.DispStatus, call.DispDurationSec, call.AMDAnsweredBy)
	}

	// Missing CallStatus -> 400.
	bad := url.Values{"CallSid": {"CAx"}}
	req3, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/notify/disposition",
		strings.NewReader(bad.Encode()))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req3.Header.Set("X-Notify-Secret", secret)

	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("bad post: %v", err)
	}
	_ = resp3.Body.Close()

	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("disposition without CallStatus = %d, want 400", resp3.StatusCode)
	}
}

func TestFetchRecord_EndToEnd(t *testing.T) {
	// Fake VoiceML: serves one Call record, checks Basic auth.
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "ACtest" || pass != "key123" {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if r.URL.Path != "/2010-04-01/Accounts/ACtest/Calls/CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json" {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sid":"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
			`"account_sid":"ACtest","from":"+12165285911","to":"+12085179911",` +
			`"status":"completed","direction":"outbound-api",` +
			`"answered_by":"human","duration":"62"}`))
	}))
	defer cb.Close()

	f := newFixtureVM(t, adminEmail, cb.URL)
	callID := seedCallWithIDs(t, f)
	admin := f.loginClient(t)

	post := func(path string, form url.Values) *http.Response {
		cl := &http.Client{
			Jar: admin.Jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}

		resp, err := cl.PostForm(f.srv.URL+path, form)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}

		return resp
	}

	// No key configured yet: fetch redirects with an explanation.
	resp := post("/recordings/"+itoa(callID)+"/fetch-record", url.Values{
		"csrf_token": {csrfFromPage(t, admin, f.srv.URL+"/recordings/"+itoa(callID))},
	})
	loc := resp.Header.Get("Location")
	_ = resp.Body.Close()

	if !strings.Contains(loc, "No+VoiceML+API+key") &&
		!strings.Contains(loc, "No%20VoiceML%20API%20key") {
		t.Fatalf("fetch without key redirected to %q", loc)
	}

	// Admin stores the account key.
	resp2 := post("/admin/integrations", url.Values{
		"account_sid": {"ACtest"},
		"api_key":     {"key123"},
		"csrf_token":  {csrfFromPage(t, admin, f.srv.URL+"/admin/integrations")},
	})
	_ = resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("save key = %d", resp2.StatusCode)
	}

	// The key must never be rendered back.
	respG, err := admin.Get(f.srv.URL + "/admin/integrations")
	if err != nil {
		t.Fatalf("integrations page: %v", err)
	}

	page, _ := io.ReadAll(respG.Body)
	_ = respG.Body.Close()

	if strings.Contains(string(page), "key123") {
		t.Error("API key leaked into the integrations page HTML")
	}

	// Now the fetch succeeds and the record lands on the call.
	resp3 := post("/recordings/"+itoa(callID)+"/fetch-record", url.Values{
		"csrf_token": {csrfFromPage(t, admin, f.srv.URL+"/recordings/"+itoa(callID))},
	})
	_ = resp3.Body.Close()

	call, _ := f.store.GetCall(context.Background(), callID)
	if call.VoiceMLRecordJSON == "" || !strings.Contains(call.VoiceMLRecordJSON, `"answered_by"`) {
		t.Fatalf("cb record not stored: %q", call.VoiceMLRecordJSON)
	}

	if call.DispStatus != "completed" || call.AMDAnsweredBy != "human" {
		t.Errorf("denormalized disp/amd = %q/%q", call.DispStatus, call.AMDAnsweredBy)
	}
}
