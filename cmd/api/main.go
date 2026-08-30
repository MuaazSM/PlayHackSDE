// Command api is the single stateless API binary. N replicas, no local state.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/lmittmann/tint"
	"github.com/redis/go-redis/v9"
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

	db, err := store.New(dialCtx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	log.Info("database connected",
		"max_conns", cfg.DBMaxConns,
		"dedicated_replica", db.HasDedicatedReplica())

	// Redis is optional by design. Rate limiting fails open and nothing on the
	// booking path is authoritative here, so a Redis that will not connect
	// degrades the service rather than preventing it from starting.
	rdb := dialRedis(dialCtx, cfg, log)
	if rdb != nil {
		defer func() { _ = rdb.Close() }()
	}

	loc, err := time.LoadLocation(cfg.TZDisplay)
	if err != nil {
		return err
	}

	facilities := facility.NewRepo(db.Primary)
	bookings := booking.NewService(db, facilities, loc)
	// Availability reads go to the replica, which falls back to the primary when
	// DB_REPLICA_URL is unset.
	availability := facility.NewAvailability(db.Replica, rdb, cfg.TZDisplay, log)

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpx.NewRouter(httpx.RouterDeps{
			Config:       cfg,
			DB:           db,
			Redis:        rdb,
			Bookings:     bookings,
			Facilities:   facilities,
			Availability: availability,
			Logger:       log,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("api listening",
			"addr", cfg.HTTPAddr,
			"auth_mode", cfg.AuthMode,
			"write_queue_depth", cfg.WriteQueueDepth)
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

	return srv.Shutdown(shutdownCtx)
}

func dialRedis(ctx context.Context, cfg *config.Config, log *slog.Logger) *redis.Client {
	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Warn("redis url invalid, rate limiting disabled", "err", err)
		return nil
	}

	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Warn("redis unreachable, rate limiting will fail open", "err", err)
	} else {
		log.Info("redis connected")
	}
	return rdb
}
