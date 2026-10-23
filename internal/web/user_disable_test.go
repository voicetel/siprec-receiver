package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/voicetel/siprec-receiver/internal/store"
)

func TestAdminDisablesAndEnablesAUser(t *testing.T) {
	e := newCovEnv(t, covOpts{})
	logs := captureLog(t)
	admin, csrf := e.adminSession(t)
	u := e.srv.URL + "/admin/users/disable"

	if err := e.store.UpsertUser(context.Background(), store.User{Email: "viewer@example.com", Role: "user"}); err != nil {
		t.Fatal(err)
	}

	set := func(email, disabled, token string) covResp {
		return covPost(t, admin, u, url.Values{"email": {email}, "disabled": {disabled}, "csrf_token": {token}})
	}

	wantStatus(t, "bad csrf", set("viewer@example.com", "1", "nope"), http.StatusForbidden)
	wantRedirect(t, "disable", set("viewer@example.com", "1", csrf), "User+disabled")

	if got, _ := e.store.GetUser(context.Background(), "viewer@example.com"); !got.Disabled {
		t.Fatal("not disabled")
	}

	if r := covGet(t, admin, e.srv.URL+"/admin/users"); !strings.Contains(r.body, ">Enable<") {
		t.Errorf("users page offers no Enable for the disabled user:\n%s", r.body)
	}

	wantRedirect(t, "enable", set("Viewer@Example.com", "0", csrf), "User+enabled")

	if got, _ := e.store.GetUser(context.Background(), "viewer@example.com"); got.Disabled {
		t.Fatal("not enabled")
	}

	wantRedirect(t, "bootstrap admin", set(adminEmail, "1", csrf), "cannot+be+disabled")
	wantRedirect(t, "absent user", set("nobody@example.com", "1", csrf), "No+such+user")

	if !strings.Contains(logs.String(), `set user "viewer@example.com" disabled=true`) {
		t.Errorf("disable not logged: %q", logs.String())
	}

	_ = e.store.Close()
	wantStatus(t, "store failure", set("viewer@example.com", "1", csrf), http.StatusInternalServerError)
}

// An administrator with a user row cannot disable themselves.
func TestAdminCannotDisableThemselves(t *testing.T) {
	f := newFixture(t, "second-admin@example.com")

	if err := f.store.UpsertUser(context.Background(), store.User{Email: "second-admin@example.com", Role: "admin"}); err != nil {
		t.Fatal(err)
	}

	c := f.loginClient(t)
	csrf := csrfFromPage(t, c, f.srv.URL+"/admin/users")
	nc := &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	r := covPost(t, nc, f.srv.URL+"/admin/users/disable",
		url.Values{"email": {"second-admin@example.com"}, "disabled": {"1"}, "csrf_token": {csrf}})
	wantRedirect(t, "self", r, "cannot+disable+yourself")

	if got, _ := f.store.GetUser(context.Background(), "second-admin@example.com"); got.Disabled {
		t.Error("the administrator disabled themselves")
	}
}
