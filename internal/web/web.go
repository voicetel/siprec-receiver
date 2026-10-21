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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/voicetel/siprec-receiver/internal/auth"
	"github.com/voicetel/siprec-receiver/internal/store"
)

// Server bundles the HTTP handlers' dependencies.
type Server struct {
	Store        *store.Store
	Auth         *auth.Authenticator
	RecordingDir string
	NotifySecret string

	tmpl *template.Template
}

//go:embed templates/*.html
var templatesFS embed.FS

// amdDispositions is the filter vocabulary for the recordings list —
// callBroadcast's internal AMD verdict set (Twilio's strict wire set
// plus the extended screened/disconnected values).
var amdDispositions = []string{
	"human", "machine_start", "machine_end_beep", "machine_end_silence",
	"machine_end_other", "fax", "unknown", "screened", "screened_ivr",
	"disconnected",
}

// New builds a Server and parses the embedded templates.
func New(st *store.Store, a *auth.Authenticator, recordingDir, notifySecret string) (*Server, error) {
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
		Store:        st,
		Auth:         a,
		RecordingDir: recordingDir,
		NotifySecret: notifySecret,
		tmpl:         tmpl,
	}, nil
}

// Handler returns the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("POST /api/notify/amd", s.handleNotifyAMD)

	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.Handle("GET /auth/login", http.HandlerFunc(s.Auth.HandleLogin))
	mux.Handle("GET /auth/callback", http.HandlerFunc(s.Auth.HandleCallback))
	mux.Handle("GET /auth/logout", http.HandlerFunc(s.Auth.HandleLogout))

	authed := http.NewServeMux()
	authed.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/recordings", http.StatusSeeOther)
	})
	authed.HandleFunc("GET /recordings", s.handleList)
	authed.HandleFunc("GET /recordings/{id}", s.handleDetail)
	authed.HandleFunc("GET /recordings/{id}/legs/{legID}/audio", s.handleAudio)
	authed.Handle("POST /recordings/{id}/delete",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleDelete)))
	authed.Handle("GET /admin/users",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleUsers)))
	authed.Handle("POST /admin/users",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleUserUpsert)))
	authed.Handle("POST /admin/users/delete",
		s.Auth.RequireAdmin(http.HandlerFunc(s.handleUserDelete)))

	mux.Handle("/", s.Auth.RequireAuth(authed))

	return mux
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
		d.CSRF = s.Auth.CSRFToken(u.Email)
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

	f, err := os.Open(path)
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

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	user := auth.CurrentUser(r)
	if !s.Auth.CheckCSRF(r, user.Email) {
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

	log.Printf("[web] admin %s deleted call %d (%d file(s))", user.Email, id, len(paths))
	http.Redirect(w, r, "/recordings?msg=Recording+deleted", http.StatusSeeOther)
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
	if !s.Auth.CheckCSRF(r, user.Email) {
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
		log.Printf("[web] upsert user %s: %v", email, err)
		http.Error(w, "failed to save user", http.StatusInternalServerError)

		return
	}

	log.Printf("[web] admin %s saved user %s role=%s", user.Email, email, role)
	http.Redirect(w, r, "/admin/users?msg=User+saved", http.StatusSeeOther)
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	user := auth.CurrentUser(r)
	if !s.Auth.CheckCSRF(r, user.Email) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)

		return
	}

	email := store.NormalizeEmail(r.FormValue("email"))
	if email == s.Auth.AdminEmail() {
		http.Redirect(w, r, "/admin/users?msg=The+bootstrap+administrator+comes+from+ADMIN_EMAIL+and+cannot+be+removed", http.StatusSeeOther)

		return
	}

	if _, err := s.Store.DeleteUser(r.Context(), email); err != nil {
		log.Printf("[web] delete user %s: %v", email, err)
		http.Error(w, "failed to delete user", http.StatusInternalServerError)

		return
	}

	log.Printf("[web] admin %s deleted user %s", user.Email, email)
	http.Redirect(w, r, "/admin/users?msg=User+deleted", http.StatusSeeOther)
}
