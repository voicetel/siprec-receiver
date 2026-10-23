package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/recording"
	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
	"github.com/voicetel/siprec-receiver/internal/store"
)

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

func waitForLog(t *testing.T, logs *lockedBuf, want string) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for log %q; got:\n%s", want, logs.String())
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// configEnv lists every variable loadConfig reads; clearEnv blanks
// them so the host environment cannot leak into a test.
var configEnv = []string{
	"SIPREC_NETWORKS", "SIPREC_ALLOW_CIDRS", "SIPREC_LISTEN", "SIPREC_LOCAL_IP",
	"SIP_TLS_LISTEN", "SIP_TLS_CERT", "SIP_TLS_KEY", "RTP_PORT_MIN", "RTP_PORT_MAX",
	"RECORDING_DIR", "DB_PATH", "HTTP_LISTEN", "PUBLIC_BASE_URL", "GOOGLE_CLIENT_ID",
	"GOOGLE_CLIENT_SECRET", "GOOGLE_HOSTED_DOMAIN", "SESSION_SECRET", "ADMIN_EMAIL",
	"NOTIFY_SECRET", "DEV_AUTH_EMAIL", "RETENTION_DAYS", "VOICEML_BASE_URL",
	"SIPREC_MAX_SESSIONS", "SIPREC_SESSION_IDLE_SEC",
}

