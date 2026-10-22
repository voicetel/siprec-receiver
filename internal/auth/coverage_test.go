package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/voicetel/siprec-receiver/internal/store"
)

// covStore is a scripted UserStore: getUser receives the 1-based call
// number so tests can make the first and later lookups differ.
type covStore struct {
	mu        sync.Mutex
	calls     int
	getUser   func(call int, email string) (*store.User, error)
	setPinned bool
	setErr    error
	setCalls  int
}

func (s *covStore) GetUser(_ context.Context, email string) (*store.User, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()

	if s.getUser == nil {
		return nil, nil
	}

	return s.getUser(n, email)
}

func (s *covStore) SetGoogleSubIfEmpty(_ context.Context, _, _ string) (bool, error) {
	s.mu.Lock()
	s.setCalls++
	s.mu.Unlock()

	return s.setPinned, s.setErr
}

var errCovStore = errors.New("store down")

func covRows(rows ...any) func(int, string) (*store.User, error) {
	return func(call int, _ string) (*store.User, error) {
		if call > len(rows) {
			call = len(rows)
		}

		switch v := rows[call-1].(type) {
		case error:
			return nil, v
		case *store.User:
			return v, nil
		default:
			return nil, nil
		}
	}
}

func covAuth(st UserStore, cfg Config) *Authenticator {
	if cfg.SessionSecret == "" {
		cfg.SessionSecret = "cov-secret"
	}

	return New(cfg, st)
}

func TestCovNewEnabledAndCookies(t *testing.T) {
	a := covAuth(&covStore{}, Config{AdminEmail: "  Boss@Example.COM "})
	if a.Enabled() {
		t.Error("Enabled with neither OAuth nor dev email")
	}

	if a.AdminEmail() != "boss@example.com" {
		t.Errorf("AdminEmail = %q", a.AdminEmail())
	}

	if a.cookieName() != "sr_session" || a.secureCookies() {
		t.Errorf("http config: cookie %q secure %v", a.cookieName(), a.secureCookies())
	}

	g := covAuth(&covStore{}, Config{
		ClientID: "cid", ClientSecret: "csec",
		RedirectURL:  "https://srs.example.com/auth/callback",
		HostedDomain: " Example.COM ",
	})
	if !g.Enabled() || g.oauth == nil || g.oauth.ClientID != "cid" {
		t.Error("OAuth config not enabled")
	}

	if g.cfg.HostedDomain != "example.com" {
		t.Errorf("HostedDomain = %q", g.cfg.HostedDomain)
	}

	if g.cookieName() != "__Host-sr_session" || !g.secureCookies() {
		t.Errorf("https config: cookie %q secure %v", g.cookieName(), g.secureCookies())
	}

	d := covAuth(&covStore{}, Config{DevEmail: " Dev@Example.com"})
	if !d.Enabled() || d.cfg.DevEmail != "dev@example.com" {
		t.Errorf("dev config: enabled %v email %q", d.Enabled(), d.cfg.DevEmail)
	}
}

func TestCovIsAdmin(t *testing.T) {
	var nilUser *SessionUser
	if nilUser.IsAdmin() {
		t.Error("nil user is admin")
	}

	if (&SessionUser{Role: "user"}).IsAdmin() || !(&SessionUser{Role: "admin"}).IsAdmin() {
		t.Error("IsAdmin role mapping wrong")
	}
}

func TestCovResolveRoleAndLookup(t *testing.T) {
	ctx := context.Background()

	a := covAuth(&covStore{getUser: covRows(errCovStore)}, Config{})
	if _, ok, err := a.ResolveRole(ctx, "x@example.com"); ok || !errors.Is(err, errCovStore) {
		t.Errorf("store error: ok %v err %v", ok, err)
	}

	if role, ok, err := a.ResolveRole(ctx, "   "); role != "" || ok || err != nil {
		t.Errorf("empty email = %q %v %v", role, ok, err)
	}

	a = covAuth(&covStore{getUser: covRows(&store.User{Role: "admin", SessionEpoch: 4})}, Config{})

	u, ok, err := a.lookup(ctx, "X@Example.com")
	if err != nil || !ok || u.Email != "x@example.com" || u.Role != "admin" || u.Epoch != 4 {
		t.Errorf("lookup = %+v %v %v", u, ok, err)
	}

	a = covAuth(&covStore{getUser: covRows(&store.User{Role: "user", Disabled: true})}, Config{})
	if _, ok, err := a.lookup(ctx, "x@example.com"); ok || err != nil {
		t.Errorf("disabled = %v %v", ok, err)
	}
}

