package web

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/auth"
	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
	"github.com/voicetel/siprec-receiver/internal/store"
)

const (
	adminEmail = "admin@example.com"
	userEmail  = "viewer@example.com"
	secret     = "test-notify-secret"
	sessSecret = "test-session-secret"
)

type fixture struct {
	srv    *httptest.Server
	store  *store.Store
	auth   *auth.Authenticator
	recDir string
}

func newFixture(t *testing.T, devEmail string) *fixture {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	authn := auth.New(auth.Config{
		SessionSecret: sessSecret,
		AdminEmail:    adminEmail,
		DevEmail:      devEmail,
	}, st)

	recDir := t.TempDir()

	ws, err := New(st, authn, recDir, secret)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	srv := httptest.NewServer(ws.Handler())
	t.Cleanup(srv.Close)

	return &fixture{srv: srv, store: st, auth: authn, recDir: recDir}
}

// loginClient returns a client holding a session for the fixture's
// DevEmail (the authenticator's dev bypass signs in as that address).
func (f *fixture) loginClient(t *testing.T) *http.Client {
	t.Helper()

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	resp, err := c.Get(f.srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login landed on %d, want 200 (recordings page)", resp.StatusCode)
	}

	return c
}

func (f *fixture) seedCall(t *testing.T) (callID int64, legID int64) {
	t.Helper()

	wavPath := filepath.Join(f.recDir, "dlg-1-12165285911.wav")
	if err := os.WriteFile(wavPath, []byte("RIFF-fake-wav-bytes"), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}

	rec, err := siprecsrv.ParseMetadata(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>complete</datamode>
  <session session_id="1234abcd"><start-time>2026-05-05T12:00:00Z</start-time></session>
  <participant participant_id="P1"><nameID aor="sip:+12165285911@fs.example.com"/></participant>
  <participant participant_id="P2"><nameID aor="sip:+12085179911@fs.example.com"/></participant>
  <stream stream_id="S1" session_id="1234abcd"><label>1</label></stream>
  <participantstreamassoc participant_id="P1"><send>S1</send></participantstreamassoc>
</recording>`)
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
	}

	err = f.store.RecordLeg(context.Background(), siprecsrv.FinalizedLeg{
		CallID: "dlg-1", Label: "12165285911", WAVPath: wavPath,
		DurationSec: 42, StartedAt: time.Now().UTC(), Metadata: rec,
	})
	if err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	calls, _, err := f.store.ListCalls(context.Background(), store.ListFilter{})
	if err != nil || len(calls) != 1 {
		t.Fatalf("ListCalls: %v (%d calls)", err, len(calls))
	}

	call, err := f.store.GetCall(context.Background(), calls[0].ID)
	if err != nil || call == nil || len(call.Legs) != 1 {
		t.Fatalf("GetCall: %+v, %v", call, err)
	}

	return call.ID, call.Legs[0].ID
}

func TestNotifyAMD_FormAuthAndLog(t *testing.T) {
	f := newFixture(t, adminEmail)
	callID, _ := f.seedCall(t)

	form := url.Values{
		"CallSid":                  {"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"AccountSid":               {"ACtest"},
		"AnsweredBy":               {"machine_end_beep"},
		"MachineDetectionDuration": {"3100"},
		"From":                     {"+12165285911"},
		"To":                       {"+12085179911"},
		"CallStatus":               {"completed"},
		"SipCallId":                {"dlg-1"},
	}

	// No secret -> 401.
	resp, err := http.PostForm(f.srv.URL+"/api/notify/amd", form)
	if err != nil {
		t.Fatalf("notify no-auth: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("notify without secret = %d, want 401", resp.StatusCode)
	}

	// Bearer secret -> 200, matched to the recording by SipCallId.
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/notify/amd",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+secret)

	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("notify: %v", err)
	}

	body, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK || !strings.Contains(string(body), `"matched":true`) {
		t.Fatalf("notify = %d %s, want 200 matched", resp2.StatusCode, body)
	}

	call, err := f.store.GetCall(context.Background(), callID)
	if err != nil || call == nil {
		t.Fatalf("GetCall: %v", err)
	}

	if call.AMDAnsweredBy != "machine_end_beep" || call.AMDDurationMs != 3100 {
		t.Errorf("stored AMD = %q/%d", call.AMDAnsweredBy, call.AMDDurationMs)
	}

	// Missing AnsweredBy -> 400.
	bad := url.Values{"CallSid": {"CAx"}}
	req3, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/notify/amd",
		strings.NewReader(bad.Encode()))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req3.Header.Set("X-Notify-Secret", secret)

	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("notify bad: %v", err)
	}
	_ = resp3.Body.Close()

	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("notify without AnsweredBy = %d, want 400", resp3.StatusCode)
	}
}

func TestUI_RequiresAuth(t *testing.T) {
	f := newFixture(t, adminEmail)

	resp, err := http.Get(f.srv.URL + "/recordings")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()

	// The default client follows the redirect to /login.
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(resp.Request.URL.Path, "/login") {
		t.Fatalf("unauthenticated /recordings landed %s (%d), want /login",
			resp.Request.URL.Path, resp.StatusCode)
	}
}

func TestUI_AdminAndUserRoles(t *testing.T) {
	f := newFixture(t, adminEmail)
	callID, legID := f.seedCall(t)

	admin := f.loginClient(t)

	// Admin sees the recordings list + detail + users page.
	for _, path := range []string{"/recordings", "/admin/users"} {
		resp, err := admin.Get(f.srv.URL + path)
		if err != nil {
			t.Fatalf("admin GET %s: %v", path, err)
		}

		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("admin GET %s = %d: %s", path, resp.StatusCode, b)
		}
	}

	detailURL := f.srv.URL + "/recordings/" + itoa(callID)
	resp, err := admin.Get(detailURL)
	if err != nil {
		t.Fatalf("admin detail: %v", err)
	}

	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if !strings.Contains(string(b), "machine") && !strings.Contains(string(b), "12165285911") {
		t.Errorf("detail page missing expected content")
	}

	// Admin adds the viewer user.
	form := url.Values{
		"email":      {userEmail},
		"role":       {"user"},
		"csrf_token": {f.auth.CSRFToken(adminEmail)},
	}

	resp2, err := admin.PostForm(f.srv.URL+"/admin/users", form)
	if err != nil {
		t.Fatalf("add user: %v", err)
	}
	_ = resp2.Body.Close()

	if u, _ := f.store.GetUser(context.Background(), userEmail); u == nil {
		t.Fatal("user was not added")
	}

	// A second authenticator sharing store+secret but dev-signed-in as
	// the viewer mints a user session against the same server.
	userFixtureAuth := auth.New(auth.Config{
		SessionSecret: sessSecret, AdminEmail: adminEmail, DevEmail: userEmail,
	}, f.store)
	_ = userFixtureAuth

	jar, _ := cookiejar.New(nil)
	userClient := &http.Client{Jar: jar}

	// Dev-login endpoint on the main server signs in as ITS DevEmail
	// (admin), so mint the viewer cookie through a throwaway server
	// bound to the second authenticator, then reuse the cookie jar's
	// cookie against the main server (same secret, same store).
	ws2, err := New(f.store, userFixtureAuth, f.recDir, secret)
	if err != nil {
		t.Fatalf("web.New 2: %v", err)
	}

	srv2 := httptest.NewServer(ws2.Handler())
	defer srv2.Close()

	resp3, err := userClient.Get(srv2.URL + "/auth/login")
	if err != nil {
		t.Fatalf("user login: %v", err)
	}
	_ = resp3.Body.Close()

	// Copy cookies to the main server's origin.
	for _, ckie := range jar.Cookies(mustParseURL(t, srv2.URL)) {
		jar.SetCookies(mustParseURL(t, f.srv.URL), []*http.Cookie{ckie})
	}

	resp4, err := userClient.Get(f.srv.URL + "/recordings")
	if err != nil {
		t.Fatalf("user list: %v", err)
	}
	_ = resp4.Body.Close()

	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("user /recordings = %d, want 200", resp4.StatusCode)
	}

	resp5, err := userClient.Get(f.srv.URL + "/admin/users")
	if err != nil {
		t.Fatalf("user admin page: %v", err)
	}
	_ = resp5.Body.Close()

	if resp5.StatusCode != http.StatusForbidden {
		t.Fatalf("user /admin/users = %d, want 403", resp5.StatusCode)
	}

	// User can play audio.
	audioURL := f.srv.URL + "/recordings/" + itoa(callID) + "/legs/" + itoa(legID) + "/audio"
	resp6, err := userClient.Get(audioURL)
	if err != nil {
		t.Fatalf("user audio: %v", err)
	}

	audio, _ := io.ReadAll(resp6.Body)
	_ = resp6.Body.Close()

	if resp6.StatusCode != http.StatusOK || string(audio) != "RIFF-fake-wav-bytes" {
		t.Fatalf("audio = %d %q", resp6.StatusCode, audio)
	}

	// User cannot delete.
	delForm := url.Values{"csrf_token": {f.auth.CSRFToken(userEmail)}}
	resp7, err := userClient.PostForm(
		f.srv.URL+"/recordings/"+itoa(callID)+"/delete", delForm)
	if err != nil {
		t.Fatalf("user delete: %v", err)
	}
	_ = resp7.Body.Close()

	if resp7.StatusCode != http.StatusForbidden {
		t.Fatalf("user delete = %d, want 403", resp7.StatusCode)
	}

	if call, _ := f.store.GetCall(context.Background(), callID); call == nil {
		t.Fatal("call was deleted by a non-admin")
	}
}

func TestAudio_PathOutsideRecordingDirRefused(t *testing.T) {
	f := newFixture(t, adminEmail)

	err := f.store.RecordLeg(context.Background(), siprecsrv.FinalizedLeg{
		CallID: "evil", Label: "x", WAVPath: "/etc/hostname",
		DurationSec: 1, StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	call, _ := f.store.GetCall(context.Background(), 1)
	admin := f.loginClient(t)

	resp, err := admin.Get(f.srv.URL + "/recordings/" + itoa(call.ID) +
		"/legs/" + itoa(call.Legs[0].ID) + "/audio")
	if err != nil {
		t.Fatalf("audio: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outside-dir audio = %d, want 404", resp.StatusCode)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}

	return u
}
