// Command siprec-receiver is a standalone SIPREC Session Recording
// Server (SRS, RFC 7866/7865) compatible with FreeSWITCH mod_siprec
// (and any RFC-compliant SRC). It records each offered audio stream
// to a WAV file, persists the recording + RS-Metadata in SQLite,
// accepts AMD/disposition call notifications keyed the VoiceML
// way, and serves a Google-OAuth-protected web UI where
// administrators manage recordings and users and authorized users
// review recordings.
//
// Configuration is entirely environment variables (see README.md).
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/voicetel/siprec-receiver/internal/auth"
	"github.com/voicetel/siprec-receiver/internal/rtp"
	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
	"github.com/voicetel/siprec-receiver/internal/store"
	"github.com/voicetel/siprec-receiver/internal/web"
)

type config struct {
	SIPListen      string
	SIPNetworks    []string
	SIPLocalIP     string
	SIPTLSListen   string
	SIPTLSCert     string
	SIPTLSKey      string
	AllowCIDRs     []*net.IPNet
	RTPPortMin     int
	RTPPortMax     int
	RecordingDir   string
	DBPath         string
	HTTPListen     string
	PublicBaseURL  string
	GoogleID       string
	GoogleSecret   string
	HostedDomain   string
	SessionSecret  string
	AdminEmail     string
	NotifySecret   string
	DevAuthEmail   string
	RetentionDays  int
	VoiceMLBaseURL string
	MaxSessions    int
	SessionIdle    time.Duration
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}

	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}

		log.Printf("[main] invalid %s=%q, using %d", key, v, def)
	}

	return def
}

// parseCIDRs parses a comma list of CIDRs (bare IPs become host
// routes) for SIPREC_ALLOW_CIDRS.
func parseCIDRs(list string) ([]*net.IPNet, error) {
	var out []*net.IPNet

	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		if !strings.Contains(item, "/") {
			if ip := net.ParseIP(item); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}

				item = fmt.Sprintf("%s/%d", item, bits)
			}
		}

		_, cidr, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("SIPREC_ALLOW_CIDRS entry %q: %w", item, err)
		}

		out = append(out, cidr)
	}

	return out, nil
}

func loadConfig() (config, error) {
	networks := []string{"udp", "tcp"}
	if v := strings.TrimSpace(os.Getenv("SIPREC_NETWORKS")); v != "" {
		networks = networks[:0]

		for _, n := range strings.Split(v, ",") {
			if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
				networks = append(networks, n)
			}
		}
	}

	cidrs, err := parseCIDRs(os.Getenv("SIPREC_ALLOW_CIDRS"))
	if err != nil {
		return config{}, err
	}

	cfg := config{
		SIPListen:      envOr("SIPREC_LISTEN", "127.0.0.1:5070"),
		SIPNetworks:    networks,
		SIPLocalIP:     envOr("SIPREC_LOCAL_IP", "127.0.0.1"),
		SIPTLSListen:   envOr("SIP_TLS_LISTEN", "127.0.0.1:5071"),
		SIPTLSCert:     os.Getenv("SIP_TLS_CERT"),
		SIPTLSKey:      os.Getenv("SIP_TLS_KEY"),
		AllowCIDRs:     cidrs,
		RTPPortMin:     envInt("RTP_PORT_MIN", 41000),
		RTPPortMax:     envInt("RTP_PORT_MAX", 42000),
		RecordingDir:   envOr("RECORDING_DIR", "./data/recordings"),
		DBPath:         envOr("DB_PATH", "./data/siprec-receiver.db"),
		HTTPListen:     envOr("HTTP_LISTEN", "127.0.0.1:8080"),
		PublicBaseURL:  strings.TrimRight(envOr("PUBLIC_BASE_URL", "http://127.0.0.1:8080"), "/"),
		GoogleID:       os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleSecret:   os.Getenv("GOOGLE_CLIENT_SECRET"),
		HostedDomain:   os.Getenv("GOOGLE_HOSTED_DOMAIN"),
		SessionSecret:  os.Getenv("SESSION_SECRET"),
		AdminEmail:     os.Getenv("ADMIN_EMAIL"),
		NotifySecret:   os.Getenv("NOTIFY_SECRET"),
		DevAuthEmail:   os.Getenv("DEV_AUTH_EMAIL"),
		RetentionDays:  envInt("RETENTION_DAYS", 0),
		VoiceMLBaseURL: os.Getenv("VOICEML_BASE_URL"),
		MaxSessions:    envInt("SIPREC_MAX_SESSIONS", 256),
		SessionIdle:    time.Duration(envInt("SIPREC_SESSION_IDLE_SEC", 300)) * time.Second,
	}

	return cfg, validateConfig(&cfg)
}

