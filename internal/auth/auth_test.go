package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/store"
)

func testAuth(t *testing.T, devEmail string) (*Authenticator, *store.Store) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	a := New(Config{
		SessionSecret: "test-secret",
		AdminEmail:    "Admin@Example.com",
		DevEmail:      devEmail,
	}, st)

	return a, st
}

func TestResolveRole(t *testing.T) {
	a, st := testAuth(t, "")
	ctx := context.Background()

	// Bootstrap admin from env, case-insensitive, no DB row needed.
	role, ok, err := a.ResolveRole(ctx, "admin@example.com")
	if err != nil || !ok || role != "admin" {
		t.Errorf("bootstrap admin = %q, %v, %v", role, ok, err)
	}

	// Unknown address: not authorized.
	if _, ok, _ := a.ResolveRole(ctx, "nobody@example.com"); ok {
		t.Error("unknown address authorized")
	}

	// DB user.
	_ = st.UpsertUser(ctx, store.User{Email: "viewer@example.com", Role: "user",
		CreatedAt: time.Now().UTC()})

	role, ok, _ = a.ResolveRole(ctx, "Viewer@Example.com")
	if !ok || role != "user" {
		t.Errorf("db user = %q, %v", role, ok)
	}

	// Disabled user loses access immediately.
	_ = st.UpsertUser(ctx, store.User{Email: "viewer@example.com", Role: "user", Disabled: true})
	if _, ok, _ := a.ResolveRole(ctx, "viewer@example.com"); ok {
		t.Error("disabled user still authorized")
	}
}

func TestSessionCookieRoundTripAndTamper(t *testing.T) {
	a, _ := testAuth(t, "")

	rec := httptest.NewRecorder()
	a.setSession(rec, "viewer@example.com")

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("setSession cookies = %d", len(cookies))
	}

	req := httptest.NewRequest(http.MethodGet, "/recordings", nil)
	req.AddCookie(cookies[0])

	email, err := a.sessionEmail(req)
	if err != nil || email != "viewer@example.com" {
		t.Fatalf("sessionEmail = %q, %v", email, err)
	}

	// Tampered signature must fail.
	bad := *cookies[0]
	bad.Value += "x"

	req2 := httptest.NewRequest(http.MethodGet, "/recordings", nil)
	req2.AddCookie(&bad)

	if _, err := a.sessionEmail(req2); err == nil {
		t.Error("tampered session accepted")
	}
}

func TestRequireAuthRedirectsAnonymous(t *testing.T) {
	a, _ := testAuth(t, "")

	called := false
	h := a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/recordings", nil))

	if called {
		t.Error("handler called for anonymous request")
	}

	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 to /login", rec.Code)
	}
}

func TestCSRF(t *testing.T) {
	a, _ := testAuth(t, "")

	tok := a.CSRFToken("viewer@example.com")
	if tok == "" || tok == a.CSRFToken("other@example.com") {
		t.Error("CSRF tokens must be per-user and non-empty")
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/users", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if a.CheckCSRF(req, "viewer@example.com") {
		t.Error("empty CSRF token accepted")
	}
}
