package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	mon := &ingest.Monolog{Store: st}
	go func() {
		if err := mon.ListenAndServe(*sockAddr); err != nil {
			log.Printf("monolog socket stopped: %v", err)
		}
	}()

	go retentionLoop(st, *retentionDays, *pruneInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/sentry", ingest.SentryHandler(st))
	mux.HandleFunc("POST /ingest/{project}/profile", ingest.WithCORS(*corsOrigin, ingest.ProfileHandler(st)))
	mux.HandleFunc("POST /ingest/{project}/error", ingest.WithCORS(*corsOrigin, ingest.ErrorHandler(st)))
	mux.HandleFunc("POST /ingest/{project}/xhprof", ingest.XHProfHandler(st))
	// Sentry SDK compatibility: sentry-laravel POSTs envelopes to /api/{id}/store.
	mux.HandleFunc("POST /api/{project}/store", ingest.SentryHandler(st))
	mux.HandleFunc("POST /api/{project}/store/", ingest.SentryHandler(st))
	mux.HandleFunc("POST /api/{project}/envelope", ingest.SentryHandler(st))
	// spiral-packages/profiler compatibility: PROFILER_ENDPOINT posts here
	// (project key = HTTP Basic username, e.g. http://backend@monitor:8000/...).
	mux.HandleFunc("POST /api/profiler/store", ingest.XHProfHandler(st))
	mux.HandleFunc("POST /api/{project}/profiler/store", ingest.XHProfHandler(st))
	// Browser preflight for the first-party reporter endpoints.
	mux.HandleFunc("OPTIONS /ingest/{project}/error", ingest.PreflightHandler(*corsOrigin))
	mux.HandleFunc("OPTIONS /ingest/{project}/profile", ingest.PreflightHandler(*corsOrigin))
	mux.HandleFunc("GET /up", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	web.Register(mux, st)

	srv := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("fenrirealm-monitor listening on %s (socket %s)", *httpAddr, *sockAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server: %v", err)
	}
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