// validateConfig fails fast on configuration that would otherwise
// only surface as confusing runtime behavior.
func validateConfig(cfg *config) error {
	for _, n := range cfg.SIPNetworks {
		if n != "udp" && n != "tcp" {
			return fmt.Errorf("SIPREC_NETWORKS entry %q is not udp or tcp", n)
		}
	}

	if cfg.RTPPortMin > cfg.RTPPortMax {
		return fmt.Errorf("RTP_PORT_MIN (%d) is greater than RTP_PORT_MAX (%d)",
			cfg.RTPPortMin, cfg.RTPPortMax)
	}

	if cfg.RTPPortMin%2 != 0 {
		// The pool steps by 2 from the minimum; an odd minimum
		// would hand out odd (RTCP-convention) ports.
		log.Printf("[main] RTP_PORT_MIN %d is odd; using %d", cfg.RTPPortMin, cfg.RTPPortMin+1)
		cfg.RTPPortMin++

		if cfg.RTPPortMin > cfg.RTPPortMax {
			return fmt.Errorf("RTP port range is empty after odd-minimum adjustment")
		}
	}

	if (cfg.SIPTLSCert == "") != (cfg.SIPTLSKey == "") {
		return fmt.Errorf("SIP_TLS_CERT and SIP_TLS_KEY must be set together (or neither)")
	}

	if cfg.SessionSecret != "" && len(cfg.SessionSecret) < 32 {
		return fmt.Errorf("SESSION_SECRET is %d bytes; at least 32 are required", len(cfg.SessionSecret))
	}

	if cfg.DevAuthEmail != "" && !isLoopbackListen(cfg.HTTPListen) {
		return fmt.Errorf("DEV_AUTH_EMAIL is set but HTTP_LISTEN (%s) is not loopback; "+
			"the dev sign-in bypass must never be reachable from other hosts", cfg.HTTPListen)
	}

	return nil
}

