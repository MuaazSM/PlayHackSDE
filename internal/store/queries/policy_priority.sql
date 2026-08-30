-- The two facts a queue priority is made of. IMPLEMENTATION.md §11.
--
--   $1  user id
--   $2  how far back a no-show still counts, in days
--
-- The tier is stored (migration 0010); the RANKING is not — Go turns a tier into
-- a number in one place, so SQL and Go cannot disagree about whether an
-- institute team outranks a hostel team.
--
-- No-shows are counted, not stored as a score. A penalty column would be a
-- second fact that could drift from the bookings it was derived from, the same
-- objection non-negotiable #4 makes to is_available. Counting here means a
-- no-show ages out of the window on its own, with nothing to run and nothing to
-- reset.
--
-- lower(during) rather than the row's created_at: the penalty is for not turning
-- up to a slot, so it is dated by the slot, not by when it was booked.
SELECT u.tier::text,
       (SELECT count(*)
          FROM bookings b
         WHERE b.user_id = u.id
           AND b.status = 'NO_SHOW'
           AND lower(b.during) >= now() - make_interval(days => $2::int))::int AS recent_no_shows
  FROM users u
 WHERE u.id = $1;
