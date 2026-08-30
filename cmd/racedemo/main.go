// Command racedemo fires N concurrent booking attempts at one facility and one
// slot, then prints the outcome split and the database's own count of confirmed
// bookings for that slot.
//
// This is the race console as a CLI — the same internal/demo service the
// POST /api/v1/demo/race endpoint and the /race screen use. It calls the booking
// service directly, in-process: no HTTP, no rate limiter, no load shedder, and
// no Redis (the client below is explicitly nil). The only thing it needs is a
// local Postgres, so venue wifi cannot break it.
//
//	make race-demo N=500
//	go run ./cmd/racedemo -n 500 -facility tennis-court-1 -at 18:00
//
// Exit status is 0 when the invariant holds — exactly one CONFIRMED booking for
// the slot, and every loser lost cleanly — and 1 when it does not. A run where
// 499 requests fail with an unclassified error also yields db_count == 1 and is
// NOT a pass.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/demo"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/seed"
	"github.com/iitg-playhack/sportsbook/internal/store"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "racedemo: %v\n", err)
		os.Exit(1)
	}
}

// options are the CLI's flags, parsed into one struct so run stays testable.
type options struct {
	n         int
	facility  string
	at        string
	minutes   int
	reset     bool
	resetOnly bool
	asJSON    bool
}

func parseFlags(args []string, out io.Writer) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet("racedemo", flag.ContinueOnError)
	fs.SetOutput(out)

	fs.IntVar(&o.n, "n", 500, "number of concurrent booking attempts to fire")
	fs.StringVar(&o.facility, "facility", "tennis-court-1", "facility slug or UUID")
	fs.StringVar(&o.at, "at", "18:00", "slot start: HH:MM in the campus timezone, or an RFC3339 timestamp")
	fs.IntVar(&o.minutes, "minutes", 60, "slot length in minutes")
	fs.BoolVar(&o.reset, "reset", true,
		"clear the slot before firing, so the demo is re-runnable back to back. "+
			"-reset=false is the 'fire again — still 1' beat")
	fs.BoolVar(&o.resetOnly, "reset-only", false, "clear the slot and exit without racing")
	fs.BoolVar(&o.asJSON, "json", false, "emit the result as JSON instead of a readable split")

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

	// A generous ceiling: 5000 attempts against a laptop Postgres is minutes,
	// not seconds, and a demo that gave up halfway would be worse than a slow one.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDial()

	db, err := store.New(dialCtx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	svc := newDemoService(db, cfg, loc)

	facilityID, err := resolveFacility(opts.facility)
	if err != nil {
		return err
	}
	start, err := resolveStart(opts.at, loc)
	if err != nil {
		return err
	}
	end := start.Add(time.Duration(opts.minutes) * time.Minute)

	name := facilityLabel(facilityID, opts.facility)

	var cleared *demo.ResetResult
	if opts.reset || opts.resetOnly {
		if cleared, err = svc.Reset(ctx, facilityID, start, end); err != nil {
			return err
		}
	}

	if opts.resetOnly {
		if opts.asJSON {
			return writeJSON(out, cleared)
		}
		fmt.Fprintf(out, "reset %s %s — cancelled %d, slot now holds %d\n",
			name, start.In(loc).Format("2006-01-02 15:04 MST"), cleared.Cancelled, cleared.DBCount)
		return nil
	}

	res, err := svc.Run(ctx, demo.Request{
		FacilityID: facilityID,
		Start:      start,
		Duration:   time.Duration(opts.minutes) * time.Minute,
		N:          opts.n,
	})
	if err != nil {
		return err
	}

	if opts.asJSON {
		if err := writeJSON(out, res); err != nil {
			return err
		}
	} else {
		report(out, name, start.In(loc), cleared, res)
	}

	// The exit code is the invariant, not the outcome split. Firing again
	// without a reset legitimately yields zero confirmations and n conflicts;
	// what must never happen is two rows in the slot, an unclassified failure,
	// or more than one goroutine believing it won.
	switch {
	case res.DBCount != 1:
		return fmt.Errorf("INVARIANT VIOLATED: the database holds %d confirmed bookings for this slot, expected 1", res.DBCount)
	case res.Other > 0:
		return fmt.Errorf("INVARIANT VIOLATED: %d attempts failed with something other than a clean conflict: %v", res.Other, res.Errors)
	case res.Confirmed > 1:
		return fmt.Errorf("INVARIANT VIOLATED: %d attempts believe they won", res.Confirmed)
	}
	return nil
}

