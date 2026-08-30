-- One student's current consumption of their fair-use budget. §11, §4.7.
--
-- READ THIS BEFORE MOVING IT: this is a read inside the write transaction, and
-- it is NOT the read-then-write non-negotiable #2 forbids. That rule is about
-- SLOT OCCUPANCY — reading whether a slot is free and then inserting into it.
-- Nothing here touches the contended slot. It reads the caller's OWN booking
-- history to evaluate a soft quota, which is why the check is advisory (§4.7):
-- two simultaneous requests can both pass it and a user can land one booking
-- over their cap. That is accepted. The slot invariant is still decided, in the
-- same transaction, by the exclusion constraint.
--
-- Everything is computed relative to the transaction's own now(), which is
-- returned so the caller decides whether the booking being requested falls
-- inside the rolling window without needing a second, differently-timed clock.
--
--   $1  user id
--   $2  facility id to scope to, or NULL for every facility
--   $3  rolling window width in days
--
-- $2 is the override story: a policy row that names a facility governs THAT
-- facility's usage, so its counters must only see that facility's bookings.
-- The global row is scoped to NULL and sees the whole campus.
--
-- `mine` is exactly idx_bookings_user_upcoming's predicate — (user_id,
-- lower(during)) WHERE status IN ('CONFIRMED','HELD') — so the index serves the
-- filter and the min() below. CANCELLED, NO_SHOW and COMPLETED rows are outside
-- it and therefore free the budget they held, which is the whole point of
-- cancelling.
--
-- "Forward" means the slot has not started yet. A booking already under way is
-- not something the student can still trade for another, so holding it against
-- their forward allowance would charge them twice for one afternoon.
WITH mine AS (
  SELECT b.during
    FROM bookings b
   WHERE b.user_id = $1
     AND b.status IN ('CONFIRMED', 'HELD')
     AND lower(b.during) >= now()
     AND ($2::uuid IS NULL OR b.facility_id = $2)
),
-- The rolling window: bookings starting inside [now, now + $3 days). Rolling,
-- not calendar — nothing here truncates to a week boundary, so an hour booked
-- on Sunday evening still counts against Monday morning's request.
window_bookings AS (
  SELECT during
    FROM mine
   WHERE lower(during) < now() + make_interval(days => $3::int)
)
SELECT now()                                                          AS as_of,
       (SELECT count(*) FROM mine)::int                               AS forward_count,
       -- When the earliest forward booking starts it stops being forward, which
       -- is the moment the forward allowance frees up by one.
       (SELECT min(lower(during)) FROM mine)                          AS forward_resets_at,
       (SELECT coalesce(
                 extract(epoch FROM sum(upper(during) - lower(during))),
                 0)::bigint
          FROM window_bookings)                                       AS window_seconds,
       -- When the earliest counted booking ENDS it leaves the window, which is
       -- the moment those hours come back.
       (SELECT min(upper(during)) FROM window_bookings)               AS window_resets_at;