func TestCovAuthorizeLogin(t *testing.T) {
	ctx := context.Background()
	unpinned := &store.User{Role: "user", SessionEpoch: 2}
	pinnedX := &store.User{Role: "user", GoogleSub: "sub-x", SessionEpoch: 3}

	tests := []struct {
		name      string
		st        *covStore
		sub       string
		wantErr   error
		wantEpoch int
		wantSets  int
	}{
		{name: "lookup error", st: &covStore{getUser: covRows(errCovStore)}, sub: "s", wantErr: errCovStore},
		{name: "unknown", st: &covStore{getUser: covRows(nil)}, sub: "s", wantErr: errForbidden},
		{name: "reread error", st: &covStore{getUser: covRows(unpinned, errCovStore)}, sub: "s", wantErr: errCovStore},
		{name: "row vanished", st: &covStore{getUser: covRows(unpinned, nil)}, sub: "s", wantErr: errForbidden},
		{name: "unpinned empty sub", st: &covStore{getUser: covRows(unpinned)}, sub: "", wantErr: errForbidden},
		{name: "pin error", st: &covStore{getUser: covRows(unpinned), setErr: errCovStore}, sub: "s", wantErr: errCovStore, wantSets: 1},
		{name: "pin ok", st: &covStore{getUser: covRows(unpinned), setPinned: true}, sub: "s", wantEpoch: 2, wantSets: 1},
		{name: "lost race same sub", st: &covStore{getUser: covRows(unpinned, unpinned, pinnedX)}, sub: "sub-x", wantEpoch: 3, wantSets: 1},
		{name: "lost race other sub", st: &covStore{getUser: covRows(unpinned, unpinned, pinnedX)}, sub: "sub-y", wantErr: errForbidden, wantSets: 1},
		{name: "lost race reread error", st: &covStore{getUser: covRows(unpinned, unpinned, errCovStore)}, sub: "s", wantErr: errCovStore, wantSets: 1},
		{name: "already pinned match", st: &covStore{getUser: covRows(pinnedX)}, sub: "sub-x", wantEpoch: 3},
		{name: "already pinned mismatch", st: &covStore{getUser: covRows(pinnedX)}, sub: "sub-z", wantErr: errForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := covAuth(tc.st, Config{AdminEmail: "boss@example.com"})

			u, err := a.authorizeLogin(ctx, "user@example.com", tc.sub)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || u != nil {
					t.Fatalf("got %+v, %v; want %v", u, err, tc.wantErr)
				}
			} else if err != nil || u.Epoch != tc.wantEpoch || u.Sub != tc.sub || u.Email != "user@example.com" {
				t.Fatalf("got %+v, %v", u, err)
			}

			if tc.st.setCalls != tc.wantSets {
				t.Errorf("SetGoogleSubIfEmpty calls = %d, want %d", tc.st.setCalls, tc.wantSets)
			}
		})
	}

	st := &covStore{getUser: covRows(nil)}
	a := covAuth(st, Config{AdminEmail: "boss@example.com"})

	u, err := a.authorizeLogin(ctx, "Boss@example.com", "admin-sub")
	if err != nil || u.Role != "admin" || u.Sub != "admin-sub" || u.Epoch != 0 || st.setCalls != 0 {
		t.Errorf("bootstrap admin = %+v, %v, pins %d", u, err, st.setCalls)
	}

	// Once its sessions were revoked the admin's row carries its epoch.
	a = covAuth(&covStore{getUser: covRows(&store.User{Role: "admin", SessionEpoch: 9})}, Config{AdminEmail: "boss@example.com"})
	if u, err := a.authorizeLogin(ctx, "boss@example.com", "admin-sub"); err != nil || u.Epoch != 9 {
		t.Errorf("revoked-before admin = %+v, %v; want epoch 9", u, err)
	}

	// The break-glass admin signs in even when its row cannot be read.
	a = covAuth(&covStore{getUser: covRows(errCovStore)}, Config{AdminEmail: "boss@example.com"})
	if u, err := a.authorizeLogin(ctx, "boss@example.com", "admin-sub"); err != nil || !u.IsAdmin() {
		t.Errorf("admin with an unreadable row = %+v, %v", u, err)
	}

	// ADMIN_GOOGLE_SUB pins the bootstrap admin's Google account.
	a = covAuth(&covStore{getUser: covRows(nil)}, Config{AdminEmail: "boss@example.com", AdminGoogleSub: "admin-sub"})
	if _, err := a.authorizeLogin(ctx, "boss@example.com", "someone-else"); !errors.Is(err, errForbidden) {
		t.Errorf("admin with another Google account = %v, want forbidden", err)
	}

	if u, err := a.authorizeLogin(ctx, "boss@example.com", "admin-sub"); err != nil || !u.IsAdmin() {
		t.Errorf("admin with the pinned account = %+v, %v", u, err)
	}
}

