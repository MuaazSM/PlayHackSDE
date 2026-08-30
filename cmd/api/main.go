// Command api is the single stateless API binary. N replicas, no local state.
//
// Phase 1 scope: config, pool, health probes, graceful shutdown. No booking
// logic — the write path lands in M1 behind internal/booking.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lmittmann/tint"
)

func main() {
	log := slog.New(tint.NewHandler(os.Stderr, &tint.Options{Level: slog.LevelInfo, TimeFormat: time.Kitchen}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("api exited with error", "err", err)
		os.Exit(1)
	}
	log.Info("api stopped cleanly")
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// SIGTERM/SIGINT cancels this context, which unwinds the whole shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pool, err := store.NewPool(dialCtx, store.PoolOptions{
		URL:         cfg.DBURL,
		MaxConns:    cfg.DBMaxConns,
		MaxConnLife: time.Hour,
		MaxConnIdle: 5 * time.Minute,
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("database connected", "max_conns", cfg.DBMaxConns)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           newRouter(pool),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", cfg.HTTPAddr, "auth_mode", cfg.AuthMode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received", "grace", cfg.ShutdownTimeout)
	}

	// Stop accepting new connections, let in-flight requests finish. Draining
	// before closing the pool is what keeps a committed booking from losing its
	// response.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelShutdown()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return nil
}

func newRouter(pool *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Liveness: the process is up. Never touches a dependency — a slow database
	// must not get the container killed.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Readiness: this replica can serve traffic, which requires the database.
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unready",
				"reason": "database unreachable",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	return r
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
