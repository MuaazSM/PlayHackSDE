// Command worker runs the async work: the outbox dispatcher and the waitlist
// sweeper.
//
// Everything here is also embeddable in cmd/api — set EMBED_WORKERS=true (the
// default) and the demo is a single process with no second thing to start,
// nothing to keep in sync on stage, and no way to end up running one half
// without the other. This binary exists for the deployment where they scale
// apart: N stateless api replicas, one worker.
//
// Both workers are safe to run in several copies at once. The dispatcher claims
// with FOR UPDATE SKIP LOCKED and the sweeper promotes through the same locking
// statement a live cancel uses, so two workers do disjoint work rather than the
// same work twice. Running this alongside an api with EMBED_WORKERS=true is
// therefore wasteful but not wrong.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/checkin"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/facility"
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
		log.Error("worker exited with error", "err", err)
		os.Exit(1)
	}
	log.Info("worker stopped cleanly")
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	db, err := store.New(dialCtx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	dispatcher, err := outbox.NewFromConfig(db, cfg, log)
	if err != nil {
		return err
	}

	// The worker only PUBLISHES live updates (§9); the hubs that consume them
	// live in the api replicas. When workers are not embedded this process owns
	// the dispatcher, so without this the stream would go silent in exactly the
	// deployment that has more than one api replica to fan out to.
	loc, err := time.LoadLocation(cfg.TZDisplay)
	if err != nil {
		return err
	}
	rdb := dialRedis(dialCtx, cfg, log)
	if rdb != nil {
		defer func() { _ = rdb.Close() }()
	}
	dispatcher.WithSlotPublisher(live.NewPublisher(rdb, loc, log))

	facilities := facility.NewRepo(db.Primary)
	waiting := waitlist.NewService(db, facilities, cfg.PromotionTTL, log)

	// The no-show sweeper (§7), promoting through the SAME waitlist service the
	// expiry sweeper uses. Two services would each hold their own claim window
	// and the queue would behave differently depending on which sweeper freed the
	// court.
	attendance := checkin.NewService(db, facilities,
		checkin.NewMinter(cfg.CheckinHMACSecret), cfg.GracePeriod, log).
		WithPromotion(waiting)

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		if err := dispatcher.Run(ctx); err != nil {
			log.Error("outbox dispatcher stopped", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		waiting.RunSweeper(ctx, waitlist.SweepInterval)
	}()
	go func() {
		defer wg.Done()
		attendance.RunSweeper(ctx, checkin.SweepInterval)
	}()

	<-ctx.Done()
	log.Info("shutdown signal received")

	// Both loops exit on ctx; wait for the in-flight pass to finish so a claimed
	// batch is not abandoned mid-send.
	wg.Wait()
	return nil
}

// dialRedis connects the live-update bus.
//
// Optional, exactly as it is in cmd/api: an unreachable Redis is logged and the
// worker carries on. Nothing it drains depends on Redis, so the failure mode is
// a quiet stream and a polling client — never a stalled outbox.
func dialRedis(ctx context.Context, cfg *config.Config, log *slog.Logger) *redis.Client {
	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Warn("redis url invalid, live updates disabled", "err", err)
		return nil
	}

	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Warn("redis unreachable, live updates will not be published", "err", err)
	} else {
		log.Info("redis connected")
	}
	return rdb
}