// A first sign-in that loses the pin race to a concurrent one re-reads
// the row; a row removed in between is refused, not dereferenced.
func TestCovAuthorizeLoginRowRemovedDuringPin(t *testing.T) {
	st := &covStore{getUser: covRows(&store.User{Role: "user"}, &store.User{Role: "user"}, nil)}
	a := covAuth(st, Config{})

	if _, err := a.authorizeLogin(context.Background(), "u@example.com", "sub-1"); !errors.Is(err, errForbidden) {
		t.Errorf("err = %v, want forbidden", err)
	}
}

func TestCovAuthorizeSession(t *testing.T) {
	ctx := context.Background()
	row := &store.User{Role: "user", GoogleSub: "sub-1", SessionEpoch: 5}

	tests := []struct {
		name    string
		st      *covStore
		p       sessionPayload
		wantErr error
	}{
		{name: "lookup error", st: &covStore{getUser: covRows(errCovStore)}, p: sessionPayload{Email: "u@example.com"}, wantErr: errCovStore},
		{name: "unknown", st: &covStore{getUser: covRows(nil)}, p: sessionPayload{Email: "u@example.com"}, wantErr: errForbidden},
		{name: "reread error", st: &covStore{getUser: covRows(row, errCovStore)}, p: sessionPayload{Email: "u@example.com", Epoch: 5}, wantErr: errCovStore},
		{name: "row vanished", st: &covStore{getUser: covRows(row, nil)}, p: sessionPayload{Email: "u@example.com", Epoch: 5}, wantErr: errForbidden},
		{name: "epoch bumped", st: &covStore{getUser: covRows(row)}, p: sessionPayload{Email: "u@example.com", Epoch: 4, Sub: "sub-1"}, wantErr: errRelogin},
		{name: "sub mismatch", st: &covStore{getUser: covRows(row)}, p: sessionPayload{Email: "u@example.com", Epoch: 5, Sub: "sub-2"}, wantErr: errForbidden},
		{name: "dev session no sub", st: &covStore{getUser: covRows(row)}, p: sessionPayload{Email: "u@example.com", Epoch: 5, Exp: 99}},
		{name: "matching sub", st: &covStore{getUser: covRows(row)}, p: sessionPayload{Email: "u@example.com", Epoch: 5, Sub: "sub-1", Exp: 99}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := covAuth(tc.st, Config{AdminEmail: "boss@example.com"})

			u, err := a.authorizeSession(ctx, &tc.p)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || u != nil {
					t.Fatalf("got %+v, %v; want %v", u, err, tc.wantErr)
				}

				return
			}

			if err != nil || u.Role != "user" || u.Sub != tc.p.Sub || u.Epoch != 5 || u.Exp != 99 {
				t.Fatalf("got %+v, %v", u, err)
			}
		})
	}

	admin := func(st *covStore, sub string) *Authenticator {
		return covAuth(st, Config{AdminEmail: "boss@example.com", AdminGoogleSub: sub})
	}

	u, err := admin(&covStore{getUser: covRows(nil)}, "").authorizeSession(ctx,
		&sessionPayload{Email: "boss@example.com", Sub: "s", Epoch: 0, Exp: 11})
	if err != nil || !u.IsAdmin() || u.Sub != "s" || u.Exp != 11 {
		t.Errorf("bootstrap admin = %+v, %v", u, err)
	}

	for name, tc := range map[string]struct {
		st   *covStore
		pin  string
		p    sessionPayload
		want error
	}{
		"revoked":          {&covStore{getUser: covRows(&store.User{Role: "admin", SessionEpoch: 8})}, "", sessionPayload{Email: "boss@example.com", Epoch: 7}, errRelogin},
		"unreadable row":   {&covStore{getUser: covRows(errCovStore)}, "", sessionPayload{Email: "boss@example.com", Epoch: 3}, nil},
		"another account":  {&covStore{getUser: covRows(nil)}, "pinned", sessionPayload{Email: "boss@example.com", Sub: "other"}, errForbidden},
		"current epoch ok": {&covStore{getUser: covRows(&store.User{Role: "admin", SessionEpoch: 8})}, "pinned", sessionPayload{Email: "boss@example.com", Sub: "pinned", Epoch: 8}, nil},
		"dev session":      {&covStore{getUser: covRows(nil)}, "pinned", sessionPayload{Email: "boss@example.com"}, nil},
	} {
		u, err := admin(tc.st, tc.pin).authorizeSession(ctx, &tc.p)
		if !errors.Is(err, tc.want) || (tc.want == nil && !u.IsAdmin()) {
			t.Errorf("%s: %+v, %v; want %v", name, u, err, tc.want)
		}
	}
}

func covCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()

	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}

	t.Fatalf("cookie %q not set; got %v", name, rec.Result().Cookies())

	return nil
}

func TestCovHandleLoginDevBypass(t *testing.T) {
	a := covAuth(&covStore{getUser: covRows(errCovStore)}, Config{DevEmail: "dev@example.com"})
	rec := httptest.NewRecorder()
	a.HandleLogin(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("store error status = %d", rec.Code)
	}

	a = covAuth(&covStore{getUser: covRows(nil)}, Config{DevEmail: "dev@example.com"})
	rec = httptest.NewRecorder()
	a.HandleLogin(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "DEV_AUTH_EMAIL") {
		t.Errorf("unauthorized dev status = %d body %q", rec.Code, rec.Body.String())
	}

	a = covAuth(&covStore{}, Config{DevEmail: "Boss@example.com", AdminEmail: "boss@example.com"})
	rec = httptest.NewRecorder()
	a.HandleLogin(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/recordings" {
		t.Fatalf("dev login = %d -> %q", rec.Code, rec.Header().Get("Location"))
	}

	req := httptest.NewRequest(http.MethodGet, "/recordings", nil)
	req.AddCookie(covCookie(t, rec, "sr_session"))

	p, err := a.session(req)
	if err != nil || p.Email != "boss@example.com" || p.Sub != "" {
		t.Errorf("dev session = %+v, %v", p, err)
	}
}

func TestCovHandleLoginNotConfigured(t *testing.T) {
	a := covAuth(&covStore{}, Config{})

	rec := httptest.NewRecorder()
	a.HandleLogin(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("login status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	a.HandleCallback(rec, httptest.NewRequest(http.MethodGet, "/auth/callback", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("callback status = %d", rec.Code)
	}
}

func TestCovHandleLoginGoogle(t *testing.T) {
	a := covAuth(&covStore{}, Config{
		ClientID: "cid", RedirectURL: "https://srs.example.com/auth/callback",
	})

	rec := httptest.NewRecorder()
	a.HandleLogin(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}

	state := covCookie(t, rec, stateCookie)
	verifier := covCookie(t, rec, verifierCookie)

	for _, c := range []*http.Cookie{state, verifier} {
		if c.Value == "" || c.Path != "/auth/" || c.MaxAge != int(stateTTL.Seconds()) ||
			!c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
			t.Errorf("flow cookie %+v", c)
		}
	}

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: %v", err)
	}

	q := loc.Query()
	if !strings.HasPrefix(loc.String(), google.Endpoint.AuthURL) ||
		q.Get("state") != state.Value || q.Get("prompt") != "select_account" ||
		q.Get("client_id") != "cid" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(verifier.Value) {
		t.Errorf("auth URL = %s", loc)
	}
}

// covOAuth is a fake Google: an httptest server answering the OAuth
// token endpoint and the JWKS cert endpoint idtoken.Validate fetches.
type covOAuth struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu        sync.Mutex
	idTokens  map[string]string // code -> id_token
	verifiers []string          // code_verifier per token request
}

var (
	covKeyOnce sync.Once
	covKey     *rsa.PrivateKey
)

type covRewrite struct {
	target *url.URL
	base   http.RoundTripper
}

func (rt covRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme = rt.target.Scheme
	r2.URL.Host = rt.target.Host
	r2.Host = rt.target.Host

	return rt.base.RoundTrip(r2)
}

