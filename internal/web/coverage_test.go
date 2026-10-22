package web

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/auth"
	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
	"github.com/voicetel/siprec-receiver/internal/store"
)

type covOpts struct {
	notifySecret     string
	voicemlBaseURL   string
	retentionDefault int
	recDir           string
}

// covEnv is a fixture whose database path is known, so tests can
// break the schema from a second connection, and whose *Server is
// reachable for direct handler/helper calls.
type covEnv struct {
	*fixture
	ws     *Server
	dbPath string
}

func newCovEnv(t *testing.T, o covOpts) *covEnv {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "cov.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	authn := auth.New(auth.Config{
		SessionSecret: sessSecret,
		AdminEmail:    adminEmail,
		DevEmail:      adminEmail,
	}, st)

	recDir := o.recDir
	if recDir == "" {
		recDir = t.TempDir()
	}

	ws, err := New(st, authn, recDir, o.notifySecret, o.voicemlBaseURL, o.retentionDefault)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	srv := httptest.NewServer(ws.Handler())
	t.Cleanup(srv.Close)

	return &covEnv{
		fixture: &fixture{srv: srv, store: st, auth: authn, recDir: recDir},
		ws:      ws,
		dbPath:  dbPath,
	}
}

// execSQL runs statements on a second connection to the env's
// database (used to break one table while the rest keeps working).
func (e *covEnv) execSQL(t *testing.T, stmts ...string) {
	t.Helper()

	db, err := sql.Open("sqlite3", e.dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	defer func() { _ = db.Close() }()

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

// adminSession signs in as the bootstrap admin and returns a client
// that does not follow redirects plus a valid CSRF token. The
// bootstrap admin is authorized without a database read, so the
// session keeps working after the store is closed.
func (e *covEnv) adminSession(t *testing.T) (*http.Client, string) {
	t.Helper()

	c := e.loginClient(t)
	token := csrfFromPage(t, c, e.srv.URL+"/admin/users")

	return &http.Client{
		Jar: c.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, token
}

type covResp struct {
	status int
	loc    string
	body   string
	header http.Header
}

func covDo(t *testing.T, c *http.Client, req *http.Request) covResp {
	t.Helper()

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}

	defer func() { _ = resp.Body.Close() }()

	b, _ := io.ReadAll(resp.Body)

	return covResp{
		status: resp.StatusCode,
		loc:    resp.Header.Get("Location"),
		body:   string(b),
		header: resp.Header,
	}
}

func covGet(t *testing.T, c *http.Client, u string) covResp {
	t.Helper()

	req, _ := http.NewRequest(http.MethodGet, u, nil)

	return covDo(t, c, req)
}

func covPost(t *testing.T, c *http.Client, u string, form url.Values) covResp {
	t.Helper()

	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return covDo(t, c, req)
}

func covNotify(t *testing.T, u, contentType, body string, hdr map[string]string) covResp {
	t.Helper()

	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)

	for k, v := range hdr {
		req.Header.Set(k, v)
	}

	return covDo(t, http.DefaultClient, req)
}

func wantStatus(t *testing.T, what string, r covResp, want int) {
	t.Helper()

	if r.status != want {
		t.Fatalf("%s = %d (%q), want %d", what, r.status, r.body, want)
	}
}

func wantRedirect(t *testing.T, what string, r covResp, locSubstr string) {
	t.Helper()

	if r.status != http.StatusSeeOther || !strings.Contains(r.loc, locSubstr) {
		t.Fatalf("%s = %d Location %q, want 303 containing %q", what, r.status, r.loc, locSubstr)
	}
}

// lockedBuf is a goroutine-safe log sink.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.String()
}