func clearEnv(t *testing.T) {
	t.Helper()

	for _, k := range configEnv {
		t.Setenv(k, "")
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("COV_ENV_OR", "")

	if got := envOr("COV_ENV_OR", "def"); got != "def" {
		t.Errorf("unset = %q", got)
	}

	t.Setenv("COV_ENV_OR", "   ")

	if got := envOr("COV_ENV_OR", "def"); got != "def" {
		t.Errorf("blank = %q", got)
	}

	t.Setenv("COV_ENV_OR", "  val ")

	if got := envOr("COV_ENV_OR", "def"); got != "val" {
		t.Errorf("set = %q", got)
	}
}

func TestEnvInt(t *testing.T) {
	logs := captureLog(t)

	t.Setenv("COV_ENV_INT", "")

	if got := envInt("COV_ENV_INT", 7); got != 7 {
		t.Errorf("unset = %d", got)
	}

	t.Setenv("COV_ENV_INT", " 42 ")

	if got := envInt("COV_ENV_INT", 7); got != 42 {
		t.Errorf("set = %d", got)
	}

	t.Setenv("COV_ENV_INT", "4x")

	if got := envInt("COV_ENV_INT", 7); got != 7 {
		t.Errorf("invalid = %d", got)
	}

	if !strings.Contains(logs.String(), `invalid COV_ENV_INT="4x", using 7`) {
		t.Errorf("invalid value not logged: %q", logs.String())
	}
}

func TestParseCIDRs(t *testing.T) {
	got, err := parseCIDRs("")
	if err != nil || got != nil {
		t.Fatalf("empty = %v, %v", got, err)
	}

	got, err = parseCIDRs(" 10.0.0.0/8, 192.168.1.5 ,, ::1, 2001:db8::/32 ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	want := []string{"10.0.0.0/8", "192.168.1.5/32", "::1/128", "2001:db8::/32"}
	if len(got) != len(want) {
		t.Fatalf("got %d nets, want %d", len(got), len(want))
	}

	for i, n := range got {
		if n.String() != want[i] {
			t.Errorf("net %d = %s, want %s", i, n, want[i])
		}
	}

	for _, bad := range []string{"bogus", "10.0.0.0/33"} {
		if _, err := parseCIDRs("10.0.0.1," + bad); err == nil ||
			!strings.Contains(err.Error(), "SIPREC_ALLOW_CIDRS entry") {
			t.Errorf("parseCIDRs(%q) error = %v", bad, err)
		}
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if strings.Join(cfg.SIPNetworks, ",") != "udp,tcp" || cfg.SIPListen != "127.0.0.1:5070" ||
		cfg.SIPLocalIP != "127.0.0.1" || cfg.SIPTLSListen != "127.0.0.1:5071" ||
		cfg.RTPPortMin != 41000 || cfg.RTPPortMax != 42000 ||
		cfg.RecordingDir != "./data/recordings" || cfg.DBPath != "./data/siprec-receiver.db" ||
		cfg.HTTPListen != "127.0.0.1:8080" || cfg.PublicBaseURL != "http://127.0.0.1:8080" ||
		cfg.RetentionDays != 0 || cfg.MaxSessions != 256 || cfg.SessionIdle != 300*time.Second ||
		cfg.AllowCIDRs != nil {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	clearEnv(t)

	env := map[string]string{
		"SIPREC_NETWORKS":         " UDP, ,tcp ",
		"SIPREC_ALLOW_CIDRS":      "10.1.0.0/16",
		"SIPREC_LISTEN":           "0.0.0.0:5080",
		"SIPREC_LOCAL_IP":         "10.1.2.3",
		"SIP_TLS_LISTEN":          "0.0.0.0:5081",
		"SIP_TLS_CERT":            "/c.pem",
		"SIP_TLS_KEY":             "/k.pem",
		"RTP_PORT_MIN":            "30000",
		"RTP_PORT_MAX":            "30100",
		"RECORDING_DIR":           "/rec",
		"DB_PATH":                 "/db/x.db",
		"HTTP_LISTEN":             "localhost:9090",
		"PUBLIC_BASE_URL":         "https://srs.example.com//",
		"GOOGLE_CLIENT_ID":        "gid",
		"GOOGLE_CLIENT_SECRET":    "gsecret",
		"GOOGLE_HOSTED_DOMAIN":    "example.com",
		"SESSION_SECRET":          strings.Repeat("s", 32),
		"ADMIN_EMAIL":             "admin@example.com",
		"NOTIFY_SECRET":           "n",
		"DEV_AUTH_EMAIL":          "dev@example.com",
		"RETENTION_DAYS":          "14",
		"VOICEML_BASE_URL":        "https://vm.example.com",
		"SIPREC_MAX_SESSIONS":     "8",
		"SIPREC_SESSION_IDLE_SEC": "60",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if strings.Join(cfg.SIPNetworks, ",") != "udp,tcp" || cfg.SIPListen != "0.0.0.0:5080" ||
		cfg.SIPLocalIP != "10.1.2.3" || cfg.SIPTLSListen != "0.0.0.0:5081" ||
		cfg.SIPTLSCert != "/c.pem" || cfg.SIPTLSKey != "/k.pem" ||
		len(cfg.AllowCIDRs) != 1 || cfg.AllowCIDRs[0].String() != "10.1.0.0/16" ||
		cfg.RTPPortMin != 30000 || cfg.RTPPortMax != 30100 ||
		cfg.RecordingDir != "/rec" || cfg.DBPath != "/db/x.db" ||
		cfg.HTTPListen != "localhost:9090" || cfg.PublicBaseURL != "https://srs.example.com" ||
		cfg.GoogleID != "gid" || cfg.GoogleSecret != "gsecret" || cfg.HostedDomain != "example.com" ||
		cfg.AdminEmail != "admin@example.com" || cfg.NotifySecret != "n" ||
		cfg.DevAuthEmail != "dev@example.com" || cfg.RetentionDays != 14 ||
		cfg.VoiceMLBaseURL != "https://vm.example.com" || cfg.MaxSessions != 8 ||
		cfg.SessionIdle != time.Minute {
		t.Errorf("config = %+v", cfg)
	}

	t.Setenv("SIPREC_ALLOW_CIDRS", "nope")

	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "SIPREC_ALLOW_CIDRS") {
		t.Errorf("bad CIDR error = %v", err)
	}

	t.Setenv("SIPREC_ALLOW_CIDRS", "")
	t.Setenv("SIPREC_NETWORKS", "sctp")

	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), `"sctp" is not udp or tcp`) {
		t.Errorf("bad network error = %v", err)
	}
}

func TestValidateConfig(t *testing.T) {
	valid := func() config {
		return config{
			SIPNetworks: []string{"udp", "tcp"},
			RTPPortMin:  41000, RTPPortMax: 42000,
			HTTPListen: "127.0.0.1:8080",
		}
	}

	cases := []struct {
		name    string
		mutate  func(*config)
		wantErr string
	}{
		{"valid", func(*config) {}, ""},
		{"bad network", func(c *config) { c.SIPNetworks = []string{"udp", "ws"} }, `"ws" is not udp or tcp`},
		{"inverted range", func(c *config) { c.RTPPortMin, c.RTPPortMax = 5000, 4000 }, "greater than RTP_PORT_MAX"},
		{"odd min empties range", func(c *config) { c.RTPPortMin, c.RTPPortMax = 5001, 5001 }, "empty after odd-minimum"},
		{"cert without key", func(c *config) { c.SIPTLSCert = "/c" }, "must be set together"},
		{"key without cert", func(c *config) { c.SIPTLSKey = "/k" }, "must be set together"},
		{"cert and key", func(c *config) { c.SIPTLSCert, c.SIPTLSKey = "/c", "/k" }, ""},
		{"short session secret", func(c *config) { c.SessionSecret = "short" }, "SESSION_SECRET is 5 bytes"},
		{"long session secret", func(c *config) { c.SessionSecret = strings.Repeat("x", 32) }, ""},
		{"dev auth on wildcard", func(c *config) {
			c.DevAuthEmail, c.HTTPListen = "d@example.com", "0.0.0.0:8080"
		}, "DEV_AUTH_EMAIL is set but HTTP_LISTEN (0.0.0.0:8080) is not loopback"},
		{"dev auth on loopback", func(c *config) { c.DevAuthEmail = "d@example.com" }, ""},
	}

	for _, tc := range cases {
		cfg := valid()
		tc.mutate(&cfg)

		err := validateConfig(&cfg)
		if tc.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}

		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: error = %v, want %q", tc.name, err, tc.wantErr)
		}
	}

	logs := captureLog(t)
	cfg := valid()
	cfg.RTPPortMin = 41001

	if err := validateConfig(&cfg); err != nil || cfg.RTPPortMin != 41002 {
		t.Errorf("odd min = %d, %v; want 41002", cfg.RTPPortMin, err)
	}

	if !strings.Contains(logs.String(), "RTP_PORT_MIN 41001 is odd; using 41002") {
		t.Errorf("odd min adjustment not logged: %q", logs.String())
	}
}

