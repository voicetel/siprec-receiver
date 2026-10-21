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
// Identity pinning: a database user's first sign-in pins the Google
// account ID (the sub claim) to their row; later sign-ins must
// present the same sub, so an allowlisted email alone — which anyone
// can register a Google account for — is never enough again. An
// optional GOOGLE_HOSTED_DOMAIN additionally requires the account
// to belong to one Workspace domain.
//
// Sessions are signed cookies carrying email, sub, a per-user
// session epoch, and expiry. The role, pin, and epoch are re-checked
// against the database on every request, so removing, disabling, or
// epoch-bumping ("revoke sessions") a user takes effect immediately.
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
	stateCookie    = "sr_oauth_state"
	verifierCookie = "sr_oauth_verifier"
	sessionTTL     = 12 * time.Hour
	stateTTL       = 10 * time.Minute
)

// UserStore is the slice of the store the authenticator needs.
type UserStore interface {
	GetUser(ctx context.Context, email string) (*store.User, error)
	SetGoogleSubIfEmpty(ctx context.Context, email, sub string) (bool, error)
}

// Config wires the authenticator. AdminEmail is the bootstrap admin
// (ADMIN_EMAIL). HostedDomain, when set, requires Google accounts
// from that Workspace domain (the hd claim). DevEmail, when set,
// bypasses Google entirely and signs in as that address — local
// development only, refused on non-loopback listeners by main.
type Config struct {
	ClientID      string
	ClientSecret  string
	RedirectURL   string // e.g. https://srs.example.com/auth/callback
	SessionSecret string
	AdminEmail    string
	HostedDomain  string
	DevEmail      string
}

// SessionUser is the authenticated principal attached to a request.
type SessionUser struct {
	Email string
	Role  string // "admin" or "user"
	Sub   string // Google account ID from the session
	Epoch int    // session epoch from the session
	Exp   int64  // session expiry (unix seconds)
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
	cfg.HostedDomain = strings.ToLower(strings.TrimSpace(cfg.HostedDomain))

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

// cookieName is the session cookie's name: __Host- prefixed when the
// public URL is https (the prefix also requires Secure, which the
// cookie carries then), plain otherwise.
func (a *Authenticator) cookieName() string {
	if strings.HasPrefix(a.cfg.RedirectURL, "https://") {
		return "__Host-sr_session"
	}

	return "sr_session"
}

func (a *Authenticator) secureCookies() bool {
	return strings.HasPrefix(a.cfg.RedirectURL, "https://")
}

// ResolveRole maps an email to a role without identity checks. The
// bootstrap admin always wins; otherwise the users table decides.
// ok is false when the address is not authorized at all.
func (a *Authenticator) ResolveRole(ctx context.Context, email string) (role string, ok bool, err error) {
	u, ok, err := a.lookup(ctx, email)
	if err != nil || !ok {
		return "", ok, err
	}

	return u.Role, true, nil
}

// lookup resolves email to a SessionUser skeleton (role + epoch),
// without sub checks. The bootstrap admin gets a synthetic user.
func (a *Authenticator) lookup(ctx context.Context, email string) (*SessionUser, bool, error) {
	email = store.NormalizeEmail(email)
	if email == "" {
		return nil, false, nil
	}

	if a.cfg.AdminEmail != "" && email == a.cfg.AdminEmail {
		return &SessionUser{Email: email, Role: "admin"}, true, nil
	}

	row, err := a.users.GetUser(ctx, email)
	if err != nil {
		return nil, false, err
	}

	if row == nil || row.Disabled {
		return nil, false, nil
	}

	return &SessionUser{
		Email: email, Role: row.Role, Epoch: row.SessionEpoch,
	}, true, nil
}

var (
	errForbidden = errors.New("not authorized")
	errRelogin   = errors.New("session no longer valid")
)

// authorizeLogin validates a fresh Google sign-in (email + sub):
// allowlist, Workspace domain is checked by the caller via claims,
// and the sub pin is created or verified for database users.
func (a *Authenticator) authorizeLogin(ctx context.Context, email, sub string) (*SessionUser, error) {
	user, ok, err := a.lookup(ctx, email)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, errForbidden
	}

	user.Sub = sub

	// The bootstrap admin is pinned by the environment, not by a
	// database row, so there is nothing to pin the sub to.
	if a.cfg.AdminEmail != "" && user.Email == a.cfg.AdminEmail {
		return user, nil
	}

	row, err := a.users.GetUser(ctx, user.Email)
	if err != nil {
		return nil, err
	}

	if row == nil {
		return nil, errForbidden
	}

	if row.GoogleSub == "" {
		if sub == "" {
			return nil, errForbidden
		}

		pinned, err := a.users.SetGoogleSubIfEmpty(ctx, user.Email, sub)
		if err != nil {
			return nil, err
		}

		if !pinned {
			// Lost a race with a concurrent first sign-in; re-read.
			row, err = a.users.GetUser(ctx, user.Email)
			if err != nil {
				return nil, err
			}
		}
	}

	if row.GoogleSub != "" && sub != row.GoogleSub {
		return nil, errForbidden
	}

	user.Epoch = row.SessionEpoch

	return user, nil
}

