package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/thiagoribeiro/nimlet/internal/metrics"
)

// servePprof runs the profiling endpoints on their own listener until ctx is cancelled.
func servePprof(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	serveInternal(ctx, "pprof", addr, mux)
}

// serveMetrics serves /metrics on its own listener, off the public API port.
func serveMetrics(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	serveInternal(ctx, "metrics", addr, mux)
}

// serveInternal runs an operator-only listener until ctx is cancelled.
func serveInternal(ctx context.Context, name, addr string, h http.Handler) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	slog.Info(name+" listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error(name+" server", "error", err)
	}
}