func TestIsLoopbackListen(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080":   true,
		"127.9.9.9:80":     true,
		"[::1]:8080":       true,
		"localhost:8080":   true,
		":8080":            false,
		"0.0.0.0:8080":     false,
		"10.0.0.1:8080":    false,
		"example.com:8080": false,
		"no-port":          false,
	} {
		if got := isLoopbackListen(addr); got != want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestEphemeralSecret(t *testing.T) {
	a, b := ephemeralSecret(), ephemeralSecret()

	if len(a) != 64 || a == b {
		t.Fatalf("secrets %q / %q: want distinct 64-char hex", a, b)
	}

	if _, err := hex.DecodeString(a); err != nil {
		t.Errorf("not hex: %v", err)
	}
}

func openTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "m.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return st, dbPath
}

func seedOldCall(t *testing.T, st *store.Store, dir, id string) string {
	t.Helper()

	wav := filepath.Join(dir, id+".wav")
	if err := os.WriteFile(wav, []byte("x"), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}

	err := st.RecordLeg(context.Background(), siprecsrv.FinalizedLeg{
		CallID: id, Label: "a", WAVPath: wav, DurationSec: 1,
		StartedAt: time.Now().AddDate(0, 0, -100),
	})
	if err != nil {
		t.Fatalf("RecordLeg: %v", err)
	}

	return wav
}

// runMaintenance runs maintenanceLoop until its first pass has
// logged want, then stops it and waits for it to return.
func runMaintenance(t *testing.T, st *store.Store, envDays int, logs *lockedBuf, want string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		maintenanceLoop(ctx, st, envDays)
	}()

	waitForLog(t, logs, want)
	cancel()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("maintenanceLoop did not stop on cancel")
	}
}

