// Package auth is siprec-receiver's web authentication: Google OAuth2
// sign-in with an allowlist authorization check, signed session
// cookies, and the admin/user role model.
//
// Roles:
//   - The bootstrap administrator is the ADMIN_EMAIL environment
//     variable. It always has the admin role, needs no database row,
//     and cannot be removed from the UI.
//   - Everyone else must have a row in the store's users table (added
//     by an administrator). The row's role is "admin" or "user" and it
//     can be disabled without deleting it.
//
// Sessions are stateless signed cookies carrying only the email;
// the role is resolved from ADMIN_EMAIL + the users table on every
// request, so removing or disabling a user takes effect immediately.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/idtoken"

	"github.com/voicetel/siprec-receiver/internal/store"
)

const (
	sessionCookie = "sr_session"
	stateCookie   = "sr_oauth_state"
	sessionTTL    = 12 * time.Hour
	stateTTL      = 10 * time.Minute
)

// UserStore is the slice of the store the authenticator needs.
type UserStore interface {
	GetUser(ctx context.Context, email string) (*store.User, error)
}

// Config wires the authenticator. AdminEmail is the bootstrap admin
// (ADMIN_EMAIL). DevEmail, when set, bypasses Google entirely and
// signs in as that address — local development only.
type Config struct {
	ClientID      string
	ClientSecret  string
	RedirectURL   string // e.g. https://srs.example.com/auth/callback
	SessionSecret string
	AdminEmail    string
	DevEmail      string
}

// SessionUser is the authenticated principal attached to a request.
type SessionUser struct {
	Email string
	Role  string // "admin" or "user"
}

// IsAdmin reports whether the user has the admin role.
func (u *SessionUser) IsAdmin() bool { return u != nil && u.Role == "admin" }

// Authenticator serves the OAuth handlers and auth middleware.
type Authenticator struct {
	cfg   Config
	users UserStore
	oauth *oauth2.Config
}

type ctxKey struct{}

// New builds an Authenticator. Call Enabled to learn whether web
// sign-in can work at all with this config.
func New(cfg Config, users UserStore) *Authenticator {
	cfg.AdminEmail = store.NormalizeEmail(cfg.AdminEmail)
	cfg.DevEmail = store.NormalizeEmail(cfg.DevEmail)

	a := &Authenticator{cfg: cfg, users: users}
	if cfg.ClientID != "" {
		a.oauth = &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint:     google.Endpoint,
		}
	}

	if cfg.DevEmail != "" {
		log.Printf("[auth] WARNING: DEV_AUTH_EMAIL set (%s) — Google sign-in is bypassed; never set this in production", cfg.DevEmail)
	}

	return a
}

// Enabled reports whether any sign-in path is configured (Google
// OAuth or the dev bypass). When false the web UI cannot be used.
func (a *Authenticator) Enabled() bool {
	return a.oauth != nil || a.cfg.DevEmail != ""
}

// AdminEmail returns the bootstrap administrator address.
func (a *Authenticator) AdminEmail() string { return a.cfg.AdminEmail }

// ResolveRole maps an authenticated Google email to a role. The
// bootstrap admin always wins; otherwise the users table decides.
// ok is false when the address is not authorized at all.
func (a *Authenticator) ResolveRole(ctx context.Context, email string) (role string, ok bool, err error) {
	email = store.NormalizeEmail(email)
	if email == "" {
		return "", false, nil
	}

	if a.cfg.AdminEmail != "" && email == a.cfg.AdminEmail {
		return "admin", true, nil
	}

	u, err := a.users.GetUser(ctx, email)
	if err != nil {
		return "", false, err
	}

	if u == nil || u.Disabled {
		return "", false, nil
	}

	return u.Role, true, nil
}

// --- OAuth handlers -------------------------------------------------------

// HandleLogin starts the sign-in flow.
func (a *Authenticator) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if a.cfg.DevEmail != "" {
		a.setSession(w, a.cfg.DevEmail)
		http.Redirect(w, r, "/recordings", http.StatusSeeOther)

		return
	}

	if a.oauth == nil {
		http.Error(w, "Google OAuth is not configured", http.StatusServiceUnavailable)

		return
	}

	state := randomToken()
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/auth/",
		MaxAge:   int(stateTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   strings.HasPrefix(a.cfg.RedirectURL, "https://"),
	})

	url := a.oauth.AuthCodeURL(state,
		oauth2.SetAuthURLParam("prompt", "select_account"))

	http.Redirect(w, r, url, http.StatusSeeOther)
}

