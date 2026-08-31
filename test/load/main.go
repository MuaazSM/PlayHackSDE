package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/internal/policy"
	"github.com/iitg-playhack/sportsbook/internal/seed"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newReader(b []byte) io.Reader { return bytes.NewReader(b) }

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	n        int
	facility string
	at       string
	minutes  int
	url      string
	asJSON   bool
}

func parseFlags(args []string, out io.Writer) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(out)

	fs.IntVar(&o.n, "n", 500, "concurrent virtual users, all aimed at one slot")
	fs.StringVar(&o.facility, "facility", "tennis-court-1", "facility slug or UUID")
	fs.StringVar(&o.at, "at", "18:00", "slot start: HH:MM in the campus timezone")
	fs.IntVar(&o.minutes, "minutes", 60, "slot length in minutes")
	fs.StringVar(&o.url, "url", "",
		"base URL of a running API. Empty starts one in-process on a loopback port, "+
			"so `make load` needs nothing installed beyond the Postgres this project already uses")
	fs.BoolVar(&o.asJSON, "json", false, "emit the report as JSON")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return o, nil
}

func run(args []string, out io.Writer) error {
	opts, err := parseFlags(args, out)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(cfg.TZDisplay)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDial()

	db, err := store.New(dialCtx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	facilityID, err := resolveFacility(ctx, db.Primary, opts.facility)
	if err != nil {
		return err
	}
	start := resolveStart(opts.at, loc)
	end := start.Add(time.Duration(opts.minutes) * time.Minute)

	exclusive, err := isExclusive(ctx, db.Primary, facilityID)
	if err != nil {
		return err
	}

	// Clear the slot so the profile is re-runnable back to back. Without this a
	// second run finds the court already taken, measures 500 conflicts and no
	// confirmations, and reports "nobody won the slot" — correct, and useless.
	if err := resetSlot(ctx, db.Primary, facilityID, start, end); err != nil {
		return err
	}

	tokens, err := mintTokens(ctx, cfg, db.Primary, opts.n)
	if err != nil {
		return err
	}

	base := opts.url
	if base == "" {
		stop, url, err := serveInProcess(ctx, cfg, db, loc)
		if err != nil {
			return err
		}
		defer stop()
		base = url
	}

	fmt.Fprintf(out, "load: %d virtual users -> %s, %s for %d minutes\n",
		opts.n, base, start.In(loc).Format("2006-01-02 15:04 MST"), opts.minutes)

	runner := &Runner{
		Client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				// Every virtual user needs its own connection or they queue in the
				// client and the run measures Go's connection pool.
				MaxIdleConns:        opts.n + 16,
				MaxIdleConnsPerHost: opts.n + 16,
				MaxConnsPerHost:     opts.n + 16,
			},
		},
		BaseURL:    base,
		FacilityID: facilityID,
		Start:      start,
		Minutes:    opts.minutes,
		Tokens:     tokens,
	}

	rep := runner.Run(ctxOrBackground(ctx), opts.n)
	failures := rep.Check(exclusive)

	if opts.asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		rep.Print(out, exclusive)
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d threshold(s) missed", len(failures))
	}
	return nil
}

