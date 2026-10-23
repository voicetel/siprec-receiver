package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A callback whose method is GET (StatusCallbackMethod=GET) sends its
// fields as the query string. Both notify endpoints take it; the raw
// payload kept for audit is the query without the secret.
func TestNotifyAcceptsGETCallbacks(t *testing.T) {
	e := newCovEnv(t, covOpts{notifySecret: secret})
	callID := seedCallWithIDs(t, e.fixture)

	q := url.Values{
		"secret": {secret}, "CallSid": {"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"CallStatus": {"completed"}, "CallDuration": {"62"}, "SequenceNumber": {"3"},
	}
	r := covGet(t, http.DefaultClient, e.srv.URL+"/api/notify/disposition?"+q.Encode())
	wantStatus(t, "GET disposition", r, http.StatusOK)

	if !strings.Contains(r.body, `"matched":true`) {
		t.Errorf("GET disposition did not match the recording: %s", r.body)
	}

	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/notify/amd?"+url.Values{
		"CallSid": {"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "AnsweredBy": {"machine_end_beep"},
		"MachineDetectionDuration": {"2100"},
	}.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	wantStatus(t, "GET amd", covDo(t, http.DefaultClient, req), http.StatusOK)

	c, _ := e.store.GetCall(context.Background(), callID)
	if c.DispStatus != "completed" || c.DispDurationSec != 62 || c.AMDAnsweredBy != "machine_end_beep" || c.AMDDurationMs != 2100 {
		t.Errorf("stored disposition %q %d, amd %q %d", c.DispStatus, c.DispDurationSec, c.AMDAnsweredBy, c.AMDDurationMs)
	}

	if strings.Contains(c.DispRaw, secret) || !strings.Contains(c.DispRaw, "CallStatus=completed") {
		t.Errorf("raw disposition = %q, want the query without the secret", c.DispRaw)
	}

	// GET is authorized like POST.
	wantStatus(t, "GET without secret",
		covGet(t, http.DefaultClient, e.srv.URL+"/api/notify/amd?AnsweredBy=human&CallSid=CAx"),
		http.StatusUnauthorized)
}

// A "+" in the secret, pasted into a callback URL unencoded, decodes as
// a space; the secret as typed still authorizes.
func TestQuerySecretWithAPlus(t *testing.T) {
	const plus = "abc+def-0123456789"

	e := newCovEnv(t, covOpts{notifySecret: plus})

	r := covGet(t, http.DefaultClient, e.srv.URL+"/api/notify/amd?secret="+plus+"&AnsweredBy=human&CallSid=CAplus")
	wantStatus(t, "raw plus", r, http.StatusOK)

	r = covGet(t, http.DefaultClient, e.srv.URL+"/api/notify/amd?"+url.Values{
		"secret": {plus}, "AnsweredBy": {"human"}, "CallSid": {"CAplus"},
	}.Encode())
	wantStatus(t, "encoded plus", r, http.StatusOK)

	r = covGet(t, http.DefaultClient, e.srv.URL+"/api/notify/amd?secret=abc+def&AnsweredBy=human&CallSid=CAplus")
	wantStatus(t, "wrong secret", r, http.StatusUnauthorized)
}