func captureLog(t *testing.T) *lockedBuf {
	t.Helper()

	buf := &lockedBuf{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	return buf
}

func recordLeg(t *testing.T, st *store.Store, leg siprecsrv.FinalizedLeg) int64 {
	t.Helper()

	if leg.StartedAt.IsZero() {
		leg.StartedAt = time.Now().UTC()
	}

	if err := st.RecordLeg(context.Background(), leg); err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	calls, _, err := st.ListCalls(context.Background(), store.ListFilter{Query: leg.CallID})
	if err != nil || len(calls) == 0 {
		t.Fatalf("ListCalls %s: %v (%d)", leg.CallID, err, len(calls))
	}

	return calls[0].ID
}

func TestCov_HealthzAndRootRedirect(t *testing.T) {
	e := newCovEnv(t, covOpts{})

	r := covGet(t, http.DefaultClient, e.srv.URL+"/healthz")
	if r.status != http.StatusOK || r.body != "ok\n" || r.header.Get("Content-Type") != "text/plain" {
		t.Fatalf("healthz = %d %q %q", r.status, r.body, r.header.Get("Content-Type"))
	}

	admin, _ := e.adminSession(t)
	wantRedirect(t, "GET /", covGet(t, admin, e.srv.URL+"/"), "/recordings")
}

func TestCov_TemplateFuncs(t *testing.T) {
	e := newCovEnv(t, covOpts{})

	tmpl, err := e.ws.tmpl.Clone()
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	tmpl, err = tmpl.New("cov").Parse(
		`{{fmtTime .Zero}}|{{fmtTime .T}}|{{fmtDur 0}}|{{fmtDur 125}}|` +
			`{{fmtBytes 512}}|{{fmtBytes 2048}}|{{fmtBytes 3145728}}|{{add 1 2}}|{{subtract 5 2}}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var out bytes.Buffer

	err = tmpl.Execute(&out, map[string]time.Time{
		"Zero": {},
		"T":    time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600)),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	want := "—|2026-01-02 02:04:05 UTC|—|2m 05s|512 B|2.0 KiB|3.0 MiB|3|3"
	if out.String() != want {
		t.Fatalf("template funcs = %q, want %q", out.String(), want)
	}
}

// failingWriter is a ResponseWriter whose body writes fail, as when
// the client has gone away mid-response.
type failingWriter struct{ h http.Header }

func (f *failingWriter) Header() http.Header       { return f.h }
func (f *failingWriter) WriteHeader(int)           {}
func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }

func TestCov_RenderWriteErrorIsLogged(t *testing.T) {
	e := newCovEnv(t, covOpts{})
	logs := captureLog(t)

	w := &failingWriter{h: http.Header{}}
	e.ws.render(w, "login.html", struct {
		baseData
		AuthEnabled bool
	}{})

	if ct := w.h.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}

	if !strings.Contains(logs.String(), "[web] render login.html: ") ||
		!strings.Contains(logs.String(), "client gone") {
		t.Fatalf("render error not logged: %q", logs.String())
	}
}

func TestCov_ListFiltersAndPaging(t *testing.T) {
	e := newCovEnv(t, covOpts{})
	e.seedCall(t)
	admin, _ := e.adminSession(t)

	r := covGet(t, admin, e.srv.URL+"/recordings?page=x")
	wantStatus(t, "list", r, http.StatusOK)

	if !strings.Contains(r.body, "dlg-1") && !strings.Contains(r.body, "12165285911") {
		t.Errorf("list page missing the seeded call")
	}

	q := url.Values{
		"page": {"2"}, "q": {" dlg "}, "from_number": {"1216"}, "to_number": {"1208"},
		"start_date": {"2020-01-01"}, "end_date": {"2099-12-31"},
		"amd": {"human"}, "disposition": {"completed"},
	}
	r = covGet(t, admin, e.srv.URL+"/recordings?"+q.Encode())
	wantStatus(t, "filtered list", r, http.StatusOK)

	// Page 2 of 1 renders a Prev link carrying every filter (they used to
	// be escaped into one unusable "%26amd%3d..." parameter), and the
	// search form echoes every filter back.
	m := regexp.MustCompile(`<a href="([^"]+)">← Prev</a>`).FindStringSubmatch(r.body)
	if m == nil {
		t.Fatalf("filtered page 2 has no Prev link:\n%s", r.body)
	}

	prev, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil {
		t.Fatalf("Prev link %q: %v", m[1], err)
	}

	got := prev.Query()
	for key, want := range map[string]string{
		"page": "1", "q": "dlg", "from_number": "1216", "to_number": "1208",
		"start_date": "2020-01-01", "end_date": "2099-12-31", "amd": "human", "disposition": "completed",
	} {
		if got.Get(key) != want {
			t.Errorf("Prev link %s = %q, want %q (link %q)", key, got.Get(key), want, prev)
		}
	}

	for _, want := range []string{`value="dlg"`, `value="1216"`, `value="1208"`,
		`value="2020-01-01"`, `value="2099-12-31"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("filtered list does not echo %s", want)
		}
	}

	// An end date before the call excludes it.
	r = covGet(t, admin, e.srv.URL+"/recordings?end_date=2000-01-01")
	if strings.Contains(r.body, "12165285911") {
		t.Errorf("end_date filter did not exclude the call")
	}

	_ = e.store.Close()
	wantStatus(t, "list with closed store",
		covGet(t, admin, e.srv.URL+"/recordings"), http.StatusInternalServerError)
}

func TestCov_DetailErrors(t *testing.T) {
	e := newCovEnv(t, covOpts{})
	admin, _ := e.adminSession(t)

	wantStatus(t, "detail bad id", covGet(t, admin, e.srv.URL+"/recordings/abc"), http.StatusNotFound)
	wantStatus(t, "detail missing", covGet(t, admin, e.srv.URL+"/recordings/999"), http.StatusNotFound)

	_ = e.store.Close()
	wantStatus(t, "detail closed store",
		covGet(t, admin, e.srv.URL+"/recordings/1"), http.StatusInternalServerError)
}

func TestCov_Audio(t *testing.T) {
	e := newCovEnv(t, covOpts{})
	callID, legID := e.seedCall(t)
	admin, _ := e.adminSession(t)

	base := e.srv.URL + "/recordings/" + itoa(callID) + "/legs/"

	r := covGet(t, admin, base+itoa(legID)+"/audio?download=1")
	wantStatus(t, "download", r, http.StatusOK)

	if cd := r.header.Get("Content-Disposition"); cd != `attachment; filename="dlg-1-12165285911.wav"` {
		t.Errorf("Content-Disposition = %q", cd)
	}

	if ct := r.header.Get("Content-Type"); ct != "audio/wav" || r.body != "RIFF-fake-wav-bytes" {
		t.Errorf("download = %q %q", ct, r.body)
	}

	wantStatus(t, "bad leg id", covGet(t, admin, base+"abc/audio"), http.StatusNotFound)
	wantStatus(t, "unknown leg", covGet(t, admin, base+"999/audio"), http.StatusNotFound)
	wantStatus(t, "leg of another call",
		covGet(t, admin, e.srv.URL+"/recordings/"+itoa(callID+1)+"/legs/"+itoa(legID)+"/audio"),
		http.StatusNotFound)

	// A leg inside the recording dir whose file is gone.
	goneCall := recordLeg(t, e.store, siprecsrv.FinalizedLeg{
		CallID: "dlg-gone", Label: "a", WAVPath: filepath.Join(e.recDir, "gone.wav"), DurationSec: 1,
	})
	gone, _ := e.store.GetCall(context.Background(), goneCall)
	wantStatus(t, "missing wav",
		covGet(t, admin, e.srv.URL+"/recordings/"+itoa(goneCall)+"/legs/"+itoa(gone.Legs[0].ID)+"/audio"),
		http.StatusNotFound)

	_ = e.store.Close()
	wantStatus(t, "audio closed store", covGet(t, admin, base+itoa(legID)+"/audio"),
		http.StatusInternalServerError)
}

// safeWAVPath resolves relative paths against the working directory;
// when that directory no longer exists, resolution fails and the
// path is refused.
func TestCov_SafeWAVPathUnresolvable(t *testing.T) {
	wd := filepath.Join(t.TempDir(), "wd")
	if err := os.Mkdir(wd, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	t.Chdir(wd)

	if err := os.Remove(wd); err != nil {
		t.Fatalf("remove wd: %v", err)
	}

	rel := &Server{RecordingDir: "recordings"}
	if _, err := rel.safeWAVPath("/abs/x.wav"); err == nil {
		t.Error("relative RecordingDir with a deleted cwd resolved")
	}

	abs := &Server{RecordingDir: "/srv/recordings"}
	if _, err := abs.safeWAVPath("x.wav"); err == nil {
		t.Error("relative WAV path with a deleted cwd resolved")
	}

	if got, err := abs.safeWAVPath("/srv/recordings"); err != nil || got != "/srv/recordings" {
		t.Errorf("recording dir itself = %q, %v", got, err)
	}
}

func TestCov_Delete(t *testing.T) {
	e := newCovEnv(t, covOpts{})
	logs := captureLog(t)
	admin, csrf := e.adminSession(t)

	// A leg "file" that is a non-empty directory cannot be removed;
	// the delete still succeeds and logs the leftover.
	stuck := filepath.Join(e.recDir, "stuck.wav")
	if err := os.MkdirAll(filepath.Join(stuck, "child"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	callID := recordLeg(t, e.store, siprecsrv.FinalizedLeg{
		CallID: "dlg-stuck", Label: "a", WAVPath: stuck, DurationSec: 1,
	})

	del := func(id string, form url.Values) covResp {
		return covPost(t, admin, e.srv.URL+"/recordings/"+id+"/delete", form)
	}

	wantStatus(t, "delete bad csrf", del(itoa(callID), url.Values{"csrf_token": {"nope"}}), http.StatusForbidden)
	wantStatus(t, "delete bad id", del("abc", url.Values{"csrf_token": {csrf}}), http.StatusNotFound)

	wantRedirect(t, "delete", del(itoa(callID), url.Values{"csrf_token": {csrf}}), "Recording+deleted")

	if c, _ := e.store.GetCall(context.Background(), callID); c != nil {
		t.Error("call survived delete")
	}

	if !strings.Contains(logs.String(), "remove "+stuck) {
		t.Errorf("failed file removal not logged: %q", logs.String())
	}

	_ = e.store.Close()
	wantStatus(t, "delete closed store", del(itoa(callID), url.Values{"csrf_token": {csrf}}),
		http.StatusInternalServerError)
}

func TestCov_FetchRecordBranches(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb") {
			http.Error(w, "boom", http.StatusInternalServerError)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sid":"CA99999999999999999999999999999999","account_sid":"ACcccccccccccccccccccccccccccccccc","status":"completed"}`))
	}))
	defer upstream.Close()

	e := newCovEnv(t, covOpts{voicemlBaseURL: upstream.URL})
	admin, csrf := e.adminSession(t)
	ok := url.Values{"csrf_token": {csrf}}

	fetch := func(id string, form url.Values) covResp {
		return covPost(t, admin, e.srv.URL+"/recordings/"+id+"/fetch-record", form)
	}

	noSid := recordLeg(t, e.store, siprecsrv.FinalizedLeg{CallID: "dlg-nosid", Label: "a", WAVPath: "/x/a.wav"})
	noAcct := recordLeg(t, e.store, siprecsrv.FinalizedLeg{
		CallID: "dlg-noacct", CallSid: "CAnoacct", Label: "a", WAVPath: "/x/b.wav",
	})
	broken := recordLeg(t, e.store, siprecsrv.FinalizedLeg{
		CallID: "dlg-broken", CallSid: "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", AccountSid: "ACcccccccccccccccccccccccccccccccc", Label: "a", WAVPath: "/x/c.wav",
	})
	good := recordLeg(t, e.store, siprecsrv.FinalizedLeg{
		CallID: "dlg-good", CallSid: "CA99999999999999999999999999999999", AccountSid: "ACcccccccccccccccccccccccccccccccc", Label: "a", WAVPath: "/x/d.wav",
	})

	if err := e.store.UpsertVoiceMLAccount(context.Background(),
		store.VoiceMLAccount{AccountSid: "ACcccccccccccccccccccccccccccccccc", APIKey: "k", CreatedBy: adminEmail}); err != nil {
		t.Fatalf("UpsertVoiceMLAccount: %v", err)
	}

	wantStatus(t, "bad csrf", fetch(itoa(good), url.Values{"csrf_token": {"x"}}), http.StatusForbidden)
	wantStatus(t, "bad id", fetch("abc", ok), http.StatusNotFound)
	wantStatus(t, "unknown call", fetch("999", ok), http.StatusNotFound)
	wantRedirect(t, "no CallSid", fetch(itoa(noSid), ok), "no+CallSid")
	wantRedirect(t, "no AccountSid", fetch(itoa(noAcct), ok), "no+AccountSid")

	r := fetch(itoa(broken), ok)
	wantRedirect(t, "upstream error", r, "/recordings/"+itoa(broken)+"?msg=Fetching+the+call+record+from+VoiceML+failed")

	// Saving the fetched record fails; nothing upstream echoes back.
	e.execSQL(t, `CREATE TRIGGER cov_no_update BEFORE UPDATE ON calls BEGIN SELECT RAISE(ABORT, 'read-only'); END`)
	wantStatus(t, "save failure", fetch(itoa(good), ok), http.StatusInternalServerError)

	e.execSQL(t, `DROP TRIGGER cov_no_update`, `ALTER TABLE voiceml_accounts RENAME TO cov_gone`)
	wantStatus(t, "key lookup failure", fetch(itoa(good), ok), http.StatusInternalServerError)

	_ = e.store.Close()
	wantStatus(t, "closed store", fetch(itoa(good), ok), http.StatusInternalServerError)

	// Without a configured VoiceML base URL nothing is fetched.
	e2 := newCovEnv(t, covOpts{})
	admin2, csrf2 := e2.adminSession(t)
	id := seedCallWithIDs(t, e2.fixture)
	wantRedirect(t, "no base URL",
		covPost(t, admin2, e2.srv.URL+"/recordings/"+itoa(id)+"/fetch-record", url.Values{"csrf_token": {csrf2}}),
		"VOICEML_BASE_URL")
}