func newCovOAuth(t *testing.T) *covOAuth {
	t.Helper()

	covKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}

		covKey = k
	})

	f := &covOAuth{key: covKey, idTokens: map[string]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		code := r.PostForm.Get("code")

		f.mu.Lock()
		f.verifiers = append(f.verifiers, r.PostForm.Get("code_verifier"))
		idTok, ok := f.idTokens[code]
		f.mu.Unlock()

		if code == "fail" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)

			return
		}

		resp := map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600}
		if ok {
			resp["id_token"] = idTok
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/oauth2/v3/certs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "cov-kid", "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes()),
		}}})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	target, _ := url.Parse(f.srv.URL)
	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = covRewrite{target: target, base: http.DefaultTransport}

	t.Cleanup(func() { http.DefaultClient.Transport = prev })

	return f
}

func (f *covOAuth) sign(t *testing.T, claims map[string]any) string {
	t.Helper()

	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "cov-kid"})

	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}

	signed := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(signed))

	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *covOAuth) claims(email string, verified bool, sub, hd string) map[string]any {
	c := map[string]any{
		"iss": "https://accounts.google.com", "aud": "cid",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"email": email, "email_verified": verified, "sub": sub,
	}
	if hd != "" {
		c["hd"] = hd
	}

	return c
}

func (f *covOAuth) authenticator(st UserStore, hostedDomain string) *Authenticator {
	a := covAuth(st, Config{
		ClientID: "cid", ClientSecret: "csec",
		RedirectURL:  "http://127.0.0.1/auth/callback",
		AdminEmail:   "boss@example.com",
		HostedDomain: hostedDomain,
	})
	a.oauth.Endpoint = oauth2.Endpoint{
		AuthURL:   f.srv.URL + "/auth",
		TokenURL:  f.srv.URL + "/token",
		AuthStyle: oauth2.AuthStyleInParams,
	}

	return a
}

func covCallback(a *Authenticator, query string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?"+query, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}

	rec := httptest.NewRecorder()
	a.HandleCallback(rec, req)

	return rec
}

func TestCovHandleCallbackStateAndCode(t *testing.T) {
	f := newCovOAuth(t)
	a := f.authenticator(&covStore{}, "")
	st := &http.Cookie{Name: stateCookie, Value: "st"}

	cases := []struct {
		name    string
		query   string
		cookies []*http.Cookie
		want    int
		body    string
	}{
		{"no state cookie", "state=st&code=c", nil, http.StatusBadRequest, "invalid OAuth state"},
		{"empty state cookie", "state=&code=c", []*http.Cookie{{Name: stateCookie, Value: ""}}, http.StatusBadRequest, "invalid OAuth state"},
		{"state mismatch", "state=other&code=c", []*http.Cookie{st}, http.StatusBadRequest, "invalid OAuth state"},
		{"missing code", "state=st", []*http.Cookie{st}, http.StatusBadRequest, "missing OAuth code"},
		{"exchange fails", "state=st&code=fail", []*http.Cookie{st}, http.StatusBadGateway, "OAuth exchange failed"},
		{"no id_token", "state=st&code=noid", []*http.Cookie{st}, http.StatusBadGateway, "no id_token"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := covCallback(a, tc.query, tc.cookies...)
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("got %d %q, want %d %q", rec.Code, rec.Body.String(), tc.want, tc.body)
			}

			cleared := 0

			for _, c := range rec.Result().Cookies() {
				if (c.Name == stateCookie || c.Name == verifierCookie) && c.MaxAge < 0 && c.Path == "/auth/" {
					cleared++
				}
			}

			if cleared != 2 {
				t.Errorf("flow cookies cleared = %d, want 2", cleared)
			}
		})
	}
}

