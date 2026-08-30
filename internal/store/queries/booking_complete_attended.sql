-- Retire the bookings that were actually used. IMPLEMENTATION.md §7.
--
-- A booking whose window has closed and which HAS an attendance record is
-- COMPLETED. This is bookkeeping, not a release: the window is already in the
-- past, so nothing about the court's availability changes and no notification is
-- owed. What it buys is a truthful "my bookings" list and honest analytics —
-- without it every attended booking would sit at CONFIRMED forever and the
-- no-show rate M-6 measures would be computed against a denominator that never
-- settles.
--
-- Ordered and locked exactly as booking_mark_no_show.sql is, and for the same
-- reason: the two statements run in one sweep transaction, and two sweepers must
-- take disjoint rows in a consistent order rather than deadlocking against each
-- other.
--
-- $1 bounds the batch.
UPDATE bookings b
   SET status = 'COMPLETED'
 WHERE b.id IN (
         SELECT x.id
           FROM bookings x
          WHERE x.status = 'CONFIRMED'
            AND upper(x.during) <= now()
            AND EXISTS (SELECT 1 FROM check_ins c WHERE c.booking_id = x.id)
          ORDER BY upper(x.during)
            FOR UPDATE SKIP LOCKED
          LIMIT $1
       )
RETURNING b.id, b.facility_id, b.user_id,
          lower(b.during) AS starts, upper(b.during) AS ends;
