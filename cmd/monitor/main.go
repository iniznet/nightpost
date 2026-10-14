package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
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
	ingressGlobalRate := flag.Float64("ingress-global-rate", 500, "M6.10 coarse ingest ceiling across ALL projects, events/s")
	ingressGlobalBurst := flag.Float64("ingress-global-burst", 1000, "M6.10 coarse ingest burst across all projects")
	ingressIPRate := flag.Float64("ingress-ip-rate", 100, "M6.10 coarse ingest ceiling per source address, events/s (0 disables — the right setting behind a reverse proxy, where every request shares the proxy address)")
	ingressIPBurst := flag.Float64("ingress-ip-burst", 200, "M6.10 coarse per-source-address burst")
	maxAutoProjects := flag.Int("max-auto-projects", 500, "max projects auto-provisioned from unsigned ingest before a NEW name is refused (0 = unbounded)")
	requireIngestKeys := flag.Bool("require-ingest-keys", false, "refuse unsigned ingest entirely — set this once ingest is reachable through a public ingress")
	sessionSecret := flag.String("session-secret", "", "HMAC key for dashboard session cookies (empty = random per boot; sessions die on restart)")
	sessionSecretFile := flag.String("session-secret-file", "", "file holding the session key (0600); loaded when -session-secret is empty, and a generated key is PERSISTED there so sessions survive restarts")
	resetAdmin := flag.String("reset-admin", "", "one-shot: reset (or create) this admin email's password, write it to a 0600 file, and exit — never printed to the log")
	resetAdminOut := flag.String("reset-admin-out", "", "where -reset-admin writes the minted password (default <db-dir>/admin-reset.txt, mode 0600)")
	alertsConfig := flag.String("alerts-config", "", "path to alerts JSON (channels + rules); empty = alerting disabled")
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.MaxOccPerMin.Store(int64(*occurrenceCap))
	// M6.3: the process flags are IMPORT-ONCE defaults. SeedSettings only
	// writes ABSENT keys, so a value an operator changed at /settings survives
	// every restart and redeploy.
	if err := st.SeedSettings(store.SettingDefaults{
		RetentionDays:        *retentionDays,
		PruneIntervalMinutes: pruneMinutes(*pruneInterval),
		OccurrenceCap:        *occurrenceCap,
		ProfilesPerProject:   5000,
		IngestRate:           *ingestRate,
		IngestBurst:          *ingestBurst,
	}); err != nil {
		log.Fatalf("seed settings: %v", err)
	}
	st.MaxOccPerMin.Store(int64(st.SettingInt(store.SettingOccurrenceCap, *occurrenceCap)))
	logDBGuard(*dbPath)
	if n, uerr := st.UnsignedProjectCount(); uerr == nil && n > 0 {
		log.Printf("WARNING: %d project(s) accept unsigned ingest (no key) — rotate keys at /keys; the unsigned path is deprecated (#157)", n)
	}

	mon := &ingest.Monolog{Store: st}
	sh := ingest.NewShaper(*ingestRate, *ingestBurst)
	gate := ingest.NewKeyGate(st, 5*time.Second)
	gate.ConfigureAutoProvision(*maxAutoProjects, *requireIngestKeys)
	guard := ingest.NewIngressGuard(*ingressGlobalRate, *ingressGlobalBurst, *ingressIPRate, *ingressIPBurst)
	cors := ingest.NewCORSResolver(*corsOrigin, st.ProjectCORSOrigin, 30*time.Second)
	if *resetAdmin != "" {
		email := strings.ToLower(strings.TrimSpace(*resetAdmin))
		pw, rerr := st.ResetAdminPassword(email)
		if rerr != nil {
			log.Fatalf("reset-admin: %v", rerr)
		}
		out := *resetAdminOut
		if out == "" {
			out = filepath.Join(filepath.Dir(*dbPath), "admin-reset.txt")
		}
		if werr := os.WriteFile(out, []byte(pw+"\n"), 0o600); werr != nil {
			log.Fatalf("reset-admin: write %s: %v", out, werr)
		}
		_ = os.Chmod(out, 0o600)
		// The password is deliberately NOT logged: the container's stdout lands
		// in the json-file log and would keep the credential for the log's
		// whole rotation window (M6.5).
		log.Printf("admin %q password reset — written to %s (mode 0600). Read it there and delete the file; it is not printed anywhere. Restart without -reset-admin when done.", email, out)
		return
	}
	secret, serr := resolveSessionSecret(*sessionSecret, *sessionSecretFile)
	if serr != nil {
		log.Fatalf("session secret: %v", serr)
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
	// M6.10 route order: coarse guard → key gate → per-project shaper →
	// handler. The guard is OUTERMOST because a {project}-name flood must be
	// throttled before any key lookup or DB work, and its ceiling is keyed on
	// nothing the caller picks. The shaper is INNERMOST because its bucket is
	// keyed on that chosen name — it can only ever be a per-tenant fairness
	// limit, never the DoS ceiling.
	chain := func(project func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
		return ingest.WithIngressGuard(guard, ingest.WithKeyGate(gate, project, ingest.WithShaper(sh, next)))
	}
	mux.HandleFunc("POST /ingest/{project}/sentry", chain(ingest.PathProject, ingest.SentryHandler(st)))
	mux.HandleFunc("POST /ingest/{project}/profile", ingest.WithCORSSource(cors.OriginFor, chain(ingest.PathProject, ingest.ProfileHandler(st))))
	mux.HandleFunc("POST /ingest/{project}/error", ingest.WithCORSSource(cors.OriginFor, chain(ingest.PathProject, ingest.ErrorHandler(st))))
	mux.HandleFunc("POST /ingest/{project}/xhprof", chain(ingest.XHProfProject, ingest.XHProfHandler(st)))
	mux.HandleFunc("POST /ingest/{project}/query", chain(ingest.PathProject, ingest.QueryHandler(st)))
	mux.HandleFunc("POST /ingest/{project}/job", chain(ingest.PathProject, ingest.JobHandler(st)))
	mux.HandleFunc("POST /ingest/{project}/release", chain(ingest.PathProject, ingest.ReleaseHandler(st)))
	// Sentry SDK compatibility: sentry-laravel POSTs envelopes to /api/{id}/store.
	mux.HandleFunc("POST /api/{project}/store", chain(ingest.PathProject, ingest.SentryHandler(st)))
	mux.HandleFunc("POST /api/{project}/store/", chain(ingest.PathProject, ingest.SentryHandler(st)))
	mux.HandleFunc("POST /api/{project}/envelope", chain(ingest.PathProject, ingest.SentryHandler(st)))
	// spiral-packages/profiler compatibility: PROFILER_ENDPOINT posts here
	// (project key = HTTP Basic username, e.g. http://backend@monitor:8000/...).
	mux.HandleFunc("POST /api/profiler/store", chain(ingest.XHProfProject, ingest.XHProfHandler(st)))
	mux.HandleFunc("POST /api/{project}/profiler/store", chain(ingest.XHProfProject, ingest.XHProfHandler(st)))
	// Browser preflight for the first-party reporter endpoints.
	mux.HandleFunc("OPTIONS /ingest/{project}/error", ingest.PreflightSource(cors.OriginFor))
	mux.HandleFunc("OPTIONS /ingest/{project}/profile", ingest.PreflightSource(cors.OriginFor))
	// Health contract (M6.5). /up is LIVENESS — the process answers, and it
	// deliberately never touches the database, so a sick store cannot get the
	// container killed and restarted into the same sickness. /ready is
	// READINESS: the store must answer a real query. Both are unauthenticated,
	// so both stay minimal — status, db and the build revision, never a tenant
	// count or anything else an unauthenticated caller should not learn.
	mux.HandleFunc("GET /up", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /ready", readyHandler(st))
	web.Register(mux, st, gate, auth, web.WithIngestHealth(web.IngestHealth{
		ShaperRejections:  sh.Rejections,
		KeyGateRejections: gate.Rejections,
		UnknownAdmits:     gate.UnknownAdmits,
		GateRefusals:      gate.Refusals,
		GuardGlobal:       guard.GlobalRejections,
		GuardIP:           guard.IPRejections,
	}), web.WithCORSInvalidator(cors), web.WithSettingDefaults(store.SettingDefaults{
		RetentionDays:        *retentionDays,
		PruneIntervalMinutes: pruneMinutes(*pruneInterval),
		OccurrenceCap:        *occurrenceCap,
		ProfilesPerProject:   5000,
		IngestRate:           *ingestRate,
		IngestBurst:          *ingestBurst,
	}))

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
// resolveSessionSecret returns the dashboard cookie key.
//
// Precedence: the -session-secret flag, then the file named by
// -session-secret-file, then a fresh random key. With a file configured a
// GENERATED key is persisted (0600) instead of being thrown away at exit: the
// old random-per-boot default silently logged every admin out on each deploy,
// which is precisely what a one-command install must not do (M6.5).
func resolveSessionSecret(flagValue, filePath string) ([]byte, error) {
	if flagValue != "" {
		return []byte(flagValue), nil
	}
	if filePath == "" {
		log.Printf("WARNING: -session-secret/-session-secret-file unset; generated a per-boot key — dashboard sessions will not survive restarts")
		return randomSessionSecret()
	}
	if b, err := os.ReadFile(filePath); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			log.Printf("session secret loaded from %s", filePath)
			return []byte(s), nil
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", filePath, err)
	}

	secret, err := randomSessionSecret()
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(filePath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	// The secret is already the hex form that is written, so the file and the
	// in-process key are byte-identical.
	if err := os.WriteFile(filePath, []byte(string(secret)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("persist session secret to %s: %w", filePath, err)
	}
	_ = os.Chmod(filePath, 0o600)
	log.Printf("session secret generated and persisted to %s (mode 0600) — dashboard sessions now survive restarts", filePath)
	return secret, nil
}

// randomSessionSecret returns the key in the SAME hex form the file holds.
// Returning raw bytes here would make the persisted file a different key from
// the one this boot used — i.e. sessions would still die on restart, which is
// the whole defect this is meant to fix.
func randomSessionSecret() ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("session secret entropy: %w", err)
	}
	return []byte(hex.EncodeToString(raw)), nil
}

// readyHandler is the readiness leg of the health contract: 200 when the
// store answers a real query, 503 carrying the error when it does not. It is a
// function rather than an inline closure so both branches are testable.
func readyHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{"status": "ok", "db": "ok", "revision": buildRevision()}
		if err := st.Ping(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			payload["status"] = "unavailable"
			// The error text is safe here: it names the store, never a tenant.
			payload["db"] = err.Error()
		}
		_ = json.NewEncoder(w).Encode(payload)
	}
}

