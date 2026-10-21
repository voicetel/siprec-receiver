// Command siprec-receiver is a standalone SIPREC Session Recording
// Server (SRS, RFC 7866/7865) compatible with FreeSWITCH mod_siprec
// (and any RFC-compliant SRC). It records each offered audio stream
// to a WAV file, persists the recording + RS-Metadata in SQLite,
// accepts AMD call notifications keyed the callBroadcast way, and
// serves a Google-OAuth-protected web UI where administrators manage
// recordings and users and authorized users review recordings.
//
// Configuration is entirely environment variables (see README.md).
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/voicetel/siprec-receiver/internal/auth"
	"github.com/voicetel/siprec-receiver/internal/rtp"
	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
	"github.com/voicetel/siprec-receiver/internal/store"
	"github.com/voicetel/siprec-receiver/internal/web"
)

type config struct {
	SIPListen     string
	SIPNetworks   []string
	SIPLocalIP    string
	SIPTLSListen  string
	SIPTLSCert    string
	SIPTLSKey     string
	RTPPortMin    int
	RTPPortMax    int
	RecordingDir  string
	DBPath        string
	HTTPListen    string
	PublicBaseURL string
	GoogleID      string
	GoogleSecret  string
	SessionSecret string
	AdminEmail    string
	NotifySecret  string
	DevAuthEmail  string
	RetentionDays int
	CBBaseURL     string
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

func loadConfig() config {
	networks := []string{"udp", "tcp"}
	if v := strings.TrimSpace(os.Getenv("SIPREC_NETWORKS")); v != "" {
		networks = networks[:0]

		for _, n := range strings.Split(v, ",") {
			if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
				networks = append(networks, n)
			}
		}
	}

	return config{
		SIPListen:     envOr("SIPREC_LISTEN", "127.0.0.1:5070"),
		SIPNetworks:   networks,
		SIPLocalIP:    envOr("SIPREC_LOCAL_IP", "127.0.0.1"),
		SIPTLSListen:  envOr("SIP_TLS_LISTEN", "127.0.0.1:5071"),
		SIPTLSCert:    os.Getenv("SIP_TLS_CERT"),
		SIPTLSKey:     os.Getenv("SIP_TLS_KEY"),
		RTPPortMin:    envInt("RTP_PORT_MIN", 41000),
		RTPPortMax:    envInt("RTP_PORT_MAX", 42000),
		RecordingDir:  envOr("RECORDING_DIR", "./data/recordings"),
		DBPath:        envOr("DB_PATH", "./data/siprec-receiver.db"),
		HTTPListen:    envOr("HTTP_LISTEN", "127.0.0.1:8080"),
		PublicBaseURL: strings.TrimRight(envOr("PUBLIC_BASE_URL", "http://127.0.0.1:8080"), "/"),
		GoogleID:      os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleSecret:  os.Getenv("GOOGLE_CLIENT_SECRET"),
		SessionSecret: os.Getenv("SESSION_SECRET"),
		AdminEmail:    os.Getenv("ADMIN_EMAIL"),
		NotifySecret:  os.Getenv("NOTIFY_SECRET"),
		DevAuthEmail:  os.Getenv("DEV_AUTH_EMAIL"),
		RetentionDays: envInt("RETENTION_DAYS", 0),
		CBBaseURL:     os.Getenv("CALLBROADCAST_BASE_URL"),
	}
}

func main() {
	cfg := loadConfig()

	if cfg.AdminEmail == "" {
		log.Printf("[main] WARNING: ADMIN_EMAIL is not set — no bootstrap administrator; " +
			"the web UI will authorize nobody until one is set (or a user row exists)")
	}

	if cfg.SessionSecret == "" {
		cfg.SessionSecret = ephemeralSecret()
		log.Printf("[main] WARNING: SESSION_SECRET is not set — using an ephemeral secret; " +
			"all sessions will be invalidated on restart")
	}

	if err := os.MkdirAll(cfg.RecordingDir, 0o755); err != nil {
		log.Fatalf("[main] mkdir recording dir: %v", err)
	}

	if dir := filepath.Dir(cfg.DBPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("[main] mkdir db dir: %v", err)
		}
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("[main] %v", err)
	}

	defer func() { _ = st.Close() }()

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
	finalizer := store.Finalizer{S: st}

	serveErr := make(chan error, 4)

	// One siprecsrv.Server per transport. A SIPREC dialog stays on
	// the transport it started on, so the per-instance session maps
	// never need to agree; the port pool, recording dir, and store
	// finalizer are shared.
	for _, network := range cfg.SIPNetworks {
		srv := siprecsrv.New(cfg.SIPLocalIP, cfg.RecordingDir, pool)
		srv.Finalizer = finalizer

		go func(network string, srv *siprecsrv.Server) {
			if err := srv.Serve(ctx, network, cfg.SIPListen); err != nil &&
				!errors.Is(err, context.Canceled) {
				serveErr <- err
			}
		}(network, srv)
	}

	if cfg.SIPTLSCert != "" && cfg.SIPTLSKey != "" {
		cert, err := tls.LoadX509KeyPair(cfg.SIPTLSCert, cfg.SIPTLSKey)
		if err != nil {
			log.Fatalf("[main] load SIP TLS keypair: %v", err)
		}

		srv := siprecsrv.New(cfg.SIPLocalIP, cfg.RecordingDir, pool)
		srv.Finalizer = finalizer

		go func() {
			tlsConf := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			if err := srv.ServeTLS(ctx, cfg.SIPTLSListen, tlsConf); err != nil &&
				!errors.Is(err, context.Canceled) {
				serveErr <- err
			}
		}()
	}

	authn := auth.New(auth.Config{
		ClientID:      cfg.GoogleID,
		ClientSecret:  cfg.GoogleSecret,
		RedirectURL:   cfg.PublicBaseURL + "/auth/callback",
		SessionSecret: cfg.SessionSecret,
		AdminEmail:    cfg.AdminEmail,
		DevEmail:      cfg.DevAuthEmail,
	}, st)

	webSrv, err := web.New(st, authn, cfg.RecordingDir, cfg.NotifySecret, cfg.CBBaseURL)
	if err != nil {
		log.Fatalf("[main] %v", err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.HTTPListen,
		Handler:           webSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
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

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	_ = httpSrv.Shutdown(shutdownCtx)
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
