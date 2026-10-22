package web

import (
	"context"
	"net/http"
	"net/url"
	"testing"
)

// Revoking the bootstrap admin's sessions works like any user's: the
// first revoke creates its users row (the epoch every admin cookie is
// checked against from then on), and the admin's current cookie stops
// working. It used to report success and change nothing.
func TestRevokeBootstrapAdminSessions(t *testing.T) {
	e := newCovEnv(t, covOpts{})
	admin, csrf := e.adminSession(t)
	u := e.srv.URL

	wantRedirect(t, "revoke unknown user",
		covPost(t, admin, u+"/admin/users/revoke", url.Values{"email": {"nobody@example.com"}, "csrf_token": {csrf}}),
		"No+such+user")

	wantRedirect(t, "revoke bootstrap admin",
		covPost(t, admin, u+"/admin/users/revoke", url.Values{"email": {adminEmail}, "csrf_token": {csrf}}),
		"Sessions+revoked")

	if row, err := e.store.GetUser(context.Background(), adminEmail); err != nil || row == nil || row.SessionEpoch == 0 {
		t.Fatalf("admin row after revoke = %+v, %v", row, err)
	}

	if r := covGet(t, admin, u+"/recordings"); r.status == http.StatusOK {
		t.Error("the bootstrap admin's revoked cookie still works")
	}

	// A fresh sign-in gets the new epoch and works again.
	again, _ := e.adminSession(t)
	if r := covGet(t, again, u+"/recordings"); r.status != http.StatusOK {
		t.Errorf("fresh admin session = %d, want 200", r.status)
	}
}

func TestRevokeStoreFailures(t *testing.T) {
	e := newCovEnv(t, covOpts{})
	admin, csrf := e.adminSession(t)
	form := url.Values{"email": {adminEmail}, "csrf_token": {csrf}}

	// The row cannot be created: the users table rejects inserts.
	e.execSQL(t, `CREATE TRIGGER cov_no_insert BEFORE INSERT ON users BEGIN SELECT RAISE(ABORT, 'read-only'); END`)
	wantStatus(t, "admin row create failure", covPost(t, admin, e.srv.URL+"/admin/users/revoke", form), http.StatusInternalServerError)

	// The user lookup itself fails (the break-glass admin still gets in).
	e.execSQL(t, `ALTER TABLE users RENAME TO cov_gone`)
	wantStatus(t, "lookup failure", covPost(t, admin, e.srv.URL+"/admin/users/revoke", form), http.StatusInternalServerError)
}
