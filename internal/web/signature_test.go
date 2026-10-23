package web

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/voicetel/siprec-receiver/internal/store"
)

// Known-answer vectors: Twilio's documented POST example, and a GET
// (URL only) vector computed with an independent HMAC replica. These
// pin webhookSignature, which the end-to-end tests below then sign with.
func TestWebhookSignatureKnownVectors(t *testing.T) {
	post := webhookSignature("12345", "https://mycompany.com/myapp.php?foo=1&bar=2", url.Values{
		"CallSid": {"CA1234567890ABCDE"},
		"Caller":  {"+14158675309"},
		"Digits":  {"1234"},
		"From":    {"+14158675309"},
		"To":      {"+18005551212"},
	})
	if post != "RSOYDt4T1cUTdK1PDd93/VVr8B8=" {
		t.Errorf("POST vector = %q", post)
	}

	if get := webhookSignature("secret", "https://example.com/cb?a=1&b=2", nil); get != "PtKEvOBE/mctzKzKgG9Svuo8uH8=" {
		t.Errorf("GET vector = %q", get)
	}
}

func TestSignedURLs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"https://srs.example.com/api/notify/amd", []string{
			"https://srs.example.com/api/notify/amd", "https://srs.example.com:443/api/notify/amd"}},
		{"https://srs.example.com:443/n?a=1", []string{
			"https://srs.example.com:443/n?a=1", "https://srs.example.com/n?a=1"}},
		{"http://srs.example.com/n", []string{"http://srs.example.com/n", "http://srs.example.com:80/n"}},
		{"https://[2001:db8::1]/n", []string{"https://[2001:db8::1]/n", "https://[2001:db8::1]:443/n"}},
		{"http://127.0.0.1:8080/n", []string{"http://127.0.0.1:8080/n"}},
		{"ftp://srs.example.com/n", []string{"ftp://srs.example.com/n"}},
		{"http://[::1/n", []string{"http://[::1/n"}},
	} {
		if got := signedURLs(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("signedURLs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

const (
	sigAccount = "AC00000000000000000000000000000001" // seedCallWithIDs's account
	sigKey     = "account-one-api-key"
	sigOther   = "AC00000000000000000000000000000002"
)

// signedEnv is a notify server whose public URL is the test server's,
// holding API keys for two accounts.
func signedEnv(t *testing.T) *covEnv {
	t.Helper()

	e := newCovEnv(t, covOpts{notifySecret: secret})
	e.ws.PublicBaseURL = e.srv.URL

	for acct, key := range map[string]string{sigAccount: sigKey, sigOther: "account-two-api-key"} {
		if err := e.store.UpsertVoiceMLAccount(context.Background(), store.VoiceMLAccount{AccountSid: acct, APIKey: key}); err != nil {
			t.Fatal(err)
		}
	}

	return e
}

// signedPost sends form to path, signed as VoiceML signs it.
func signedPost(t *testing.T, e *covEnv, path, key string, form url.Values) covResp {
	t.Helper()

	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(signatureHeader, webhookSignature(key, e.srv.URL+path, form))

	return covDo(t, http.DefaultClient, req)
}

// A VoiceML callback carrying no secret is authorized by its
// X-Twilio-Signature, made with the stored API key of its AccountSid,
// by POST or GET.
func TestNotifySignedByTheAccountKey(t *testing.T) {
	e := signedEnv(t)
	callID := seedCallWithIDs(t, e.fixture)

	form := url.Values{
		"AccountSid": {sigAccount}, "CallSid": {"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"CallStatus": {"completed"}, "CallDuration": {"62"},
	}
	r := signedPost(t, e, "/api/notify/disposition", sigKey, form)
	wantStatus(t, "signed POST", r, http.StatusOK)

	if !strings.Contains(r.body, `"matched":true`) {
		t.Errorf("signed POST did not match the recording: %s", r.body)
	}

	q := url.Values{"AccountSid": {sigAccount}, "CallSid": {"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "AnsweredBy": {"human"}}
	target := e.srv.URL + "/api/notify/amd?" + q.Encode()
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(signatureHeader, webhookSignature(sigKey, target, nil))
	wantStatus(t, "signed GET", covDo(t, http.DefaultClient, req), http.StatusOK)

	if c, _ := e.store.GetCall(context.Background(), callID); c.DispDurationSec != 62 || c.AMDAnsweredBy != "human" {
		t.Errorf("stored %d s, amd %q", c.DispDurationSec, c.AMDAnsweredBy)
	}
}

// Signatures that do not verify, or cannot be checked, are 401.
func TestNotifyRejectsUnverifiableSignatures(t *testing.T) {
	e := signedEnv(t)
	seedCallWithIDs(t, e.fixture)

	form := url.Values{"AccountSid": {sigAccount}, "CallSid": {"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "AnsweredBy": {"human"}}

	// Signed with the wrong key, or over other fields.
	wantStatus(t, "wrong key", signedPost(t, e, "/api/notify/amd", "not-the-key", form), http.StatusUnauthorized)

	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/notify/amd",
		strings.NewReader(form.Encode()+"&AnsweredBy=fax"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(signatureHeader, webhookSignature(sigKey, e.srv.URL+"/api/notify/amd", form))
	wantStatus(t, "tampered body", covDo(t, http.DefaultClient, req), http.StatusUnauthorized)

	// No stored key for the AccountSid, or no AccountSid at all.
	unknown := url.Values{"AccountSid": {"AC00000000000000000000000000000009"}, "CallSid": {"CAx"}, "AnsweredBy": {"human"}}
	wantStatus(t, "unknown account", signedPost(t, e, "/api/notify/amd", sigKey, unknown), http.StatusUnauthorized)

	noAccount := url.Values{"CallSid": {"CAx"}, "AnsweredBy": {"human"}}
	wantStatus(t, "no AccountSid", signedPost(t, e, "/api/notify/amd", sigKey, noAccount), http.StatusUnauthorized)

	// A JSON body is not signed this way.
	body := `{"AccountSid":"` + sigAccount + `","CallSid":"CAx","AnsweredBy":"human"}`
	req, _ = http.NewRequest(http.MethodPost, e.srv.URL+"/api/notify/amd", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(signatureHeader, webhookSignature(sigKey, e.srv.URL+"/api/notify/amd", nil))
	wantStatus(t, "signed JSON", covDo(t, http.DefaultClient, req), http.StatusUnauthorized)

	// Without a public base URL the signed URL cannot be rebuilt.
	e.ws.PublicBaseURL = ""
	wantStatus(t, "no public base URL", signedPost(t, e, "/api/notify/amd", sigKey, form), http.StatusUnauthorized)
	e.ws.PublicBaseURL = e.srv.URL

	// The key lookup itself fails.
	_ = e.store.Close()
	wantStatus(t, "key lookup failure", signedPost(t, e, "/api/notify/amd", sigKey, form), http.StatusInternalServerError)
}

// An account's signature authorizes only that account's calls: naming
// another account's call is 403 and changes nothing. The shared secret
// stays trusted for every call.
func TestSignedNotifyStaysInItsAccount(t *testing.T) {
	e := signedEnv(t)
	callID := seedCallWithIDs(t, e.fixture)

	form := url.Values{
		"AccountSid": {sigOther}, "CallSid": {"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "CallStatus": {"busy"},
	}
	r := signedPost(t, e, "/api/notify/disposition", "account-two-api-key", form)
	wantStatus(t, "foreign call", r, http.StatusForbidden)

	if !strings.Contains(r.body, "another account") {
		t.Errorf("403 body = %q", r.body)
	}

	if c, _ := e.store.GetCall(context.Background(), callID); c.DispStatus != "" || c.AccountSid != sigAccount {
		t.Errorf("foreign notification changed the call: %q %q", c.DispStatus, c.AccountSid)
	}

	r = covNotify(t, e.srv.URL+"/api/notify/disposition", "application/x-www-form-urlencoded", form.Encode(),
		map[string]string{"Authorization": "Bearer " + secret})
	wantStatus(t, "secret, any account", r, http.StatusOK)
}
