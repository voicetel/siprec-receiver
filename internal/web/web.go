// Package web is siprec-receiver's HTTP surface: the Google-OAuth
// protected recording review/admin UI and the machine-to-machine AMD
// call-notification endpoint. It shares one listener; the notify
// endpoint authenticates with a shared secret, everything else with a
// session cookie (see internal/auth).
package web

import (
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/voicetel/siprec-receiver/internal/auth"
	"github.com/voicetel/siprec-receiver/internal/store"
	"github.com/voicetel/siprec-receiver/internal/voiceml"
)

// Server bundles the HTTP handlers' dependencies.
type Server struct {
	Store          *store.Store
	Auth           *auth.Authenticator
	RecordingDir   string
	NotifySecret   string
	VoiceMLBaseURL string // VoiceML base URL for on-request record fetches; "" disables

	secretQueryWarn sync.Once
	tmpl            *template.Template
}

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed openapi.yaml
var openAPISpec []byte

// redocPage renders the spec with ReDoc. The ReDoc bundle loads from
// the jsDelivr CDN; the spec itself is same-origin.
const redocPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>siprec-receiver API</title>
</head>
<body>
<redoc spec-url="/openapi.yaml"></redoc>
<script src="https://cdn.jsdelivr.net/npm/redoc@2/bundles/redoc.standalone.js"></script>
</body>
</html>
`

// amdDispositions is the filter vocabulary for the recordings list —
// VoiceML's internal AMD verdict set (Twilio's strict wire set
// plus the extended screened/disconnected values).
var amdDispositions = []string{
	"human", "machine_start", "machine_end_beep", "machine_end_silence",
	"machine_end_other", "fax", "unknown", "screened", "screened_ivr",
	"disconnected",
}

// New builds a Server and parses the embedded templates.
func New(st *store.Store, a *auth.Authenticator, recordingDir, notifySecret, voicemlBaseURL string) (*Server, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}

			return t.UTC().Format("2006-01-02 15:04:05 UTC")
		},
		"add":      func(a, b int) int { return a + b },
		"subtract": func(a, b int) int { return a - b },
		"fmtDur": func(sec int) string {
			if sec <= 0 {
				return "—"
			}

			return fmt.Sprintf("%dm %02ds", sec/60, sec%60)
		},
	}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}

	return &Server{
		Store:          st,
		Auth:           a,
		RecordingDir:   recordingDir,
		NotifySecret:   notifySecret,
		VoiceMLBaseURL: strings.TrimRight(strings.TrimSpace(voicemlBaseURL), "/"),
		tmpl:           tmpl,
	}, nil
}

// Handler returns the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(openAPISpec)
	})
	mux.HandleFunc("GET /api/docs", func(w http.ResponseWriter, r *http.Request) {
		// The global CSP forbids CDN scripts; this one public page
		// gets a scoped policy that allows exactly the ReDoc bundle.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' https://cdn.jsdelivr.net; "+
				"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; "+
				"img-src 'self' data: https://cdn.jsdelivr.net; worker-src 'self' blob:; "+
				"font-src 'self' data: https://cdn.jsdelivr.net; frame-ancestors 'none'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(redocPage))
	})

	mux.HandleFunc("POST /api/notify/amd", s.handleNotifyAMD)
	mux.HandleFunc("POST /api/notify/disposition", s.handleNotifyDisposition)

	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.Handle("GET /auth/login", http.HandlerFunc(s.Auth.HandleLogin))
	mux.Handle("GET /auth/callback", http.HandlerFunc(s.Auth.HandleCallback))

	authed := http.NewServeMux()
	authed.Handle("POST /auth/logout", http.HandlerFunc(s.Auth.HandleLogout))
	authed.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/recordings", http.StatusSeeOther)
	})
	authed.HandleFunc("GET /recordings", s.handleList)
	authed.HandleFunc("GET /recordings/{id}", s.handleDetail)
	authed.HandleFunc("GET /recordings/{id}/legs/{legID}/audio", s.handleAudio)
	authed.HandleFunc("POST /recordings/{id}/fetch-record", s.handleFetchRecord)
	authed.Handle("POST /recordings/{id}/delete",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleDelete)))
	authed.Handle("GET /admin/users",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleUsers)))
	authed.Handle("POST /admin/users",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleUserUpsert)))
	authed.Handle("POST /admin/users/delete",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleUserDelete)))
	authed.Handle("POST /admin/users/revoke",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleUserRevoke)))
	authed.Handle("GET /admin/audit",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleAudit)))
	authed.Handle("GET /admin/integrations",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleIntegrations)))
	authed.Handle("POST /admin/integrations",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleIntegrationUpsert)))
	authed.Handle("POST /admin/integrations/delete",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleIntegrationDelete)))

	mux.Handle("/", s.Auth.RequireAuth(authed))

	return securityHeaders(mux)
}

// securityHeaders adds the baseline hardening headers to every
// response: no framing (the pages carry destructive admin actions),
// no MIME sniffing, same-origin referrers, and a CSP that only
// allows our own pages, styles, and audio.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy",
			"frame-ancestors 'none'; default-src 'self'; style-src 'self' 'unsafe-inline'; media-src 'self'; img-src 'self' data:")
		h.Set("Referrer-Policy", "same-origin")

		next.ServeHTTP(w, r)
	})
}

type baseData struct {
	User *auth.SessionUser
	CSRF string
	Msg  string
}

func (s *Server) base(r *http.Request) baseData {
	u := auth.CurrentUser(r)

	d := baseData{User: u, Msg: r.URL.Query().Get("msg")}
	if u != nil {
		d.CSRF = s.Auth.CSRFToken(u.Email, u.Exp)
	}

	return d
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("[web] render %s: %v", name, err)
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	data := struct {
		baseData
		AuthEnabled bool
	}{baseData: s.base(r), AuthEnabled: s.Auth.Enabled()}

	s.render(w, "login.html", data)
}

// --- Recordings -----------------------------------------------------------

type listData struct {
	baseData
	Calls []store.Call
	Total int
	Page  int
	Pages int
	Query string
	AMD   string
	AMDs  []string
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	const pageSize = 50

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	f := store.ListFilter{
		Query:  strings.TrimSpace(r.URL.Query().Get("q")),
		AMD:    r.URL.Query().Get("amd"),
		Limit:  pageSize,
		Offset: (page - 1) * pageSize,
	}

	calls, total, err := s.Store.ListCalls(r.Context(), f)
	if err != nil {
		log.Printf("[web] list calls: %v", err)
		http.Error(w, "failed to list recordings", http.StatusInternalServerError)

		return
	}

	pages := (total + pageSize - 1) / pageSize
	if pages < 1 {
		pages = 1
	}

	s.render(w, "recordings.html", listData{
		baseData: s.base(r),
		Calls:    calls,
		Total:    total,
		Page:     page,
		Pages:    pages,
		Query:    f.Query,
		AMD:      f.AMD,
		AMDs:     amdDispositions,
	})
}

type detailData struct {
	baseData
	Call *store.Call
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)

		return
	}

	call, err := s.Store.GetCall(r.Context(), id)
	if err != nil {
		log.Printf("[web] get call %d: %v", id, err)
		http.Error(w, "failed to load recording", http.StatusInternalServerError)

		return
	}

	if call == nil {
		http.NotFound(w, r)

		return
	}

	s.render(w, "detail.html", detailData{baseData: s.base(r), Call: call})
}

// handleAudio streams one leg's WAV. http.ServeContent provides the
// Range support the HTML audio element needs for seeking.
func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request) {
	legID, err := strconv.ParseInt(r.PathValue("legID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)

		return
	}

	leg, err := s.Store.GetLeg(r.Context(), legID)
	if err != nil {
		log.Printf("[web] get leg %d: %v", legID, err)
		http.Error(w, "failed to load audio", http.StatusInternalServerError)

		return
	}

	callID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if leg == nil || leg.CallID != callID {
		http.NotFound(w, r)

		return
	}

	path, err := s.safeWAVPath(leg.WAVPath)
	if err != nil {
		http.NotFound(w, r)

		return
	}

	f, err := os.Open(path) // #nosec G304 -- path confined to RECORDING_DIR by safeWAVPath
	if err != nil {
		http.NotFound(w, r)

		return
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, "failed to stat audio", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "audio/wav")

	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
	}

	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// safeWAVPath confines a stored WAV path to the recording directory —
// a stored path must never let a request escape it.
func (s *Server) safeWAVPath(p string) (string, error) {
	absDir, err := filepath.Abs(s.RecordingDir)
	if err != nil {
		return "", err
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}

	if abs != absDir && !strings.HasPrefix(abs, absDir+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q outside recording dir", p)
	}

	return abs, nil
}

// audit records an administrative action. Failures are logged, not
// fatal: the audit trail must never block the action it describes.
func (s *Server) audit(r *http.Request, action, target, detail string) {
	actor := ""
	if u := auth.CurrentUser(r); u != nil {
		actor = u.Email
	}

	if err := s.Store.Audit(r.Context(), actor, action, target, detail); err != nil {
		log.Printf("[web] audit %q %q: %v", action, target, err) // #nosec G706 -- quoted values
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	user := auth.CurrentUser(r)
	if !s.Auth.CheckCSRF(r, user) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)

		return
	}

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)

		return
	}

	paths, err := s.Store.DeleteCall(r.Context(), id)
	if err != nil {
		log.Printf("[web] delete call %d: %v", id, err)
		http.Error(w, "failed to delete recording", http.StatusInternalServerError)

		return
	}

	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("[web] delete call %d: remove %s: %v", id, p, err)
		}
	}

	log.Printf("[web] admin %q deleted call %d (%d file(s))", user.Email, id, len(paths)) // #nosec G706 -- quoted values
	s.audit(r, "recording.delete", strconv.FormatInt(id, 10),
		fmt.Sprintf("%d file(s) removed", len(paths)))
	http.Redirect(w, r, "/recordings?msg=Recording+deleted", http.StatusSeeOther)
}

// handleFetchRecord pulls the full Call record for this call from
// VoiceML, on request, using the per-account API key an
// administrator configured under Integrations. Any signed-in user may
// refresh: the fetch only reads VoiceML and stores what it
// answered. Errors are reported as page messages, never as raw
// upstream bodies (which can echo account SIDs).
func (s *Server) handleFetchRecord(w http.ResponseWriter, r *http.Request) {
	user := auth.CurrentUser(r)
	if !s.Auth.CheckCSRF(r, user) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)

		return
	}

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)

		return
	}

	detail := "/recordings/" + strconv.FormatInt(id, 10)
	fail := func(msg string) {
		http.Redirect(w, r, detail+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}

	call, err := s.Store.GetCall(r.Context(), id)
	if err != nil {
		log.Printf("[web] fetch-record: get call %d: %v", id, err)
		http.Error(w, "failed to load call", http.StatusInternalServerError)

		return
	}

	if call == nil {
		http.NotFound(w, r)

		return
	}

	switch {
	case call.CallSid == "":
		fail("This call has no CallSid, so there is nothing to fetch. " +
			"Stamp vm-callsid on the SRS URI (or notify it) to link the call.")
		return
	case call.AccountSid == "":
		fail("This call has no AccountSid, so the account's API key can't be selected.")
		return
	case s.VoiceMLBaseURL == "":
		fail("VOICEML_BASE_URL is not configured on this server.")
		return
	}

	acct, err := s.Store.GetVoiceMLAccount(r.Context(), call.AccountSid)
	if err != nil {
		log.Printf("[web] fetch-record: key lookup %s: %v", call.AccountSid, err)
		http.Error(w, "failed to look up VoiceML credentials", http.StatusInternalServerError)

		return
	}

	if acct == nil || acct.APIKey == "" {
		fail("No VoiceML API key is configured for account " + call.AccountSid +
			". An administrator can add one under Integrations.")
		return
	}

	rec, pretty, err := voiceml.New(s.VoiceMLBaseURL).FetchCall(
		r.Context(), call.AccountSid, acct.APIKey, call.CallSid)
	if err != nil {
		log.Printf("[web] fetch-record call %d (CallSid %s): %v", id, call.CallSid, err)
		fail("Fetching the call record from VoiceML failed (" + err.Error() + ").")
		return
	}

	if err := s.Store.SaveVoiceMLRecord(r.Context(), id, rec, pretty); err != nil {
		log.Printf("[web] fetch-record: save %d: %v", id, err)
		http.Error(w, "failed to store the fetched record", http.StatusInternalServerError)

		return
	}

	log.Printf("[web] %q fetched VoiceML record for call %d (CallSid %q, status %q)", // #nosec G706 -- quoted values
		user.Email, id, call.CallSid, rec.Status)
	s.audit(r, "record.fetch", strconv.FormatInt(id, 10),
		"CallSid "+call.CallSid+" status "+rec.Status)
	fail("Call record fetched from VoiceML.")
}

// --- Admin: users -----------------------------------------------------------

type usersData struct {
	baseData
	Users      []store.User
	AdminEmail string
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		log.Printf("[web] list users: %v", err)
		http.Error(w, "failed to list users", http.StatusInternalServerError)

		return
	}

	s.render(w, "users.html", usersData{
		baseData:   s.base(r),
		Users:      users,
		AdminEmail: s.Auth.AdminEmail(),
	})
}

func (s *Server) handleUserUpsert(w http.ResponseWriter, r *http.Request) {
	user := auth.CurrentUser(r)
	if !s.Auth.CheckCSRF(r, user) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)

		return
	}

	email := store.NormalizeEmail(r.FormValue("email"))
	role := r.FormValue("role")

	if email == "" || !strings.Contains(email, "@") {
		http.Redirect(w, r, "/admin/users?msg=Invalid+email+address", http.StatusSeeOther)

		return
	}

	if role != "admin" {
		role = "user"
	}

	err := s.Store.UpsertUser(r.Context(), store.User{
		Email:     email,
		Role:      role,
		CreatedBy: user.Email,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		log.Printf("[web] upsert user %q: %v", email, err) // #nosec G706 -- quoted values
		http.Error(w, "failed to save user", http.StatusInternalServerError)

		return
	}

	log.Printf("[web] admin %q saved user %q role=%s", user.Email, email, role) // #nosec G706 -- quoted values
	s.audit(r, "user.upsert", email, "role="+role)
	http.Redirect(w, r, "/admin/users?msg=User+saved", http.StatusSeeOther)
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	user := auth.CurrentUser(r)
	if !s.Auth.CheckCSRF(r, user) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)

		return
	}

	email := store.NormalizeEmail(r.FormValue("email"))
	if email == s.Auth.AdminEmail() {
		http.Redirect(w, r, "/admin/users?msg=The+bootstrap+administrator+comes+from+ADMIN_EMAIL+and+cannot+be+removed", http.StatusSeeOther)

		return
	}

	if _, err := s.Store.DeleteUser(r.Context(), email); err != nil {
		log.Printf("[web] delete user %q: %v", email, err) // #nosec G706 -- quoted values
		http.Error(w, "failed to delete user", http.StatusInternalServerError)

		return
	}

	log.Printf("[web] admin %q deleted user %q", user.Email, email) // #nosec G706 -- quoted values
	s.audit(r, "user.delete", email, "")
	http.Redirect(w, r, "/admin/users?msg=User+deleted", http.StatusSeeOther)
}

func (s *Server) handleUserRevoke(w http.ResponseWriter, r *http.Request) {
	user := auth.CurrentUser(r)
	if !s.Auth.CheckCSRF(r, user) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)

		return
	}

	email := store.NormalizeEmail(r.FormValue("email"))

	if err := s.Store.BumpSessionEpoch(r.Context(), email); err != nil {
		log.Printf("[web] revoke sessions %q: %v", email, err) // #nosec G706 -- quoted values
		http.Error(w, "failed to revoke sessions", http.StatusInternalServerError)

		return
	}

	log.Printf("[web] admin %q revoked sessions for %q", user.Email, email) // #nosec G706 -- quoted values
	s.audit(r, "user.revoke_sessions", email, "")
	http.Redirect(w, r, "/admin/users?msg=Sessions+revoked", http.StatusSeeOther)
}

// --- Admin: audit log ---------------------------------------------------------

type auditData struct {
	baseData
	Entries []store.AuditEntry
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.Store.ListAudit(r.Context(), 200)
	if err != nil {
		log.Printf("[web] list audit: %v", err)
		http.Error(w, "failed to load audit log", http.StatusInternalServerError)

		return
	}

	s.render(w, "audit.html", auditData{baseData: s.base(r), Entries: entries})
}

// --- Admin: VoiceML integrations --------------------------------------

type integrationsData struct {
	baseData
	Accounts       []store.VoiceMLAccount
	VoiceMLBaseURL string
}

func (s *Server) handleIntegrations(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.Store.ListVoiceMLAccounts(r.Context())
	if err != nil {
		log.Printf("[web] list cb accounts: %v", err)
		http.Error(w, "failed to list integrations", http.StatusInternalServerError)

		return
	}

	s.render(w, "integrations.html", integrationsData{
		baseData:       s.base(r),
		Accounts:       accounts,
		VoiceMLBaseURL: s.VoiceMLBaseURL,
	})
}

func (s *Server) handleIntegrationUpsert(w http.ResponseWriter, r *http.Request) {
	user := auth.CurrentUser(r)
	if !s.Auth.CheckCSRF(r, user) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)

		return
	}

	accountSid := strings.TrimSpace(r.FormValue("account_sid"))
	apiKey := strings.TrimSpace(r.FormValue("api_key"))

	if accountSid == "" || apiKey == "" {
		http.Redirect(w, r, "/admin/integrations?msg=AccountSid+and+API+key+are+both+required", http.StatusSeeOther)

		return
	}

	err := s.Store.UpsertVoiceMLAccount(r.Context(), store.VoiceMLAccount{
		AccountSid: accountSid,
		APIKey:     apiKey,
		CreatedBy:  user.Email,
	})
	if err != nil {
		log.Printf("[web] upsert VoiceML account %q: %v", accountSid, err) // #nosec G706 -- quoted values
		http.Error(w, "failed to save integration", http.StatusInternalServerError)

		return
	}

	log.Printf("[web] admin %q saved VoiceML key for account %q", user.Email, accountSid) // #nosec G706 -- quoted values
	s.audit(r, "voicemlkey.save", accountSid, "")
	http.Redirect(w, r, "/admin/integrations?msg=VoiceML+key+saved", http.StatusSeeOther)
}

func (s *Server) handleIntegrationDelete(w http.ResponseWriter, r *http.Request) {
	user := auth.CurrentUser(r)
	if !s.Auth.CheckCSRF(r, user) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)

		return
	}

	accountSid := strings.TrimSpace(r.FormValue("account_sid"))

	if _, err := s.Store.DeleteVoiceMLAccount(r.Context(), accountSid); err != nil {
		log.Printf("[web] delete VoiceML account %q: %v", accountSid, err) // #nosec G706 -- quoted values
		http.Error(w, "failed to delete integration", http.StatusInternalServerError)

		return
	}

	log.Printf("[web] admin %q deleted VoiceML key for account %q", user.Email, accountSid) // #nosec G706 -- quoted values
	s.audit(r, "voicemlkey.delete", accountSid, "")
	http.Redirect(w, r, "/admin/integrations?msg=VoiceML+key+deleted", http.StatusSeeOther)
}
