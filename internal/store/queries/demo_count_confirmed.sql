-- The proof. IMPLEMENTATION.md §13.
--
-- Issued AFTER every racing goroutine has finished, on the PRIMARY, in its own
-- statement. Everything else the race console prints is telemetry gathered by
-- the process that ran the race; this number is the database's own answer to
-- "how many people hold this court", asked fresh.
--
-- Primary, not the replica, deliberately: replication lag would let this report
-- a stale count, and a proof that can lag is not a proof.
--
-- Bounds are '[)' to match the exclusion constraint. Overlap (&&) rather than
-- equality, because a two-hour booking that straddles the demo slot occupies it
-- just as completely as an exact match, and a count that missed it would claim
-- the slot was free when it was not.
SELECT count(*)
  FROM bookings
 WHERE facility_id = $1
   AND during && tstzrange($2::timestamptz, $3::timestamptz, '[)')
   AND status = 'CONFIRMED';
