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

	role, ok, err := a.ResolveRole(ctx, "admin@example.com")
	if err != nil || !ok || role != "admin" {
		t.Errorf("bootstrap admin = %q, %v, %v", role, ok, err)
	}

	if _, ok, _ := a.ResolveRole(ctx, "nobody@example.com"); ok {
		t.Error("unknown address authorized")
	}

	_ = st.UpsertUser(ctx, store.User{Email: "viewer@example.com", Role: "user",
		CreatedAt: time.Now().UTC()})

	role, ok, _ = a.ResolveRole(ctx, "Viewer@Example.com")
	if !ok || role != "user" {
		t.Errorf("db user = %q, %v", role, ok)
	}

	if found, err := st.SetUserDisabled(ctx, "viewer@example.com", true); !found || err != nil {
		t.Fatalf("SetUserDisabled = %v, %v", found, err)
	}

	if _, ok, _ := a.ResolveRole(ctx, "viewer@example.com"); ok {
		t.Error("disabled user still authorized")
	}

	// Saving the user's role does not enable them.
	_ = st.UpsertUser(ctx, store.User{Email: "viewer@example.com", Role: "admin"})
	if _, ok, _ := a.ResolveRole(ctx, "viewer@example.com"); ok {
		t.Error("a role change enabled a disabled user")
	}
}

func TestSubPinning(t *testing.T) {
	a, st := testAuth(t, "")
	ctx := context.Background()

	_ = st.UpsertUser(ctx, store.User{Email: "viewer@example.com", Role: "user",
		CreatedAt: time.Now().UTC()})

	// First sign-in pins the sub.
	user, err := a.authorizeLogin(ctx, "viewer@example.com", "sub-123")
	if err != nil || user == nil {
		t.Fatalf("first login: %v", err)
	}

	row, _ := st.GetUser(ctx, "viewer@example.com")
	if row.GoogleSub != "sub-123" {
		t.Fatalf("sub not pinned: %q", row.GoogleSub)
	}

	// Same sub: fine. Different Google account, same email: refused.
	if _, err := a.authorizeLogin(ctx, "viewer@example.com", "sub-123"); err != nil {
		t.Errorf("same-sub login refused: %v", err)
	}

	if _, err := a.authorizeLogin(ctx, "viewer@example.com", "sub-999"); err == nil {
		t.Error("different-sub login accepted")
	}
}

func TestSessionRoundTripTamperAndRevocation(t *testing.T) {
	a, st := testAuth(t, "")
	ctx := context.Background()

	_ = st.UpsertUser(ctx, store.User{Email: "viewer@example.com", Role: "user",
		CreatedAt: time.Now().UTC()})

	user, err := a.authorizeLogin(ctx, "viewer@example.com", "sub-123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	rec := httptest.NewRecorder()
	a.setSession(rec, user)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("setSession cookies = %d", len(cookies))
	}

	req := httptest.NewRequest(http.MethodGet, "/recordings", nil)
	req.AddCookie(cookies[0])

	p, err := a.session(req)
	if err != nil || p.Email != "viewer@example.com" || p.Sub != "sub-123" {
		t.Fatalf("session = %+v, %v", p, err)
	}

	if _, err := a.authorizeSession(ctx, p); err != nil {
		t.Fatalf("authorizeSession: %v", err)
	}

	// Tampered signature must fail.
	bad := *cookies[0]
	bad.Value += "x"

	req2 := httptest.NewRequest(http.MethodGet, "/recordings", nil)
	req2.AddCookie(&bad)

	if _, err := a.session(req2); err == nil {
		t.Error("tampered session accepted")
	}

	// Epoch bump revokes the outstanding session.
	if err := st.BumpSessionEpoch(ctx, "viewer@example.com"); err != nil {
		t.Fatalf("BumpSessionEpoch: %v", err)
	}

	if _, err := a.authorizeSession(ctx, p); err == nil {
		t.Error("revoked session still authorized")
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

func TestCSRFBoundToSession(t *testing.T) {
	a, _ := testAuth(t, "")

	tok := a.CSRFToken("viewer@example.com", 1700000000)
	if tok == "" || tok == a.CSRFToken("other@example.com", 1700000000) ||
		tok == a.CSRFToken("viewer@example.com", 1700003600) {
		t.Error("CSRF tokens must be per-user and per-session-expiry")
	}

	user := &SessionUser{Email: "viewer@example.com", Exp: 1700000000}

	req := httptest.NewRequest(http.MethodPost, "/admin/users", nil)
	if a.CheckCSRF(req, user) {
		t.Error("empty CSRF token accepted")
	}
}