// buildRevision reports the binary's VCS revision for the readiness payload,
// so "which build is actually deployed?" is answerable from the endpoint
// rather than by guessing at image tags (M6.5).
func buildRevision() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				if len(s.Value) > 12 {
					return s.Value[:12]
				}
				if s.Value == "" {
					return "dev"
				}
				return s.Value
			}
		}
	}
	return "dev"
}

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

// pruneMinutes renders a prune interval as whole minutes, never below 1.
func pruneMinutes(d time.Duration) int {
	m := int((d + time.Minute - 1) / time.Minute)
	if m < 1 {
		m = 1
	}
	return m
}

// retentionLoop keeps the store bounded: occurrences age out, per-issue
// occurrence history is capped, and every project keeps its own rolling
// profile window. One cycle runs at boot, then on the interval; every cycle is
// chunked deletes on the single-writer connection.
//
// M6.3: the loop RE-READS its knobs from the settings table each cycle, so an
// operator's change at /settings takes effect without a restart — the flags
// passed here are only the import-once fallback for an unseeded key.
func retentionLoop(st *store.Store, fallbackDays int, fallbackEvery time.Duration) {
	fallbackMinutes := pruneMinutes(fallbackEvery)
	runOnce := func() {
		days := st.SettingInt(store.SettingRetentionDays, fallbackDays)
		keep := st.SettingInt(store.SettingProfilesPerProject, 5000)
		st.MaxOccPerMin.Store(int64(st.SettingInt(store.SettingOccurrenceCap, int(st.MaxOccPerMin.Load()))))
		msg, err := st.RunRetention(days, keep)
		if err != nil {
			log.Printf("retention: %v", err)
			return
		}
		log.Print(msg)
	}
	runOnce()
	for {
		minutes := st.SettingInt(store.SettingPruneMinutes, fallbackMinutes)
		if minutes <= 0 {
			minutes = fallbackMinutes
		}
		time.Sleep(time.Duration(minutes) * time.Minute)
		runOnce()
	}
}