func TestCov_Users(t *testing.T) {
	e := newCovEnv(t, covOpts{})
	logs := captureLog(t)
	admin, csrf := e.adminSession(t)
	u := e.srv.URL

	wantStatus(t, "upsert bad csrf", covPost(t, admin, u+"/admin/users", url.Values{"email": {"a@b.c"}}),
		http.StatusForbidden)
	wantRedirect(t, "upsert invalid email",
		covPost(t, admin, u+"/admin/users", url.Values{"email": {"nope"}, "csrf_token": {csrf}}),
		"Invalid+email")
	wantRedirect(t, "upsert admin",
		covPost(t, admin, u+"/admin/users", url.Values{
			"email": {"Boss@Example.com"}, "role": {"admin"}, "csrf_token": {csrf},
		}), "User+saved")

	if got, _ := e.store.GetUser(context.Background(), "boss@example.com"); got == nil || got.Role != "admin" {
		t.Fatalf("admin user not stored: %+v", got)
	}

	wantStatus(t, "delete bad csrf", covPost(t, admin, u+"/admin/users/delete", url.Values{"email": {"boss@example.com"}}),
		http.StatusForbidden)
	wantRedirect(t, "delete bootstrap admin",
		covPost(t, admin, u+"/admin/users/delete", url.Values{"email": {adminEmail}, "csrf_token": {csrf}}),
		"cannot+be+removed")

	before, _ := e.store.GetUser(context.Background(), "boss@example.com")

	wantStatus(t, "revoke bad csrf", covPost(t, admin, u+"/admin/users/revoke", url.Values{"email": {"boss@example.com"}}),
		http.StatusForbidden)
	wantRedirect(t, "revoke",
		covPost(t, admin, u+"/admin/users/revoke", url.Values{"email": {"boss@example.com"}, "csrf_token": {csrf}}),
		"Sessions+revoked")

	if got, _ := e.store.GetUser(context.Background(), "boss@example.com"); got.SessionEpoch != before.SessionEpoch+1 {
		t.Errorf("session epoch = %d, want %d", got.SessionEpoch, before.SessionEpoch+1)
	}

	// A broken audit trail never blocks the action it describes.
	e.execSQL(t, `ALTER TABLE audit_log RENAME TO cov_gone`)
	wantRedirect(t, "delete user without audit",
		covPost(t, admin, u+"/admin/users/delete", url.Values{"email": {"boss@example.com"}, "csrf_token": {csrf}}),
		"User+deleted")

	if got, _ := e.store.GetUser(context.Background(), "boss@example.com"); got != nil {
		t.Error("user survived delete")
	}

	if !strings.Contains(logs.String(), `[web] audit "user.delete" "boss@example.com"`) {
		t.Errorf("audit failure not logged: %q", logs.String())
	}

	_ = e.store.Close()

	wantStatus(t, "list closed", covGet(t, admin, u+"/admin/users"), http.StatusInternalServerError)
	wantStatus(t, "upsert closed",
		covPost(t, admin, u+"/admin/users", url.Values{"email": {"x@y.z"}, "csrf_token": {csrf}}),
		http.StatusInternalServerError)
	wantStatus(t, "delete closed",
		covPost(t, admin, u+"/admin/users/delete", url.Values{"email": {"x@y.z"}, "csrf_token": {csrf}}),
		http.StatusInternalServerError)
	wantStatus(t, "revoke closed",
		covPost(t, admin, u+"/admin/users/revoke", url.Values{"email": {"x@y.z"}, "csrf_token": {csrf}}),
		http.StatusInternalServerError)
	wantStatus(t, "audit closed", covGet(t, admin, u+"/admin/audit"), http.StatusInternalServerError)
}