func TestCovHandleCallbackIDToken(t *testing.T) {
	f := newCovOAuth(t)
	viewer := &store.User{Role: "user", SessionEpoch: 1}

	f.idTokens["garbage"] = "not-a-jwt"
	f.idTokens["unverified"] = f.sign(t, f.claims("viewer@example.com", false, "sub-v", ""))
	f.idTokens["nosub"] = f.sign(t, f.claims("viewer@example.com", true, "", ""))
	f.idTokens["wronghd"] = f.sign(t, f.claims("viewer@example.com", true, "sub-v", "other.com"))
	f.idTokens["viewer"] = f.sign(t, f.claims("viewer@example.com", true, "sub-v", "Example.com"))
	f.idTokens["admin"] = f.sign(t, f.claims("Boss@Example.com", true, "sub-a", ""))

	st := &http.Cookie{Name: stateCookie, Value: "st"}
	ver := &http.Cookie{Name: verifierCookie, Value: "the-verifier"}

	cases := []struct {
		name  string
		store *covStore
		hd    string
		code  string
		want  int
		body  string
	}{
		{"invalid id_token", &covStore{}, "", "garbage", http.StatusUnauthorized, "invalid id_token"},
		{"unverified email", &covStore{}, "", "unverified", http.StatusForbidden, "not verified"},
		{"missing sub", &covStore{}, "", "nosub", http.StatusForbidden, "not verified"},
		{"wrong hosted domain", &covStore{}, "example.com", "wronghd", http.StatusForbidden, "Workspace domain"},
		{"not allowlisted", &covStore{getUser: covRows(nil)}, "example.com", "viewer", http.StatusForbidden, "not authorized"},
		{"store error", &covStore{getUser: covRows(errCovStore)}, "example.com", "viewer", http.StatusInternalServerError, "authorization check failed"},
		{"viewer ok", &covStore{getUser: covRows(viewer), setPinned: true}, "example.com", "viewer", http.StatusSeeOther, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := f.authenticator(tc.store, tc.hd)

			rec := covCallback(a, "state=st&code="+tc.code, st, ver)
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("got %d %q, want %d %q", rec.Code, rec.Body.String(), tc.want, tc.body)
			}
		})
	}

	f.mu.Lock()
	for i, v := range f.verifiers {
		if v != "the-verifier" {
			t.Errorf("token request %d code_verifier = %q", i, v)
		}
	}
	f.verifiers = nil
	f.mu.Unlock()

	// Bootstrap admin, no verifier cookie: exchange without PKCE,
	// then a session cookie that RequireAuth accepts.
	a := f.authenticator(&covStore{}, "")

	rec := covCallback(a, "state=st&code=admin", st)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/recordings" {
		t.Fatalf("admin callback = %d -> %q %q", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}

	f.mu.Lock()
	if len(f.verifiers) != 1 || f.verifiers[0] != "" {
		t.Errorf("verifiers without cookie = %q", f.verifiers)
	}
	f.mu.Unlock()

	sess := covCookie(t, rec, "sr_session")
	if !sess.HttpOnly || sess.Secure || sess.MaxAge != int(sessionTTL.Seconds()) || sess.Path != "/" {
		t.Errorf("session cookie %+v", sess)
	}

	var got *SessionUser

	h := a.RequireAuth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = CurrentUser(r) }))
	req := httptest.NewRequest(http.MethodGet, "/recordings", nil)
	req.AddCookie(sess)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got == nil || got.Email != "boss@example.com" || !got.IsAdmin() || got.Sub != "sub-a" {
		t.Errorf("CurrentUser after callback = %+v", got)
	}
}

func TestCovHandleLogout(t *testing.T) {
	a := covAuth(&covStore{}, Config{RedirectURL: "https://srs.example.com/auth/callback"})

	rec := httptest.NewRecorder()
	a.HandleLogout(rec, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("logout = %d -> %q", rec.Code, rec.Header().Get("Location"))
	}

	names := map[string]bool{}

	for _, c := range rec.Result().Cookies() {
		if c.MaxAge >= 0 || c.Value != "" || !c.Secure {
			t.Errorf("cookie not cleared: %+v", c)
		}

		names[c.Name] = true
	}

	if !names["sr_session"] || !names["__Host-sr_session"] {
		t.Errorf("cleared cookies = %v", names)
	}
}

func covSigned(a *Authenticator, body string) *http.Cookie {
	return &http.Cookie{Name: a.cookieName(), Value: body + "." + a.sign(body)}
}