// serveInProcess starts the real router on a loopback port.
//
// The same wiring cmd/api uses, minus Redis and the workers: neither is on the
// booking path, the rate limiter fails open without Redis, and a load profile
// should not be able to fail because a cache was cold. What it does keep is the
// entire middleware chain, which is the part being measured.
func serveInProcess(ctx context.Context, cfg *config.Config, db *store.DB, loc *time.Location) (func(), string, error) {
	// Silence the write-path log line for the duration of the run. §14 wants one
	// structured line per booking and the demo gets it; five hundred of them
	// racing to the same terminal during a measurement is the driver measuring
	// its own stderr.
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))

	facilities := facility.NewRepo(db.Primary)
	availability := facility.NewAvailability(db.Replica, nil, cfg.TZDisplay, slog.Default())
	bookings := booking.NewService(db, facilities, loc).
		WithAlternatives(booking.NewAlternatives(db.Replica, availability, cfg.TZDisplay)).
		WithPolicy(policy.Check)

	handler := httpx.NewRouter(httpx.RouterDeps{
		Config:       cfg,
		DB:           db,
		Bookings:     bookings,
		Facilities:   facilities,
		Availability: availability,
		Logger:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("load: listen: %w", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	stop := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	_ = ctx
	return stop, "http://" + ln.Addr().String(), nil
}

// mintTokens creates n throwaway students and signs a bearer for each.
//
// Distinct users, not one user n times: the fair-use cap and the (user,
// idem_key) unique index would both reject a repeat booker, and the run would
// then be measuring those rather than the exclusion constraint.
func mintTokens(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, n int) ([]string, error) {
	rolls := make([]string, n)
	names := make([]string, n)
	emails := make([]string, n)
	for i := 0; i < n; i++ {
		suffix := uuid.NewString()[:12]
		rolls[i] = "load-" + suffix
		names[i] = "Load " + suffix
		emails[i] = "load-" + suffix + "@iitg.ac.in"
	}

	rows, err := pool.Query(ctx, `
		INSERT INTO users (roll_no, name, email)
		SELECT * FROM unnest($1::text[], $2::text[], $3::citext[])
		RETURNING id, roll_no`, rolls, names, emails)
	if err != nil {
		return nil, fmt.Errorf("load: create users: %w", err)
	}
	defer rows.Close()

	auth := httpx.NewAuthenticator(cfg, pool)
	now := time.Now()

	tokens := make([]string, 0, n)
	for rows.Next() {
		var p httpx.Principal
		if err := rows.Scan(&p.UserID, &p.RollNo); err != nil {
			return nil, fmt.Errorf("load: scan user: %w", err)
		}
		p.Role = "STUDENT"
		tok, err := auth.Sign(p, now)
		if err != nil {
			return nil, fmt.Errorf("load: sign token: %w", err)
		}
		tokens = append(tokens, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load: create users: %w", err)
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("load: no tokens minted")
	}
	return tokens, nil
}

// resolveFacility accepts a UUID or one of the seeded slugs.
func resolveFacility(ctx context.Context, pool *pgxpool.Pool, ref string) (uuid.UUID, error) {
	if id, err := uuid.Parse(ref); err == nil {
		return id, nil
	}
	for _, f := range seed.Facilities {
		if f.Slug == ref {
			return f.ID(), nil
		}
	}
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM facilities WHERE name = $1`, ref).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("load: unknown facility %q: %w", ref, err)
	}
	return id, nil
}

func isExclusive(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) (bool, error) {
	var exclusive bool
	if err := pool.QueryRow(ctx,
		`SELECT is_exclusive FROM facilities WHERE id = $1`, id).Scan(&exclusive); err != nil {
		return false, fmt.Errorf("load: facility %s: %w (is the database seeded? run make seed)", id, err)
	}
	return exclusive, nil
}

// resolveStart reads HH:MM in the campus timezone, on today's date, rolling to
// tomorrow if that moment has already passed — a booking in the past is refused
// by validation and would never reach the constraint.
func resolveStart(at string, loc *time.Location) time.Time {
	now := time.Now().In(loc)
	t, err := time.ParseInLocation("15:04", at, loc)
	if err != nil {
		t, _ = time.ParseInLocation("15:04", "18:00", loc)
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	if !start.After(now) {
		start = start.AddDate(0, 0, 1)
	}
	return start.UTC()
}

// resetSlot frees the window so the profile can be re-run back to back.
//
// Status, not DELETE — the same move demo_reset_slot.sql makes, and for the same
// two reasons. A booking has an audit trail pointing at it, so deleting it
// violates a foreign key; and moving a row to CANCELLED drops it out of
// no_double_book's partial index, which frees the slot without there being any
// "mark it available" step to get wrong (non-negotiable #4).
//
// Not booking.Cancel: this is stage management, and a real cancellation would
// promote a waitlisted student into the slot half a second before the run fires.
func resetSlot(ctx context.Context, pool *pgxpool.Pool, facilityID uuid.UUID, start, end time.Time) error {
	_, err := pool.Exec(ctx, `
		UPDATE bookings
		   SET status = 'CANCELLED'
		 WHERE facility_id = $1
		   AND during && tstzrange($2, $3, '[)')
		   AND status IN ('CONFIRMED', 'HELD')`, facilityID, start, end)
	if err != nil {
		return fmt.Errorf("load: reset slot: %w", err)
	}
	return nil
}