// isLoopbackListen reports whether an HTTP listen address binds
// only loopback (an empty or wildcard host does not).
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("[main] configuration: %v", err)
	}

	if cfg.AdminEmail == "" {
		log.Printf("[main] WARNING: ADMIN_EMAIL is not set — no bootstrap administrator; " +
			"the web UI will authorize nobody until one is set (or a user row exists)")
	}

	if cfg.SessionSecret == "" {
		cfg.SessionSecret = ephemeralSecret()
		log.Printf("[main] WARNING: SESSION_SECRET is not set — using an ephemeral secret; " +
			"all sessions will be invalidated on restart")
	}

	if len(cfg.AllowCIDRs) == 0 {
		log.Printf("[main] WARNING: SIPREC_ALLOW_CIDRS is not set — the SIP listener " +
			"accepts INVITEs from any source; firewall it to your FreeSWITCH hosts")
	}

	// Resolve data paths once, absolutely: WAV paths are stored in
	// the database and must mean the same thing on every start,
	// whatever the working directory.
	if cfg.RecordingDir, err = filepath.Abs(cfg.RecordingDir); err != nil {
		log.Fatalf("[main] recording dir: %v", err)
	}

	if cfg.DBPath, err = filepath.Abs(cfg.DBPath); err != nil {
		log.Fatalf("[main] db path: %v", err)
	}

	if err := os.MkdirAll(cfg.RecordingDir, 0o700); err != nil {
		log.Fatalf("[main] mkdir recording dir: %v", err)
	}

	if dir := filepath.Dir(cfg.DBPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Fatalf("[main] mkdir db dir: %v", err)
		}
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("[main] %v", err)
	}

	// The database holds caller/callee numbers, metadata, and API
	// keys: owner-only, whatever the umask said.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(cfg.DBPath+suffix, 0o600); err != nil && !os.IsNotExist(err) {
			log.Printf("[main] chmod db: %v", err)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Adopt WAVs a previous crash/kill left unfinalized (their SIPREC
	// sessions never sent BYE), then start the retention sweep. Both
	// run before the listeners come up so the UI never shows a state
	// the sweep is about to change underneath it.
	if n, err := st.RecoverOrphans(ctx, cfg.RecordingDir); err != nil {
		log.Printf("[main] orphan recovery: %v", err)
	} else if n > 0 {
		log.Printf("[main] orphan recovery: adopted %d recording(s) from %s", n, cfg.RecordingDir)
	}

	if cfg.RetentionDays > 0 {
		go retentionLoop(ctx, st, cfg.RetentionDays)
	}

	pool := rtp.NewPortPool(cfg.RTPPortMin, cfg.RTPPortMax)

	// One siprecsrv family: the base server owns the shared session
	// map; every transport is a Clone sharing it, so a dialog that
	// starts on UDP can end on TCP.
	base := siprecsrv.New(cfg.SIPLocalIP, cfg.RecordingDir, pool)
	base.SetFinalizer(store.Finalizer{S: st})
	base.SetLimits(cfg.AllowCIDRs, cfg.MaxSessions, cfg.SessionIdle)

	serveErr := make(chan error, 4)

	var wg sync.WaitGroup

	serve := func(run func() error) {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := run(); err != nil && !errors.Is(err, context.Canceled) {
				serveErr <- err
			}
		}()
	}

	for _, network := range cfg.SIPNetworks {
		inst := base.Clone()

		serve(func() error { return inst.Serve(ctx, network, cfg.SIPListen) })
	}

	if cfg.SIPTLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.SIPTLSCert, cfg.SIPTLSKey)
		if err != nil {
			log.Fatalf("[main] load SIP TLS keypair: %v", err)
		}

		inst := base.Clone()
		tlsConf := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}

		serve(func() error { return inst.ServeTLS(ctx, cfg.SIPTLSListen, tlsConf) })
	}

	authn := auth.New(auth.Config{
		ClientID:      cfg.GoogleID,
		ClientSecret:  cfg.GoogleSecret,
		RedirectURL:   cfg.PublicBaseURL + "/auth/callback",
		SessionSecret: cfg.SessionSecret,
		AdminEmail:    cfg.AdminEmail,
		HostedDomain:  cfg.HostedDomain,
		DevEmail:      cfg.DevAuthEmail,
	}, st)

	webSrv, err := web.New(st, authn, cfg.RecordingDir, cfg.NotifySecret, cfg.VoiceMLBaseURL)
	if err != nil {
		log.Fatalf("[main] %v", err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.HTTPListen,
		Handler:           webSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("[main] web UI + notify endpoint on http://%s (base %s, auth enabled=%v)",
			cfg.HTTPListen, cfg.PublicBaseURL, authn.Enabled())

		if err := httpSrv.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	log.Printf("[main] SIPREC SRS on %s (%s), RTP ports %d-%d, recordings in %s, db %s",
		cfg.SIPListen, strings.Join(cfg.SIPNetworks, ","), cfg.RTPPortMin,
		cfg.RTPPortMax, cfg.RecordingDir, cfg.DBPath)

	select {
	case err := <-serveErr:
		log.Printf("[main] server error: %v", err)
	case <-ctx.Done():
		log.Printf("[main] shutting down")
	}

	// Ordered teardown: stop taking HTTP work, drain active SIPREC
	// sessions into the store (finalizes WAVs + rows), wait for the
	// SIP listeners to exit, and only then close the store.
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = httpSrv.Shutdown(shutdownCtx)
	shutdownCancel()

	base.Shutdown()
	wg.Wait()

	if err := st.Close(); err != nil {
		log.Printf("[main] store close: %v", err)
	}

	log.Printf("[main] stopped")
}

// retentionLoop deletes calls (rows + WAV files) that started more
// than days ago: once at startup, then daily. Disabled when days <= 0
// (main only starts it then).
func retentionLoop(ctx context.Context, st *store.Store, days int) {
	sweep := func() {
		cutoff := time.Now().AddDate(0, 0, -days)

		paths, n, err := st.DeleteCallsStartedBefore(ctx, cutoff)
		if err != nil {
			log.Printf("[retention] sweep: %v", err)

			return
		}

		for _, p := range paths {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				log.Printf("[retention] remove %s: %v", p, err)
			}
		}

		if n > 0 {
			log.Printf("[retention] deleted %d call(s) started before %s (%d file(s))",
				n, cutoff.Format("2006-01-02"), len(paths))
		}
	}

	sweep()

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func ephemeralSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}

	return hex.EncodeToString(b)
}