func covPayload(t *testing.T, p sessionPayload) string {
	t.Helper()

	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestCovSessionErrors(t *testing.T) {
	a := covAuth(&covStore{}, Config{})
	future := time.Now().Add(time.Hour).Unix()

	cases := []struct {
		name   string
		cookie *http.Cookie
		want   string
	}{
		{"no cookie", nil, "no session"},
		{"no separator", &http.Cookie{Name: "sr_session", Value: "abc"}, "bad session signature"},
		{"bad base64", covSigned(a, "!!!"), "bad session encoding"},
		{"bad json", covSigned(a, base64.RawURLEncoding.EncodeToString([]byte("not json"))), "bad session payload"},
		{"expired", covSigned(a, covPayload(t, sessionPayload{Email: "u@example.com", Exp: time.Now().Add(-time.Minute).Unix()})), "session expired"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}

			p, err := a.session(req)
			if err == nil || err.Error() != tc.want || p != nil {
				t.Fatalf("got %+v, %v; want %q", p, err, tc.want)
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(covSigned(a, covPayload(t, sessionPayload{Email: " U@Example.com ", Exp: future})))

	p, err := a.session(req)
	if err != nil || p.Email != "u@example.com" {
		t.Errorf("normalized session = %+v, %v", p, err)
	}
}

func TestCovRequireAuth(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	row := &store.User{Role: "user", GoogleSub: "sub-1", SessionEpoch: 2}

	cases := []struct {
		name   string
		st     *covStore
		p      sessionPayload
		want   int
		called bool
	}{
		{"revoked", &covStore{getUser: covRows(row)}, sessionPayload{Email: "u@example.com", Sub: "sub-1", Epoch: 1, Exp: future}, http.StatusSeeOther, false},
		{"forbidden", &covStore{getUser: covRows(nil)}, sessionPayload{Email: "u@example.com", Exp: future}, http.StatusForbidden, false},
		{"store error", &covStore{getUser: covRows(errCovStore)}, sessionPayload{Email: "u@example.com", Exp: future}, http.StatusInternalServerError, false},
		{"ok", &covStore{getUser: covRows(row)}, sessionPayload{Email: "u@example.com", Sub: "sub-1", Epoch: 2, Exp: future}, http.StatusNoContent, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := covAuth(tc.st, Config{})

			var got *SessionUser

			h := a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = CurrentUser(r)
				w.WriteHeader(http.StatusNoContent)
			}))

			req := httptest.NewRequest(http.MethodGet, "/recordings", nil)
			req.AddCookie(covSigned(a, covPayload(t, tc.p)))

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want || (got != nil) != tc.called {
				t.Fatalf("status %d (want %d), handler user %+v", rec.Code, tc.want, got)
			}

			if tc.want == http.StatusSeeOther && rec.Header().Get("Location") != "/login" {
				t.Errorf("Location = %q", rec.Header().Get("Location"))
			}

			if tc.called && (got.Email != "u@example.com" || got.Role != "user" || got.Exp != future) {
				t.Errorf("CurrentUser = %+v", got)
			}
		})
	}
}

func TestCovRequireAdmin(t *testing.T) {
	a := covAuth(&covStore{}, Config{})
	h := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, tc := range []struct {
		user *SessionUser
		want int
	}{
		{nil, http.StatusForbidden},
		{&SessionUser{Email: "u@example.com", Role: "user"}, http.StatusForbidden},
		{&SessionUser{Email: "b@example.com", Role: "admin"}, http.StatusNoContent},
	} {
		req := httptest.NewRequest(http.MethodGet, "/admin", nil)
		if tc.user != nil {
			req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, tc.user))
		}

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != tc.want {
			t.Errorf("user %+v: status %d, want %d", tc.user, rec.Code, tc.want)
		}
	}

	if CurrentUser(httptest.NewRequest(http.MethodGet, "/", nil)) != nil {
		t.Error("CurrentUser without RequireAuth should be nil")
	}
}

func TestCovCheckCSRF(t *testing.T) {
	a := covAuth(&covStore{}, Config{})
	user := &SessionUser{Email: "u@example.com", Exp: 1700000000}

	post := func(tok string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/admin/users",
			strings.NewReader(url.Values{"csrf_token": {tok}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		return r
	}

	good := a.CSRFToken(" U@Example.com", user.Exp)

	if a.CheckCSRF(post(good), nil) {
		t.Error("nil user accepted")
	}

	if !a.CheckCSRF(post(good), user) {
		t.Error("valid token rejected")
	}

	if a.CheckCSRF(post(good+"0"), user) {
		t.Error("wrong token accepted")
	}
}

func TestCovRandomToken(t *testing.T) {
	x, y := randomToken(), randomToken()
	if len(x) != 32 || x == y || strings.Trim(x, "0123456789abcdef") != "" {
		t.Errorf("randomToken = %q, %q", x, y)
	}
}
