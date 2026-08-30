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

	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/waitlist"
	"github.com/lmittmann/tint"
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

	facilities := facility.NewRepo(db.Primary)
	waiting := waitlist.NewService(db, facilities, cfg.PromotionTTL, log)

	var wg sync.WaitGroup
	wg.Add(2)

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

	<-ctx.Done()
	log.Info("shutdown signal received")

	// Both loops exit on ctx; wait for the in-flight pass to finish so a claimed
	// batch is not abandoned mid-send.
	wg.Wait()
	return nil
}