func TestMaintenanceLoopPurgesAndVacuums(t *testing.T) {
	logs := captureLog(t)
	st, _ := openTestStore(t)
	dir := t.TempDir()
	wav := seedOldCall(t, st, dir, "old-env")

	// Grow the file past the vacuum floor, then free most of it.
	ctx := context.Background()
	if err := st.SetSetting(ctx, "cov_blob", strings.Repeat("x", 8<<20)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	if err := st.SetSetting(ctx, "cov_blob", ""); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	runMaintenance(t, st, 30, logs, "[maintenance] vacuumed database")

	if !strings.Contains(logs.String(), "purged 1 expired call(s), 1 file(s) (retention 30 day(s), RETENTION_DAYS)") {
		t.Errorf("env-retention purge not logged: %q", logs.String())
	}

	if _, err := os.Stat(wav); !os.IsNotExist(err) {
		t.Errorf("expired WAV still present: %v", err)
	}

	if stats, err := st.DBStats(ctx); err != nil || stats.Freelist != 0 {
		t.Errorf("after vacuum: %+v, %v", stats, err)
	}
}

func TestMaintenanceLoopAdminSetting(t *testing.T) {
	logs := captureLog(t)
	st, _ := openTestStore(t)
	seedOldCall(t, st, t.TempDir(), "old-ui")

	if err := st.SetSetting(context.Background(), store.SettingRetentionDays, "30"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	// The env default (0, disabled) loses to the saved setting.
	runMaintenance(t, st, 0, logs, "(retention 30 day(s), admin setting)")

	calls, _, err := st.ListCalls(context.Background(), store.ListFilter{})
	if err != nil || len(calls) != 0 {
		t.Errorf("calls after purge = %d, %v", len(calls), err)
	}
}

func TestMaintenanceLoopPurgeError(t *testing.T) {
	logs := captureLog(t)
	st, dbPath := openTestStore(t)

	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	if _, err := db.Exec(`ALTER TABLE calls RENAME TO cov_calls`); err != nil {
		t.Fatalf("rename calls: %v", err)
	}

	_ = db.Close()

	runMaintenance(t, st, 30, logs, "[maintenance] purge: ")

	if strings.Contains(logs.String(), "purged") {
		t.Errorf("failed purge reported success: %q", logs.String())
	}
}

// With a dead context every step fails, and each failure is logged
// rather than stopping the loop.
func TestMaintenanceLoopLogsFailures(t *testing.T) {
	logs := captureLog(t)
	st, _ := openTestStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	maintenanceLoop(ctx, st, 30)

	for _, want := range []string{
		"[maintenance] resolve retention: ", "[maintenance] wal checkpoint: ",
		"[maintenance] sqlite optimize: ", "[maintenance] vacuum check: ",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing log %q in %q", want, logs.String())
		}
	}
}

func writeSelfSignedPair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}

	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")

	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	return certPath, keyPath
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	addr := l.Addr().String()
	_ = l.Close()

	return addr
}

func runMain(t *testing.T) {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)
		main()
	}()

	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("main did not return")
		}
	})
}

// A listener that cannot bind ends the process cleanly: main logs the
// server error and runs the ordered teardown.
func TestMainStopsOnServeError(t *testing.T) {
	clearEnv(t)
	logs := captureLog(t)

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = busy.Close() }()

	tmp := t.TempDir()
	recDir := filepath.Join(tmp, "rec")
	dbPath := filepath.Join(tmp, "db", "srs.db")

	if err := os.MkdirAll(recDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	w, err := recording.Open(filepath.Join(recDir, "orphan-call-12165285911.wav"), 8000, 1)
	if err != nil {
		t.Fatalf("recording.Open: %v", err)
	}

	if _, err := w.Append(make([]int16, 8000)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cert, key := writeSelfSignedPair(t, tmp)

	for k, v := range map[string]string{
		"SIPREC_NETWORKS": "udp,tcp",
		"SIPREC_LISTEN":   busy.Addr().String(),
		"SIP_TLS_LISTEN":  "127.0.0.1:0",
		"SIP_TLS_CERT":    cert,
		"SIP_TLS_KEY":     key,
		"RTP_PORT_MIN":    "47000",
		"RTP_PORT_MAX":    "47010",
		"RECORDING_DIR":   recDir,
		"DB_PATH":         dbPath,
		"HTTP_LISTEN":     busy.Addr().String(),
		"RETENTION_DAYS":  "30",
	} {
		t.Setenv(k, v)
	}

	runMain(t)
	waitForLog(t, logs, "[main] stopped")

	out := logs.String()
	for _, want := range []string{
		"ADMIN_EMAIL is not set", "SESSION_SECRET is not set", "SIPREC_ALLOW_CIDRS is not set",
		"orphan recovery: adopted 1 recording(s) from " + recDir,
		"[main] server error: ", "address already in use",
		"listening on tls/127.0.0.1:0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing log %q", want)
		}
	}

	info, err := os.Stat(dbPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("db file mode = %v, %v; want 0600", info, err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st.Close() }()

	calls, _, err := st.ListCalls(context.Background(), store.ListFilter{})
	if err != nil || len(calls) != 1 || calls[0].SipCallID != "orphan-call" {
		t.Errorf("adopted calls = %+v, %v", calls, err)
	}
}