// newDemoService wires the race console over the same write path a student
// books through.
//
// Alternatives are attached because a real 409 carries them, and leaving them
// off would report a rejection latency the product never actually delivers.
// Redis is deliberately nil: nothing on this path is allowed to need it
// (non-negotiable #3), and passing nil is the cheapest way to keep that true by
// construction rather than by intention.
func newDemoService(db *store.DB, cfg *config.Config, loc *time.Location) *demo.Service {
	facilities := facility.NewRepo(db.Primary)
	availability := facility.NewAvailability(db.Replica, nil, cfg.TZDisplay,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	bookings := booking.NewService(db, facilities, loc).
		WithAlternatives(booking.NewAlternatives(db.Replica, availability, cfg.TZDisplay))

	return demo.NewService(db, bookings, facilities)
}

// resolveFacility accepts a UUID or one of the seeded slugs, so the common case
// is `-facility tennis-court-1` rather than pasting a UUID on stage.
func resolveFacility(ref string) (uuid.UUID, error) {
	if id, err := uuid.Parse(ref); err == nil {
		return id, nil
	}
	for _, f := range seed.Facilities {
		if f.Slug == ref {
			return f.ID(), nil
		}
	}
	return uuid.Nil, fmt.Errorf("unknown facility %q — use a UUID or one of the seeded slugs (e.g. tennis-court-1)", ref)
}

// resolveStart turns -at into an absolute instant.
//
// A bare HH:MM means the NEXT occurrence of that wall-clock time in the campus
// timezone — today if it is still ahead, tomorrow otherwise. Rehearsing at
// midnight must not fail with "slot is in the past"; the demo has to work at
// whatever hour it is actually run.
func resolveStart(at string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		return t.UTC(), nil
	}

	hm, err := time.Parse("15:04", at)
	if err != nil {
		return time.Time{}, fmt.Errorf("-at must be HH:MM or an RFC3339 timestamp, got %q", at)
	}

	now := time.Now().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
	if !start.After(now) {
		start = start.AddDate(0, 0, 1)
	}
	return start.UTC(), nil
}

// facilityLabel resolves the facility's display name from the seed catalogue,
// falling back to whatever the caller typed. A label is cosmetic; spending a
// query on one, or failing the demo over one, is not.
func facilityLabel(id uuid.UUID, fallback string) string {
	for _, f := range seed.Facilities {
		if f.ID() == id {
			return f.Name
		}
	}
	return fallback
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// report prints the outcome split. This is what a judge reads over a shoulder,
// so the proof line is separated from the telemetry and stated as the SQL that
// produced it.
func report(out io.Writer, name string, start time.Time, cleared *demo.ResetResult, res *demo.Result) {
	fmt.Fprintf(out, "\nracedemo  %d concurrent attempts  %s  %s\n\n",
		res.N, name, start.Format("2006-01-02 15:04 MST"))

	if cleared != nil {
		fmt.Fprintf(out, "  reset           cancelled %d, slot cleared to %d\n\n",
			cleared.Cancelled, cleared.DBCount)
	}

	fmt.Fprintf(out, "  confirmed       %d\n", res.Confirmed)
	fmt.Fprintf(out, "  conflict 409    %d\n", res.Conflict409)
	fmt.Fprintf(out, "  other           %d\n", res.Other)
	for _, e := range res.Errors {
		fmt.Fprintf(out, "                  %s\n", e)
	}

	fmt.Fprintf(out, "\n  elapsed         %d ms\n", res.ElapsedMS)
	fmt.Fprintf(out, "  p50 / p99       %d / %d ms\n", res.P50MS, res.P99MS)
	// Deliberately NOT labelled against the M-3 target. M-3 (rejection p99 <
	// 150 ms) is a claim about the served path, where the shedder bounds
	// admitted concurrency to WRITE_QUEUE_DEPTH and most of a burst gets a fast
	// 429. This path admits all N on purpose, so its p99 is the unshed curve —
	// a different measurement, and quoting a target next to it would invite the
	// comparison the demo is careful not to make. The served number is measured
	// in test/concurrency/shed_race_test.go.
	fmt.Fprintf(out, "  reject p99      %d ms   (all %d admitted — no shedder on this path)\n",
		res.RejectP99MS, res.N)
	fmt.Fprintf(out, "  start spread    %d ms   (how simultaneous they really were)\n", res.StartSpreadMS)

	fmt.Fprintf(out, "\n  SELECT count(*) FROM bookings\n")
	fmt.Fprintf(out, "   WHERE facility_id = … AND during && … AND status = 'CONFIRMED'\n")
	fmt.Fprintf(out, "  ->  %d\n", res.DBCount)

	if res.Winner != nil {
		fmt.Fprintf(out, "\n  winner          %s   %s\n", res.Winner.User, res.Winner.Reference)
	}

	fmt.Fprintf(out, "\nconfirmed=%d conflicts=%d other=%d db_count=%d\n\n",
		res.Confirmed, res.Conflict409, res.Other, res.DBCount)
}
