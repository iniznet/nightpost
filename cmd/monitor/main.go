package main

import (
	"context"
	"crypto/rand"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/iniznet/fenrirealm-monitor/internal/alerts"
	"github.com/iniznet/fenrirealm-monitor/internal/ingest"
	"github.com/iniznet/fenrirealm-monitor/internal/store"
	"github.com/iniznet/fenrirealm-monitor/internal/web"
)

func main() {
	dbPath := flag.String("db", "monitor.db", "sqlite database path")
	httpAddr := flag.String("http", ":8000", "HTTP listen address (UI + Sentry ingest)")
	sockAddr := flag.String("socket", ":9913", "Monolog TCP socket listen address")
	retentionDays := flag.Int("retention-days", 30, "delete occurrences older than this many days (issues are kept)")
	pruneInterval := flag.Duration("prune-interval", 10*time.Minute, "how often the retention cycle runs")
	corsOrigin := flag.String("cors-origin", "", "browser origin allowed to POST first-party error/profile reports (empty = no CORS)")
	occurrenceCap := flag.Int("occurrence-cap", 30, "max occurrence rows stored per issue fingerprint per minute (0 = unlimited)")
	ingestRate := flag.Float64("ingest-rate", 50, "ingest events per second allowed per project before 429")
	ingestBurst := flag.Float64("ingest-burst", 100, "ingest burst ceiling per project")
	sessionSecret := flag.String("session-secret", "", "HMAC key for dashboard session cookies (empty = random per boot; sessions die on restart)")
	resetAdmin := flag.String("reset-admin", "", "one-shot: reset (or create) this admin email's password, print a fresh one, and exit")
	alertsConfig := flag.String("alerts-config", "", "path to alerts JSON (channels + rules); empty = alerting disabled")
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.MaxOccPerMin = *occurrenceCap
	logDBGuard(*dbPath)
	if n, uerr := st.UnsignedProjectCount(); uerr == nil && n > 0 {
		log.Printf("WARNING: %d project(s) accept unsigned ingest (no key) — rotate keys at /keys; the unsigned path is deprecated (#157)", n)
	}

	mon := &ingest.Monolog{Store: st}
	sh := ingest.NewShaper(*ingestRate, *ingestBurst)
	gate := ingest.NewKeyGate(st, 5*time.Second)
	if *resetAdmin != "" {
		email := strings.ToLower(strings.TrimSpace(*resetAdmin))
		pw, rerr := st.ResetAdminPassword(email)
		if rerr != nil {
			log.Fatalf("reset-admin: %v", rerr)
		}
		log.Printf("admin %q password reset (shown once): %s — restart without -reset-admin when done", email, pw)
		return
	}
	secret := []byte(*sessionSecret)
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			log.Fatalf("session secret entropy: %v", err)
		}
		log.Printf("WARNING: -session-secret unset; generated a per-boot key — dashboard sessions will not survive restarts")
	}
	if *alertsConfig != "" {
		acfg, aerr := alerts.LoadConfig(*alertsConfig)
		if aerr != nil {
			log.Fatalf("alerts config: %v", aerr)
		}
		disp := alerts.NewDispatcher(st, acfg, *alertsConfig)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go disp.Run(ctx)
		log.Printf("alerts: dispatcher armed (%d channels, %d rules)", len(acfg.Channels), len(acfg.Rules))
	}
	auth := web.NewAuth(st, secret)
	go func() {
		if err := mon.ListenAndServe(*sockAddr); err != nil {
			log.Printf("monolog socket stopped: %v", err)
		}
	}()

	go retentionLoop(st, *retentionDays, *pruneInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/sentry", ingest.WithShaper(sh, ingest.WithKeyGate(gate, ingest.PathProject, ingest.SentryHandler(st))))
	mux.HandleFunc("POST /ingest/{project}/profile", ingest.WithCORS(*corsOrigin, ingest.WithShaper(sh, ingest.WithKeyGate(gate, ingest.PathProject, ingest.ProfileHandler(st)))))
	mux.HandleFunc("POST /ingest/{project}/error", ingest.WithCORS(*corsOrigin, ingest.WithShaper(sh, ingest.WithKeyGate(gate, ingest.PathProject, ingest.ErrorHandler(st)))))
	mux.HandleFunc("POST /ingest/{project}/xhprof", ingest.WithShaper(sh, ingest.WithKeyGate(gate, ingest.XHProfProject, ingest.XHProfHandler(st))))
	mux.HandleFunc("POST /ingest/{project}/query", ingest.WithShaper(sh, ingest.WithKeyGate(gate, ingest.PathProject, ingest.QueryHandler(st))))
	// Sentry SDK compatibility: sentry-laravel POSTs envelopes to /api/{id}/store.
	mux.HandleFunc("POST /api/{project}/store", ingest.WithShaper(sh, ingest.WithKeyGate(gate, ingest.PathProject, ingest.SentryHandler(st))))
	mux.HandleFunc("POST /api/{project}/store/", ingest.WithShaper(sh, ingest.WithKeyGate(gate, ingest.PathProject, ingest.SentryHandler(st))))
	mux.HandleFunc("POST /api/{project}/envelope", ingest.WithShaper(sh, ingest.WithKeyGate(gate, ingest.PathProject, ingest.SentryHandler(st))))
	// spiral-packages/profiler compatibility: PROFILER_ENDPOINT posts here
	// (project key = HTTP Basic username, e.g. http://backend@monitor:8000/...).
	mux.HandleFunc("POST /api/profiler/store", ingest.WithShaper(sh, ingest.WithKeyGate(gate, ingest.XHProfProject, ingest.XHProfHandler(st))))
	mux.HandleFunc("POST /api/{project}/profiler/store", ingest.WithShaper(sh, ingest.WithKeyGate(gate, ingest.XHProfProject, ingest.XHProfHandler(st))))
	// Browser preflight for the first-party reporter endpoints.
	mux.HandleFunc("OPTIONS /ingest/{project}/error", ingest.PreflightHandler(*corsOrigin))
	mux.HandleFunc("OPTIONS /ingest/{project}/profile", ingest.PreflightHandler(*corsOrigin))
	mux.HandleFunc("GET /up", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	web.Register(mux, st, gate, auth)

	srv := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		_ = mon.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("fenrirealm-monitor listening on %s (socket %s)", *httpAddr, *sockAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server: %v", err)
	}
}

type mountPoint struct{ dir, fstype string }

// logDBGuard prints the resolved DB path and the filesystem mount holding
// it, warning loudly when the database would live in the container layer
// and be wiped by the next deploy (#159).
func logDBGuard(dbPath string) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		log.Printf("db path: %s (unresolved: %v)", dbPath, err)
		return
	}
	log.Printf("db path: %s", abs)
	m := dbMount(abs)
	if m == nil {
		return
	}
	if m.dir == "/" && m.fstype == "overlay" {
		log.Printf("WARNING: db %s sits on the container layer (%s) — data will not survive container replacement", abs, m.fstype)
		return
	}
	log.Printf("db mount: %s (%s) — persistent", m.dir, m.fstype)
}

// dbMount finds the deepest /proc/mounts entry containing the path's
// directory; nil when it cannot be determined (non-Linux, unreadable file),
// which callers treat as unknown rather than unsafe.
func dbMount(path string) *mountPoint {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	dir := filepath.Dir(path)
	var best *mountPoint
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if strings.HasPrefix(dir, fields[1]) && (best == nil || len(fields[1]) > len(best.dir)) {
			best = &mountPoint{dir: fields[1], fstype: fields[2]}
		}
	}
	return best
}

// retentionLoop keeps the store bounded: occurrences age out, per-issue
// occurrence history is capped, profiles keep a rolling window. One cycle
// runs at boot, then on the interval; every cycle is chunked deletes on the
// single-writer connection.
func retentionLoop(st *store.Store, days int, every time.Duration) {
	if every <= 0 {
		every = 10 * time.Minute
	}
	runOnce := func() {
		msg, err := st.RunRetention(days, 5000)
		if err != nil {
			log.Printf("retention: %v", err)
			return
		}
		log.Print(msg)
	}
	runOnce()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for range ticker.C {
		runOnce()
	}
}
