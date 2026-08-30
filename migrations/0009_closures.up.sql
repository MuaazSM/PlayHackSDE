-- 0009 — make the two states a manager closure needs representable.
--
-- A closure is a booking row (user_id NULL, status BLOCKED). Nothing about that
-- needs a new table, and this migration does not add one. What it does is widen
-- two CHECK constraints that were written before closures had a lifecycle, and
-- which between them made the feature impossible to express. Both changes are
-- strict relaxations: every row legal before is still legal.
--
-- ---------------------------------------------------------------------------
-- 1. bookings: a REOPENED closure is a CANCELLED row with no user.
--
--    The original constraint was
--        CHECK ((status = 'BLOCKED') = (user_id IS NULL))
--    which says a row has no user IF AND ONLY IF it is blocked. Reopening a
--    closure moves the row to CANCELLED — the same way every other release in
--    this system works, per booking_cancel.sql: the status change drops the row
--    out of no_double_book's partial index and the slot is bookable again the
--    instant the transaction commits. With the old constraint that UPDATE is
--    rejected, so the only alternatives would have been DELETE (which loses the
--    audit trail and breaks booking_events' foreign key) or a user_id on a
--    closure (which is a lie: nobody booked it).
--
--    The replacement keeps both halves of the original rule and admits exactly
--    one new state, the cancelled closure:
--      * a BLOCKED row still must have no user;
--      * a row with no user must be BLOCKED or CANCELLED.
--
-- ---------------------------------------------------------------------------
-- 2. slot_capacity: a closed slot may hold bookings that predate the closure.
--
--    IMPLEMENTATION.md §10.4 step 2: closing a SHARED facility sets
--    capacity = 0 for every overlapping slot, because the exclusion constraint
--    is scoped to is_exclusive and so a BLOCKED row alone blocks nothing on the
--    gym. Mechanism B's `booked < capacity` guard then fails trivially.
--
--    But a manager closes the gym for maintenance at 17:55 with five people
--    already booked, and §10.4 step 3 is explicit that those bookings are
--    FLAGGED for a human, not auto-cancelled. So booked = 5 while capacity = 0
--    is a real, correct, transient state — and CHECK (booked <= capacity)
--    rejected exactly it, which would have made the closure transaction fail
--    against precisely the slots it most needs to close.
--
--    The replacement keeps the invariant for every normal row and admits the
--    closed one. capacity = 0 is set by nothing but a closure: capacity_take
--    always inserts the facility's own capacity, which CHECK (capacity >= 1) on
--    facilities keeps positive.
--
--    Note this is NOT a weakening of the overbooking guard. The guard is
--    capacity_take's `WHERE slot_capacity.booked < slot_capacity.capacity`,
--    which with capacity = 0 refuses every take. This CHECK was a backstop, and
--    it stays one for every row a booking can reach.
-- ---------------------------------------------------------------------------

-- Both original constraints are unnamed, so their generated names
-- (bookings_check1, slot_capacity_check, …) depend on declaration order in
-- migration 0003/0004 and would be fragile to hardcode. Find each by its
-- definition instead, and give the replacements real names.
DO $$
DECLARE name text;
BEGIN
  SELECT conname INTO name
    FROM pg_constraint
   WHERE conrelid = 'bookings'::regclass
     AND contype = 'c'
     AND pg_get_constraintdef(oid) LIKE '%user_id IS NULL%';

  IF name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE bookings DROP CONSTRAINT %I', name);
  END IF;
END $$;

ALTER TABLE bookings
  ADD CONSTRAINT bookings_blocked_has_no_user
    CHECK (status <> 'BLOCKED' OR user_id IS NULL),
  ADD CONSTRAINT bookings_user_required
    CHECK (user_id IS NOT NULL OR status IN ('BLOCKED', 'CANCELLED'));

DO $$
DECLARE name text;
BEGIN
  SELECT conname INTO name
    FROM pg_constraint
   WHERE conrelid = 'slot_capacity'::regclass
     AND contype = 'c'
     AND pg_get_constraintdef(oid) LIKE '%booked <= capacity%';

  IF name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE slot_capacity DROP CONSTRAINT %I', name);
  END IF;
END $$;

ALTER TABLE slot_capacity
  ADD CONSTRAINT slot_capacity_not_overbooked
    CHECK (booked <= capacity OR capacity = 0);

-- Closures are read by facility and window on the manager console and on the
-- reopen path. The partial GiST index from 0007 already covers
-- (facility_id, during) WHERE status IN ('CONFIRMED','HELD','BLOCKED'), which
-- serves both, so nothing is added here.
