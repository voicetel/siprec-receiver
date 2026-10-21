package web

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Every response carries the hardening headers (framing is denied:
// the pages hold destructive admin actions).
func TestSecurityHeadersPresent(t *testing.T) {
	f := newFixture(t, adminEmail)
	admin := f.loginClient(t)

	resp, err := admin.Get(f.srv.URL + "/recordings")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q", got)
	}

	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}

	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", got)
	}
}

// Logout is POST-only (a cross-site GET must not sign a user out),
// and the POST actually ends the session.
func TestLogoutRequiresPOST(t *testing.T) {
	f := newFixture(t, adminEmail)
	admin := f.loginClient(t)

	resp, err := admin.Get(f.srv.URL + "/auth/logout")
	if err != nil {
		t.Fatalf("GET logout: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("GET /auth/logout = 200 (it must not be a GET route)")
	}

	// And the GET did not end the session.
	still, err := admin.Get(f.srv.URL + "/recordings")
	if err != nil {
		t.Fatalf("GET after GET-logout: %v", err)
	}
	_ = still.Body.Close()

	if strings.Contains(still.Request.URL.Path, "/login") {
		t.Fatal("GET /auth/logout ended the session")
	}

	resp2, err := admin.PostForm(f.srv.URL+"/auth/logout", url.Values{})
	if err != nil {
		t.Fatalf("POST logout: %v", err)
	}
	_ = resp2.Body.Close()

	// The (cleared) cookie no longer opens the recordings list: the
	// client lands on the login page.
	resp3, err := admin.Get(f.srv.URL + "/recordings")
	if err != nil {
		t.Fatalf("GET after logout: %v", err)
	}
	_ = resp3.Body.Close()

	if !strings.Contains(resp3.Request.URL.Path, "/login") {
		t.Errorf("after logout landed on %s, want /login", resp3.Request.URL.Path)
	}
}

// Administrative actions land in the audit log, readable by admins.
func TestAuditLogRecordsAdminActions(t *testing.T) {
	f := newFixture(t, adminEmail)
	callID, _ := f.seedCall(t)
	admin := f.loginClient(t)

	resp, err := admin.PostForm(f.srv.URL+"/admin/users", url.Values{
		"email":      {"audit-viewer@example.com"},
		"role":       {"user"},
		"csrf_token": {csrfFromPage(t, admin, f.srv.URL+"/admin/users")},
	})
	if err != nil {
		t.Fatalf("add user: %v", err)
	}
	_ = resp.Body.Close()

	resp2, err := admin.PostForm(f.srv.URL+"/recordings/"+itoa(callID)+"/delete", url.Values{
		"csrf_token": {csrfFromPage(t, admin, f.srv.URL+"/recordings/"+itoa(callID))},
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = resp2.Body.Close()

	resp3, err := admin.Get(f.srv.URL + "/admin/audit")
	if err != nil {
		t.Fatalf("audit page: %v", err)
	}

	page, _ := io.ReadAll(resp3.Body)
	_ = resp3.Body.Close()

	for _, want := range []string{"user.upsert", "recording.delete", adminEmail} {
		if !strings.Contains(string(page), want) {
			t.Errorf("audit page missing %q", want)
		}
	}
}