func TestCov_Integrations(t *testing.T) {
	e := newCovEnv(t, covOpts{voicemlBaseURL: " https://vm.example.com/ "})
	admin, csrf := e.adminSession(t)
	u := e.srv.URL

	if e.ws.VoiceMLBaseURL != "https://vm.example.com" {
		t.Errorf("VoiceMLBaseURL = %q, want trimmed", e.ws.VoiceMLBaseURL)
	}

	wantStatus(t, "upsert bad csrf",
		covPost(t, admin, u+"/admin/integrations", url.Values{"account_sid": {"AC1"}, "api_key": {"k"}}),
		http.StatusForbidden)
	wantRedirect(t, "upsert missing key",
		covPost(t, admin, u+"/admin/integrations", url.Values{"account_sid": {"AC1"}, "csrf_token": {csrf}}),
		"both+required")
	wantRedirect(t, "upsert",
		covPost(t, admin, u+"/admin/integrations", url.Values{
			"account_sid": {"AC1"}, "api_key": {"k"}, "csrf_token": {csrf},
		}), "key+saved")

	r := covGet(t, admin, u+"/admin/integrations")
	if r.status != http.StatusOK || !strings.Contains(r.body, "AC1") {
		t.Fatalf("integrations page = %d, missing AC1", r.status)
	}

	wantStatus(t, "delete bad csrf",
		covPost(t, admin, u+"/admin/integrations/delete", url.Values{"account_sid": {"AC1"}}),
		http.StatusForbidden)
	wantRedirect(t, "delete",
		covPost(t, admin, u+"/admin/integrations/delete", url.Values{"account_sid": {" AC1 "}, "csrf_token": {csrf}}),
		"key+deleted")

	if a, _ := e.store.GetVoiceMLAccount(context.Background(), "AC1"); a != nil {
		t.Error("integration survived delete")
	}

	_ = e.store.Close()

	wantStatus(t, "list closed", covGet(t, admin, u+"/admin/integrations"), http.StatusInternalServerError)
	wantStatus(t, "upsert closed",
		covPost(t, admin, u+"/admin/integrations", url.Values{
			"account_sid": {"AC1"}, "api_key": {"k"}, "csrf_token": {csrf},
		}), http.StatusInternalServerError)
	wantStatus(t, "delete closed",
		covPost(t, admin, u+"/admin/integrations/delete", url.Values{"account_sid": {"AC1"}, "csrf_token": {csrf}}),
		http.StatusInternalServerError)
}