// authorizeSession re-validates a session cookie's claims against
// the database on every request: role, disabled flag, sub pin, and
// session epoch (revocation).
func (a *Authenticator) authorizeSession(ctx context.Context, p *sessionPayload) (*SessionUser, error) {
	user, ok, err := a.lookup(ctx, p.Email)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, errForbidden
	}

	user.Sub, user.Epoch, user.Exp = p.Sub, p.Epoch, p.Exp

	if a.cfg.AdminEmail != "" && user.Email == a.cfg.AdminEmail {
		return user, nil // bootstrap admin: env is the revocation lever
	}

	row, err := a.users.GetUser(ctx, user.Email)
	if err != nil {
		return nil, err
	}

	if row == nil {
		return nil, errForbidden
	}

	if p.Epoch != row.SessionEpoch {
		return nil, errRelogin // revoked (or pre-epoch cookie)
	}

	// Sub pinning: enforced when both sides have a sub. A session
	// without one is a dev-bypass session (no Google identity).
	if row.GoogleSub != "" && p.Sub != "" && p.Sub != row.GoogleSub {
		return nil, errForbidden
	}

	return user, nil
}

// --- OAuth handlers -------------------------------------------------------

// HandleLogin starts the sign-in flow.
func (a *Authenticator) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if a.cfg.DevEmail != "" {
		// Dev bypass: no Google identity exists, so no sub pinning;
		// role and epoch still come from the allowlist/admin env.
		user, ok, err := a.lookup(r.Context(), a.cfg.DevEmail)
		if err != nil {
			http.Error(w, "authorization check failed", http.StatusInternalServerError)

			return
		}

		if !ok {
			http.Error(w, "DEV_AUTH_EMAIL is not an authorized user", http.StatusForbidden)

			return
		}

		a.setSession(w, user)
		http.Redirect(w, r, "/recordings", http.StatusSeeOther)

		return
	}

	if a.oauth == nil {
		http.Error(w, "Google OAuth is not configured", http.StatusServiceUnavailable)

		return
	}

	state := randomToken()
	verifier := oauth2.GenerateVerifier()

	for _, c := range []*http.Cookie{ // #nosec G124 -- Secure follows the https public URL (secureCookies); HttpOnly+SameSite always set
		{Name: stateCookie, Value: state}, // #nosec G124 -- Secure follows the https public URL (secureCookies); HttpOnly+SameSite always set
		{Name: verifierCookie, Value: verifier}, // #nosec G124 -- Secure follows the https public URL (secureCookies); HttpOnly+SameSite always set
	} {
		c.Path = "/auth/"
		c.MaxAge = int(stateTTL.Seconds())
		c.HttpOnly = true
		c.SameSite = http.SameSiteLaxMode
		c.Secure = a.secureCookies()
		http.SetCookie(w, c)
	}

	url := a.oauth.AuthCodeURL(state,
		oauth2.SetAuthURLParam("prompt", "select_account"),
		oauth2.S256ChallengeOption(verifier))

	http.Redirect(w, r, url, http.StatusSeeOther)
}