// HandleCallback completes the flow: exchange, verify the ID token,
// authorize against the allowlist, mint the session cookie.
func (a *Authenticator) HandleCallback(w http.ResponseWriter, r *http.Request) {
	if a.oauth == nil {
		http.Error(w, "Google OAuth is not configured", http.StatusServiceUnavailable)

		return
	}

	stateCookieValue, err := r.Cookie(stateCookie)
	if err != nil || stateCookieValue.Value == "" ||
		!hmac.Equal([]byte(stateCookieValue.Value), []byte(r.URL.Query().Get("state"))) {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)

		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing OAuth code", http.StatusBadRequest)

		return
	}

	tok, err := a.oauth.Exchange(r.Context(), code)
	if err != nil {
		log.Printf("[auth] OAuth exchange failed: %v", err)
		http.Error(w, "OAuth exchange failed", http.StatusBadGateway)

		return
	}

	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		http.Error(w, "OAuth response carried no id_token", http.StatusBadGateway)

		return
	}

	payload, err := idtoken.Validate(r.Context(), rawIDToken, a.cfg.ClientID)
	if err != nil {
		log.Printf("[auth] id_token validation failed: %v", err)
		http.Error(w, "invalid id_token", http.StatusUnauthorized)

		return
	}

	email, _ := payload.Claims["email"].(string)
	verified, _ := payload.Claims["email_verified"].(bool)

	if email == "" || !verified {
		http.Error(w, "Google account email is not verified", http.StatusForbidden)

		return
	}

	_, authorized, err := a.ResolveRole(r.Context(), email)
	if err != nil {
		log.Printf("[auth] role lookup for %s failed: %v", email, err)
		http.Error(w, "authorization check failed", http.StatusInternalServerError)

		return
	}

	if !authorized {
		http.Error(w, "This Google account is not authorized for siprec-receiver. Ask an administrator to add your email.", http.StatusForbidden)

		return
	}

	a.setSession(w, store.NormalizeEmail(email))
	http.Redirect(w, r, "/recordings", http.StatusSeeOther)
}

// HandleLogout clears the session.
func (a *Authenticator) HandleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- Sessions ---------------------------------------------------------------

type sessionPayload struct {
	Email string `json:"e"`
	Exp   int64  `json:"x"`
}

func (a *Authenticator) setSession(w http.ResponseWriter, email string) {
	payload, _ := json.Marshal(sessionPayload{
		Email: email,
		Exp:   time.Now().Add(sessionTTL).Unix(),
	})

	body := base64.RawURLEncoding.EncodeToString(payload)
	value := body + "." + a.sign(body)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   strings.HasPrefix(a.cfg.RedirectURL, "https://"),
	})
}

func (a *Authenticator) sign(body string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write([]byte(body))

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// sessionEmail verifies the session cookie and returns its email.
func (a *Authenticator) sessionEmail(r *http.Request) (string, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", errors.New("no session")
	}

	body, sig, found := strings.Cut(cookie.Value, ".")
	if !found || !hmac.Equal([]byte(sig), []byte(a.sign(body))) {
		return "", errors.New("bad session signature")
	}

	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", errors.New("bad session encoding")
	}

	var p sessionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", errors.New("bad session payload")
	}

	if p.Exp < time.Now().Unix() {
		return "", errors.New("session expired")
	}

	return store.NormalizeEmail(p.Email), nil
}

// --- Middleware ---------------------------------------------------------------

// CurrentUser returns the authenticated user attached by RequireAuth,
// or nil.
func CurrentUser(r *http.Request) *SessionUser {
	u, _ := r.Context().Value(ctxKey{}).(*SessionUser)

	return u
}

// RequireAuth gates a handler on a valid session + allowlist entry.
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email, err := a.sessionEmail(r)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)

			return
		}

		role, ok, err := a.ResolveRole(r.Context(), email)
		if err != nil {
			log.Printf("[auth] role lookup for %s failed: %v", email, err)
			http.Error(w, "authorization check failed", http.StatusInternalServerError)

			return
		}

		if !ok {
			http.Error(w, "This account is no longer authorized.", http.StatusForbidden)

			return
		}

		user := &SessionUser{Email: email, Role: role}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, user)))
	})
}

// RequireAdmin gates a handler on the admin role. Compose after
// RequireAuth.
func (a *Authenticator) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !CurrentUser(r).IsAdmin() {
			http.Error(w, "Administrator access required.", http.StatusForbidden)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// CSRFToken derives the per-user CSRF token embedded in POST forms.
func (a *Authenticator) CSRFToken(email string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write([]byte("csrf:" + store.NormalizeEmail(email)))

	return hex.EncodeToString(mac.Sum(nil))
}

// CheckCSRF validates the csrf_token form field of a POST request.
func (a *Authenticator) CheckCSRF(r *http.Request, email string) bool {
	got := r.FormValue("csrf_token")

	return got != "" && hmac.Equal([]byte(got), []byte(a.CSRFToken(email)))
}

func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is catastrophic; fall back to time-based
		// material so the flow still functions (state is also checked
		// against a cookie, so this is not the only barrier).
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}

	return hex.EncodeToString(b)
}