func TestCov_Settings(t *testing.T) {
	e := newCovEnv(t, covOpts{retentionDefault: 0})
	admin, csrf := e.adminSession(t)
	u := e.srv.URL

	r := covGet(t, admin, u+"/admin/settings")
	wantStatus(t, "settings page", r, http.StatusOK)

	if !strings.Contains(r.body, "never") {
		t.Errorf("settings page should report the purge never ran")
	}

	save := func(form url.Values) covResp { return covPost(t, admin, u+"/admin/settings", form) }
	purge := func(form url.Values) covResp { return covPost(t, admin, u+"/admin/settings/purge", form) }

	wantStatus(t, "save bad csrf", save(url.Values{"retention_days": {"30"}}), http.StatusForbidden)

	for _, bad := range []string{"abc", "-1", "3651"} {
		wantRedirect(t, "save "+bad, save(url.Values{"retention_days": {bad}, "csrf_token": {csrf}}),
			"must+be+a+whole+number")
	}

	wantStatus(t, "purge bad csrf", purge(url.Values{}), http.StatusForbidden)
	wantRedirect(t, "purge disabled", purge(url.Values{"csrf_token": {csrf}}), "Retention+is+disabled")

	wantRedirect(t, "save", save(url.Values{"retention_days": {" 30 "}, "csrf_token": {csrf}}), "Retention+setting+saved")

	if v, ok, _ := e.store.GetSetting(context.Background(), store.SettingRetentionDays); !ok || v != "30" {
		t.Fatalf("retention setting = %q, %v", v, ok)
	}

	old := recordLeg(t, e.store, siprecsrv.FinalizedLeg{
		CallID: "dlg-old", Label: "a", WAVPath: filepath.Join(e.recDir, "old.wav"),
		StartedAt: time.Now().AddDate(0, 0, -100),
	})

	r = purge(url.Values{"csrf_token": {csrf}})
	wantRedirect(t, "purge", r, "Purged+1+expired+call")

	if c, _ := e.store.GetCall(context.Background(), old); c != nil {
		t.Error("expired call survived purge")
	}

	r = covGet(t, admin, u+"/admin/settings")
	if r.status != http.StatusOK || strings.Contains(r.body, "Last purge: never") {
		t.Errorf("settings page after purge = %d, still says never", r.status)
	}

	// The purge cannot read leg paths.
	e.execSQL(t, `ALTER TABLE legs RENAME TO cov_legs`)
	wantStatus(t, "purge failure", purge(url.Values{"csrf_token": {csrf}}), http.StatusInternalServerError)

	// Settings load but the call count does not.
	e.execSQL(t, `ALTER TABLE cov_legs RENAME TO legs`, `ALTER TABLE calls RENAME TO cov_calls`)
	wantStatus(t, "settings call count failure", covGet(t, admin, u+"/admin/settings"), http.StatusInternalServerError)

	// The retention setting itself is unreadable.
	e.execSQL(t, `ALTER TABLE cov_calls RENAME TO calls`, `ALTER TABLE settings RENAME TO cov_settings`)
	wantStatus(t, "settings read failure", covGet(t, admin, u+"/admin/settings"), http.StatusInternalServerError)

	_ = e.store.Close()
	wantStatus(t, "save closed", save(url.Values{"retention_days": {"5"}, "csrf_token": {csrf}}),
		http.StatusInternalServerError)
	wantStatus(t, "purge closed", purge(url.Values{"csrf_token": {csrf}}), http.StatusInternalServerError)
}

