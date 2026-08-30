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
	"github.com/iitg-playhack/sportsbook/internal/live"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/waitlist"
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
	// Availability reads go to the replica, which falls back to the primary when
	// DB_REPLICA_URL is unset.
	availability := facility.NewAvailability(db.Replica, rdb, cfg.TZDisplay, log)

	// A 409 carries somewhere else to go (§5.3). Replica-only and on a 40 ms
	// budget, so enriching a rejection can never make it miss M-3; if this
	// lookup is slow or the replica is unreachable, the conflict ships bare.
	// The queue, and the cancel path's promotion hook. Both halves share one
	// service, so the sweeper and a live cancel claim through the same
	// SKIP LOCKED statement and cannot promote the same student (§6.2).
	waiting := waitlist.NewService(db, facilities, cfg.PromotionTTL, log)

	bookings := booking.NewService(db, facilities, loc).
		WithAlternatives(booking.NewAlternatives(db.Replica, availability, cfg.TZDisplay)).
		WithPromotion(waiting)

	// Live updates (§9). The hub is this replica's half: it subscribes to Redis
	// and fans out to the SSE clients connected HERE. Every replica runs one,
	// which is what lets the dispatcher publish a transition once and have it
	// reach every connected student without any replica knowing about another.
	//
	// Started unconditionally, whether or not workers are embedded: a replica
	// that publishes nothing still has clients that need to receive.
	hub := live.NewHub(rdb, log)
	go func() {
		if err := hub.Run(ctx); err != nil {
			log.Error("live hub stopped", "err", err)
		}
	}()

	// The publishing half, attached to the dispatcher below. Redis is a BUS for
	// this and nothing more — non-negotiable #3 — so a nil client here silences
	// live updates and changes nothing else about the system.
	slots := live.NewPublisher(rdb, loc, log)

	// Async work runs in-process for the demo, as CLAUDE.md allows: a ticker and
	// one short transaction every thirty seconds, plus a dispatcher that is
	// idle until something commits. Giving them their own deployment unit buys
	// no capability at this scale and gives the demo a second process to lose.
	//
	// EMBED_WORKERS=false hands them to cmd/worker instead. Nothing about
	// correctness moves with them — the outbox rows and the expired holds are
	// written either way; only the delay before somebody acts on them changes.
	if cfg.EmbedWorkers {
		dispatcher, err := outbox.NewFromConfig(db, cfg, log)
		if err != nil {
			return err
		}
		dispatcher.WithSlotPublisher(slots)
		go func() {
			if err := dispatcher.Run(ctx); err != nil {
				log.Error("outbox dispatcher stopped", "err", err)
			}
		}()
		go waiting.RunSweeper(ctx, waitlist.SweepInterval)
	} else {
		log.Info("workers not embedded; run cmd/worker separately",
			"embed_workers", false)
	}

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpx.NewRouter(httpx.RouterDeps{
			Config:       cfg,
			DB:           db,
			Redis:        rdb,
			Bookings:     bookings,
			Facilities:   facilities,
			Availability: availability,
			Waitlist:     waiting,
			Live:         hub,
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
