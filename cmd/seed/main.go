// Command seed populates facilities, users and the global policy row.
// Idempotent — safe to re-run mid-demo.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/seed"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/lmittmann/tint"
)

func main() {
	log := slog.New(tint.NewHandler(os.Stderr, &tint.Options{Level: slog.LevelInfo, TimeFormat: time.Kitchen}))

	if err := run(log); err != nil {
		log.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := store.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	res, err := seed.Run(ctx, db.Primary)
	if err != nil {
		return err
	}

	log.Info("seed applied", "facilities", res.Facilities, "users", res.Users, "policies", res.Policies)
	return nil
}