func TestCov_NotifyDisabled(t *testing.T) {
	e := newCovEnv(t, covOpts{notifySecret: ""})

	for _, p := range []string{"/api/notify/amd", "/api/notify/disposition"} {
		r := covNotify(t, e.srv.URL+p, "application/x-www-form-urlencoded", "AnsweredBy=human",
			map[string]string{"Authorization": "Bearer anything"})
		wantStatus(t, p+" disabled", r, http.StatusServiceUnavailable)
	}
}

func TestCov_NotifyErrors(t *testing.T) {
	e := newCovEnv(t, covOpts{notifySecret: secret})
	hdr := map[string]string{"X-Notify-Secret": secret}
	form := "application/x-www-form-urlencoded"

	for _, p := range []string{"/api/notify/amd", "/api/notify/disposition"} {
		big := "AnsweredBy=human&CallStatus=completed&pad=" + strings.Repeat("x", 70<<10)
		wantStatus(t, p+" oversized", covNotify(t, e.srv.URL+p, form, big, hdr), http.StatusBadRequest)

		r := covNotify(t, e.srv.URL+p, form, "AnsweredBy=human&CallStatus=completed", hdr)
		wantStatus(t, p+" no key", r, http.StatusBadRequest)

		if !strings.Contains(r.body, "correlation key") {
			t.Errorf("%s no-key body = %q", p, r.body)
		}

		wantStatus(t, p+" empty bearer",
			covNotify(t, e.srv.URL+p, form, "AnsweredBy=human", map[string]string{"Authorization": "Bearer "}),
			http.StatusUnauthorized)
	}

	_ = e.store.Close()

	for _, p := range []string{"/api/notify/amd", "/api/notify/disposition"} {
		r := covNotify(t, e.srv.URL+p, form, "AnsweredBy=human&CallStatus=completed&CallSid=CAx", hdr)
		wantStatus(t, p+" closed store", r, http.StatusInternalServerError)
	}
}

