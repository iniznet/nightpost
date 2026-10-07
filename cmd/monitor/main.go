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

	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/sentry", ingest.SentryHandler(st))
	mux.HandleFunc("POST /ingest/{project}/profile", ingest.ProfileHandler(st))
	mux.HandleFunc("POST /ingest/{project}/error", ingest.ErrorHandler(st))
	mux.HandleFunc("POST /ingest/{project}/xhprof", ingest.XHProfHandler(st))
	// Sentry SDK compatibility: sentry-laravel POSTs envelopes to /api/{id}/store.
	mux.HandleFunc("POST /api/{project}/store", ingest.SentryHandler(st))
	mux.HandleFunc("POST /api/{project}/store/", ingest.SentryHandler(st))
	mux.HandleFunc("POST /api/{project}/envelope", ingest.SentryHandler(st))
	// spiral-packages/profiler compatibility: PROFILER_ENDPOINT posts here
	// (project key = HTTP Basic username, e.g. http://backend@monitor:8000/...).
	mux.HandleFunc("POST /api/profiler/store", ingest.XHProfHandler(st))
	mux.HandleFunc("POST /api/{project}/profiler/store", ingest.XHProfHandler(st))
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