// HandleCallback completes the flow: exchange (with the PKCE
// verifier), verify the ID token, authorize against the allowlist
// and identity pins, mint the session cookie.
func (a *Authenticator) HandleCallback(w http.ResponseWriter, r *http.Request) {
	if a.oauth == nil {
		http.Error(w, "Google OAuth is not configured", http.StatusServiceUnavailable)

		return
	}

	// One-time flow cookies: clear whatever the outcome.
	for _, name := range []string{stateCookie, verifierCookie} {
		http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure follows the https public URL (secureCookies); HttpOnly+SameSite always set
			Name: name, Value: "", Path: "/auth/", MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secureCookies(),
		})
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

	var exchangeOpts []oauth2.AuthCodeOption
	if verifier, verr := r.Cookie(verifierCookie); verr == nil && verifier.Value != "" {
		exchangeOpts = append(exchangeOpts, oauth2.VerifierOption(verifier.Value))
	}

	tok, err := a.oauth.Exchange(r.Context(), code, exchangeOpts...)
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
	sub, _ := payload.Claims["sub"].(string)

	if email == "" || !verified || sub == "" {
		http.Error(w, "Google account email is not verified", http.StatusForbidden)

		return
	}

	if a.cfg.HostedDomain != "" {
		hd, _ := payload.Claims["hd"].(string)
		if !strings.EqualFold(hd, a.cfg.HostedDomain) {
			http.Error(w, "Google account is not in the required Workspace domain", http.StatusForbidden)

			return
		}
	}

	user, err := a.authorizeLogin(r.Context(), email, sub)
	if errors.Is(err, errForbidden) {
		http.Error(w, "This Google account is not authorized for siprec-receiver. Ask an administrator to add your email.", http.StatusForbidden)

		return
	}

	if err != nil {
		log.Printf("[auth] authorization for %q failed: %v", email, err) // #nosec G706 -- quoted values
		http.Error(w, "authorization check failed", http.StatusInternalServerError)

		return
	}

	a.setSession(w, user)
	http.Redirect(w, r, "/recordings", http.StatusSeeOther)
}

// HandleLogout clears the session (POST; linked as a form so a
// cross-site GET cannot log a user out).
func (a *Authenticator) HandleLogout(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{a.cookieName(), "sr_session", "__Host-sr_session"} {
		http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure follows the https public URL (secureCookies); HttpOnly+SameSite always set
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   a.secureCookies(),
		})
	}

	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- Sessions ---------------------------------------------------------------

type sessionPayload struct {
	Email string `json:"e"`
	Sub   string `json:"s,omitempty"`
	Epoch int    `json:"n"`
	Exp   int64  `json:"x"`
}

func (a *Authenticator) setSession(w http.ResponseWriter, user *SessionUser) {
	payload, _ := json.Marshal(sessionPayload{
		Email: user.Email,
		Sub:   user.Sub,
		Epoch: user.Epoch,
		Exp:   time.Now().Add(sessionTTL).Unix(),
	})

	body := base64.RawURLEncoding.EncodeToString(payload)
	value := body + "." + a.sign(body)

	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure follows the https public URL (secureCookies); HttpOnly+SameSite always set
		Name:     a.cookieName(),
		Value:    value,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.secureCookies(),
	})
}

func (a *Authenticator) sign(body string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write([]byte(body))

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// session verifies the session cookie and returns its claims.
func (a *Authenticator) session(r *http.Request) (*sessionPayload, error) {
	cookie, err := r.Cookie(a.cookieName())
	if err != nil {
		return nil, errors.New("no session")
	}

	body, sig, found := strings.Cut(cookie.Value, ".")
	if !found || !hmac.Equal([]byte(sig), []byte(a.sign(body))) {
		return nil, errors.New("bad session signature")
	}

	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, errors.New("bad session encoding")
	}

	var p sessionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errors.New("bad session payload")
	}

	if p.Exp < time.Now().Unix() {
		return nil, errors.New("session expired")
	}

	p.Email = store.NormalizeEmail(p.Email)

	return &p, nil
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
		p, err := a.session(r)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)

			return
		}

		user, err := a.authorizeSession(r.Context(), p)
		if errors.Is(err, errRelogin) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)

			return
		}

		if errors.Is(err, errForbidden) {
			http.Error(w, "This account is no longer authorized.", http.StatusForbidden)

			return
		}

		if err != nil {
			log.Printf("[auth] session authorization for %s failed: %v", p.Email, err)
			http.Error(w, "authorization check failed", http.StatusInternalServerError)

			return
		}

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

// CSRFToken derives the CSRF token for one session. Binding it to
// the session expiry means a leaked token dies with the session
// instead of living until the secret rotates.
func (a *Authenticator) CSRFToken(email string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	_, _ = fmt.Fprintf(mac, "csrf:%s:%d", store.NormalizeEmail(email), exp)

	return hex.EncodeToString(mac.Sum(nil))
}

// CheckCSRF validates the csrf_token form field of a POST request
// for the session's user.
func (a *Authenticator) CheckCSRF(r *http.Request, user *SessionUser) bool {
	if user == nil {
		return false
	}

	got := r.FormValue("csrf_token")

	return got != "" && hmac.Equal([]byte(got), []byte(a.CSRFToken(user.Email, user.Exp)))
}

func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}

	return hex.EncodeToString(b)
}