// SIGINT triggers the graceful shutdown path. The test also listens
// for SIGINT so the signal can never fall through to the default
// (process-killing) action.
func TestMainStopsOnSignal(t *testing.T) {
	clearEnv(t)
	logs := captureLog(t)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	t.Cleanup(func() { signal.Stop(sigs) })

	tmp := t.TempDir()
	recDir := filepath.Join(tmp, "rec")

	// An unreadable recording dir makes orphan recovery fail, which
	// is logged and not fatal.
	if err := os.MkdirAll(recDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.Chmod(recDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	t.Cleanup(func() { _ = os.Chmod(recDir, 0o700) })

	httpAddr := freeTCPAddr(t)

	for k, v := range map[string]string{
		"SIPREC_NETWORKS":    "udp",
		"SIPREC_LISTEN":      "127.0.0.1:0",
		"SIPREC_ALLOW_CIDRS": "127.0.0.1",
		"RTP_PORT_MIN":       "47100",
		"RTP_PORT_MAX":       "47110",
		"RECORDING_DIR":      recDir,
		"DB_PATH":            filepath.Join(tmp, "srs.db"),
		"HTTP_LISTEN":        httpAddr,
		"ADMIN_EMAIL":        "admin@example.com",
		"SESSION_SECRET":     strings.Repeat("s", 32),
	} {
		t.Setenv(k, v)
	}

	runMain(t)

	deadline := time.Now().Add(20 * time.Second)

	for {
		resp, err := http.Get("http://" + httpAddr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				break
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("web server never came up: %v\n%s", err, logs.String())
		}

		time.Sleep(20 * time.Millisecond)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("kill: %v", err)
	}

	waitForLog(t, logs, "[main] stopped")

	out := logs.String()
	for _, want := range []string{"[main] orphan recovery: ", "[main] shutting down"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing log %q", want)
		}
	}

	for _, unwanted := range []string{"ADMIN_EMAIL is not set", "SESSION_SECRET is not set",
		"SIPREC_ALLOW_CIDRS is not set", "server error"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unexpected log %q", unwanted)
		}
	}

	if _, err := http.Get("http://" + httpAddr + "/healthz"); err == nil {
		t.Error("web server still answering after shutdown")
	}
}

func TestRandomOffset(t *testing.T) {
	seen := map[int]bool{}

	for range 64 {
		n := randomOffset()
		if n < 0 || n > 65535 {
			t.Fatalf("randomOffset() = %d, want 0..65535", n)
		}

		seen[n] = true
	}

	if len(seen) < 2 {
		t.Error("randomOffset returned the same value 64 times")
	}
}

// Every fetch carries an account's API key as Basic auth: VOICEML_BASE_URL
// must be https, or plain http only to a loopback host.
func TestValidVoiceMLBaseURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"":                        true,
		"https://voiceml.example": true,
		"http://127.0.0.1:8080":   true,
		"http://localhost:8080":   true,
		"http://[::1]:8080":       true,
		"http://voiceml.example":  false,
		"ftp://voiceml.example":   false,
		"voiceml.example":         false,
		"https://":                false,
		"http://%zz":              false,
	} {
		err := validVoiceMLBaseURL(raw)
		if (err == nil) != ok {
			t.Errorf("validVoiceMLBaseURL(%q) = %v, want ok=%v", raw, err, ok)
		}
	}

	cfg := &config{RTPPortMin: 41000, RTPPortMax: 42000, VoiceMLBaseURL: "http://voiceml.example"}
	if err := validateConfig(cfg); err == nil {
		t.Error("validateConfig accepted a plain-http VOICEML_BASE_URL")
	}
}