func TestCov_NotifyJSONAndQuery(t *testing.T) {
	e := newCovEnv(t, covOpts{notifySecret: secret})
	logs := captureLog(t)
	callID := seedCallWithIDs(t, e.fixture)
	bearer := map[string]string{"Authorization": "Bearer " + secret}

	// snake_case JSON, numbers, and an ignored non-string/number value.
	r := covNotify(t, e.srv.URL+"/api/notify/amd", "application/json; charset=utf-8",
		`{"call_sid":"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","answered_by":"fax",`+
			`"machine_detection_duration":2500,"call_status":"in-progress","flag":true}`, bearer)
	if r.status != http.StatusOK || !strings.Contains(r.body, `"matched":true`) {
		t.Fatalf("json amd = %d %s", r.status, r.body)
	}

	c, _ := e.store.GetCall(context.Background(), callID)
	if c.AMDAnsweredBy != "fax" || c.AMDDurationMs != 2500 || !strings.Contains(c.AMDRaw, `"flag":true`) {
		t.Errorf("json amd stored %q/%d raw %q", c.AMDAnsweredBy, c.AMDDurationMs, c.AMDRaw)
	}

	r = covNotify(t, e.srv.URL+"/api/notify/disposition", "application/json",
		`{"call_sid":"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","call_status":"busy","call_duration":7,`+
			`"sip_response_code":486,"error_code":"E1","error_message":"busy here"}`, bearer)
	wantStatus(t, "json disposition", r, http.StatusOK)

	c, _ = e.store.GetCall(context.Background(), callID)
	if c.DispStatus != "busy" || c.DispDurationSec != 7 || c.DispSipCode != 486 ||
		c.DispErrorCode != "E1" || c.DispErrorMessage != "busy here" {
		t.Errorf("json disposition stored %+v", c)
	}

	// Malformed JSON carries no fields.
	wantStatus(t, "bad json",
		covNotify(t, e.srv.URL+"/api/notify/amd", "application/json", `{not json`, bearer),
		http.StatusBadRequest)

	// ?secret= authorizes (warning once); query parameters fill in
	// fields the body lacks but never override the body.
	q := url.Values{"secret": {secret}, "AnsweredBy": {"human"}, "CallSid": {"CAquery"}}
	body := url.Values{"CallSid": {"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}.Encode()

	for range 2 {
		r = covNotify(t, e.srv.URL+"/api/notify/amd?"+q.Encode(), "application/x-www-form-urlencoded", body, nil)
		if r.status != http.StatusOK || !strings.Contains(r.body, `"matched":true`) {
			t.Fatalf("query-secret amd = %d %s", r.status, r.body)
		}
	}

	if n := strings.Count(logs.String(), "presented as ?secret= query parameter"); n != 1 {
		t.Errorf("query-secret warning logged %d times, want 1", n)
	}

	c, _ = e.store.GetCall(context.Background(), callID)
	if c.AMDAnsweredBy != "human" || strings.Contains(c.AMDRaw, secret) {
		t.Errorf("query amd stored %q raw %q", c.AMDAnsweredBy, c.AMDRaw)
	}

	// A wrong query secret falls through to the headers.
	r = covNotify(t, e.srv.URL+"/api/notify/amd?secret=wrong", "application/x-www-form-urlencoded",
		"AnsweredBy=human&CallSid=CAother&Caller=%2B1555&Called=%2B1666", bearer)
	wantStatus(t, "wrong query secret, good bearer", r, http.StatusOK)

	r = covNotify(t, e.srv.URL+"/api/notify/amd?secret=wrong", "application/x-www-form-urlencoded",
		"AnsweredBy=human&CallSid=CAother", nil)
	wantStatus(t, "wrong query secret only", r, http.StatusUnauthorized)
}
