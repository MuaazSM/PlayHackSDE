// Package seed populates the demo dataset: the seven facilities from
// IMPLEMENTATION.md §0, one global policy row, and twelve users.
//
// Every statement is an upsert keyed on a deterministic UUID, so Run is safe to
// re-execute mid-demo: row counts do not change and canonical values are
// restored. Nothing here deletes.
package seed

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// namespace makes every seeded id a pure function of its natural key. Stable
// ids survive a re-seed and are greppable in logs during the demo.
var namespace = uuid.NewSHA1(uuid.NameSpaceDNS, []byte("playhack.iitg.ac.in"))

// ID returns the deterministic UUID for a seed key such as
// "facility:tennis-court-1" or "user:student01".
func ID(key string) uuid.UUID { return uuid.NewSHA1(namespace, []byte(key)) }

// Facility is a seeded venue.
type Facility struct {
	Slug        string
	Name        string
	Sport       string
	IsExclusive bool
	Capacity    int
	OpensAt     string // time-of-day, IST wall clock
	ClosesAt    string
	Granularity string // interval literal
	MinDuration string
	MaxDuration string
}

// ID is the facility's deterministic primary key.
func (f Facility) ID() uuid.UUID { return ID("facility:" + f.Slug) }

// Facilities is the seeded reality from IMPLEMENTATION.md §0: six exclusive
// venues and one shared gymnasium at capacity 30.
//
// Six-and-one is deliberate. It gives both concurrency mechanisms something real
// to run against, and gives the 409 response somewhere to point — Tennis Court 2
// is the alternative for Tennis Court 1.
var Facilities = []Facility{
	{"tennis-court-1", "Tennis Court 1", "tennis", true, 1, "06:00", "22:00", "60 minutes", "60 minutes", "120 minutes"},
	{"tennis-court-2", "Tennis Court 2", "tennis", true, 1, "06:00", "22:00", "60 minutes", "60 minutes", "120 minutes"},
	{"badminton-court-1", "Badminton Court 1", "badminton", true, 1, "06:00", "22:00", "60 minutes", "60 minutes", "120 minutes"},
	{"badminton-court-2", "Badminton Court 2", "badminton", true, 1, "06:00", "22:00", "60 minutes", "60 minutes", "120 minutes"},
	{"football-field", "Football Field", "football", true, 1, "06:00", "20:00", "60 minutes", "60 minutes", "120 minutes"},
	{"cricket-ground", "Cricket Ground", "cricket", true, 1, "06:00", "20:00", "60 minutes", "60 minutes", "180 minutes"},
	// The only shared facility. Mechanism B exists solely for this row.
	{"gymnasium", "Gymnasium", "gym", false, 30, "05:00", "23:00", "60 minutes", "60 minutes", "60 minutes"},
}

// User is a seeded account.
type User struct {
	RollNo string
	Name   string
	Email  string
	Role   string
}

// ID is the user's deterministic primary key.
func (u User) ID() uuid.UUID { return ID("user:" + u.RollNo) }

// Users is student01..student10, manager01 and secretary01.
//
// The race demo needs a pool of distinct user IDs to fire as; ten real student
// rows plus generated ones is enough.
var Users = buildUsers()

func buildUsers() []User {
	us := make([]User, 0, 12)
	for i := 1; i <= 10; i++ {
		roll := fmt.Sprintf("student%02d", i)
		us = append(us, User{
			RollNo: roll,
			Name:   fmt.Sprintf("Student %02d", i),
			Email:  roll + "@iitg.ac.in",
			Role:   "STUDENT",
		})
	}
	us = append(us,
		User{RollNo: "manager01", Name: "Manager 01", Email: "manager01@iitg.ac.in", Role: "MANAGER"},
		User{RollNo: "secretary01", Name: "Secretary 01", Email: "secretary01@iitg.ac.in", Role: "SECRETARY"},
	)
	return us
}

// GlobalPolicyID is the id of the single facility_id IS NULL policy row.
var GlobalPolicyID = ID("policy:global")

// Result reports what a Run touched.
type Result struct {
	Facilities int
	Users      int
	Policies   int
}

func (r Result) String() string {
	return fmt.Sprintf("facilities=%d users=%d policies=%d", r.Facilities, r.Users, r.Policies)
}

const upsertFacility = `
INSERT INTO facilities (id, name, sport, is_exclusive, capacity,
                        opens_at, closes_at, granularity, min_duration, max_duration, is_active)
VALUES ($1, $2, $3, $4, $5, $6::time, $7::time, $8::interval, $9::interval, $10::interval, true)
ON CONFLICT (id) DO UPDATE SET
  name         = EXCLUDED.name,
  sport        = EXCLUDED.sport,
  is_exclusive = EXCLUDED.is_exclusive,
  capacity     = EXCLUDED.capacity,
  opens_at     = EXCLUDED.opens_at,
  closes_at    = EXCLUDED.closes_at,
  granularity  = EXCLUDED.granularity,
  min_duration = EXCLUDED.min_duration,
  max_duration = EXCLUDED.max_duration,
  is_active    = true`

const upsertUser = `
INSERT INTO users (id, roll_no, name, email, role)
VALUES ($1, $2, $3, $4, $5::user_role)
ON CONFLICT (id) DO UPDATE SET
  roll_no = EXCLUDED.roll_no,
  name    = EXCLUDED.name,
  email   = EXCLUDED.email,
  role    = EXCLUDED.role`

const upsertPolicy = `
INSERT INTO policies (id, facility_id, max_forward_bookings, max_weekly_hours, no_show_penalty_days)
VALUES ($1, NULL, $2, $3, $4)
ON CONFLICT (id) DO UPDATE SET
  max_forward_bookings = EXCLUDED.max_forward_bookings,
  max_weekly_hours     = EXCLUDED.max_weekly_hours,
  no_show_penalty_days = EXCLUDED.no_show_penalty_days`

// Run applies the seed. It is idempotent: running it twice leaves row counts
// unchanged.
func Run(ctx context.Context, pool *pgxpool.Pool) (Result, error) {
	var res Result

	tx, err := pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("seed: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if res, err = runTx(ctx, tx); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("seed: commit: %w", err)
	}
	return res, nil
}

func runTx(ctx context.Context, tx pgx.Tx) (Result, error) {
	var res Result

	for _, f := range Facilities {
		_, err := tx.Exec(ctx, upsertFacility,
			f.ID(), f.Name, f.Sport, f.IsExclusive, f.Capacity,
			f.OpensAt, f.ClosesAt, f.Granularity, f.MinDuration, f.MaxDuration)
		if err != nil {
			return res, fmt.Errorf("seed: facility %s: %w", f.Slug, err)
		}
		res.Facilities++
	}

	for _, u := range Users {
		if _, err := tx.Exec(ctx, upsertUser, u.ID(), u.RollNo, u.Name, u.Email, u.Role); err != nil {
			return res, fmt.Errorf("seed: user %s: %w", u.RollNo, err)
		}
		res.Users++
	}

	// One global row: max 3 forward bookings, 10 hours a week, no no-show penalty.
	if _, err := tx.Exec(ctx, upsertPolicy, GlobalPolicyID, 3, 10, 0); err != nil {
		return res, fmt.Errorf("seed: global policy: %w", err)
	}
	res.Policies++

	return res, nil
}
