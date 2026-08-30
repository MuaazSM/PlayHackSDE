-- Reclaim promotion offers nobody accepted. IMPLEMENTATION.md §6.3.
--
-- Every 30 seconds: any HELD booking whose claim window has closed goes to
-- CANCELLED, which drops it out of no_double_book's predicate and makes the
-- court bookable again the instant this transaction commits. There is no
-- "release" step here either — non-negotiable #4 holds on the expiry path as
-- much as on the cancel path.
--
-- FOR UPDATE SKIP LOCKED on the inner select for the same reason the promotion
-- itself uses it: two sweeper replicas (or a sweeper and a claim in flight)
-- must not queue behind each other over one row. A row somebody else is already
-- working on is somebody else's to finish.
--
-- held_until is cleared to satisfy CHECK ((status = 'HELD') = (held_until IS
-- NOT NULL)) — the schema will not let an expired hold keep looking like one.
--
-- $1 bounds the batch. Each returned window is then offered to the next student
-- in line through waitlist_claim_head, in this same transaction.
UPDATE bookings b
   SET status = 'CANCELLED',
       held_until = NULL
 WHERE b.id IN (
         SELECT id
           FROM bookings
          WHERE status = 'HELD'
            AND held_until <= now()
          ORDER BY held_until
            FOR UPDATE SKIP LOCKED
          LIMIT $1
       )
RETURNING b.id, b.facility_id, b.user_id,
          lower(b.during) AS starts, upper(b.during) AS ends;
