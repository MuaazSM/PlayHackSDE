-- THE STEP THAT MAKES A GYM CLOSURE ACTUALLY CLOSE ANYTHING. §10.4 step 2.
--
-- no_double_book's predicate is scoped to `is_exclusive` (migration 0003), which
-- is what lets Mechanisms A and B coexist. The cost of that scoping is paid right
-- here: a BLOCKED row on a SHARED facility is not in the constraint's index at
-- all, so on its own it blocks precisely nothing. The grid would show 'closed'
-- and the gym would keep taking bookings — a silent, demo-shaped failure.
--
-- Setting capacity = 0 for every overlapping slot is the partner step. Mechanism
-- B's guard is `WHERE slot_capacity.booked < slot_capacity.capacity`; with
-- capacity 0 it can never hold, so capacity_take returns zero rows and the
-- booking is refused as CAPACITY_FULL. Same single-statement, no-gap property as
-- every other take — nothing here reads occupancy to decide.
--
-- The row is UPSERTED because counter rows are created lazily: most closed slots
-- have never been booked and so have no row to update. Inserting one with
-- capacity 0 and booked 0 closes the slot pre-emptively.
--
-- booked is deliberately NOT touched. Bookings that predate the closure keep
-- their places until a human cancels them (§10.4 step 3), so `booked > capacity`
-- is a legitimate transient state — migration 0009 widened the CHECK to admit
-- exactly it.
--
-- Concurrency against a booking landing at the same instant: both statements
-- contend for the same (facility_id, slot_start) row. ON CONFLICT DO UPDATE takes
-- a row lock, and the loser re-evaluates against the committed version — so if
-- the closure commits first the booking's guard sees capacity 0 and fails, and if
-- the booking commits first it appears in closure_affected.sql for staff. There
-- is no ordering in which both are ignored.
--
-- $1 facility id, $2 slot start, $3 slot end.
INSERT INTO slot_capacity (facility_id, slot_start, slot_end, capacity, booked)
VALUES ($1, $2::timestamptz, $3::timestamptz, 0, 0)
ON CONFLICT (facility_id, slot_start) DO UPDATE
   SET capacity = 0
RETURNING slot_start, booked, capacity;
