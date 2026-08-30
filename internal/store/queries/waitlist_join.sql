-- Join the queue for a window. IMPLEMENTATION.md §6.2.
--
-- position is a bigserial: the sequence hands out the ordering key, so nothing
-- reads a counter and increments it, and two students joining the same queue in
-- the same millisecond cannot receive the same place. It is the queue's order,
-- not the student's place in it — see waitlist_place.sql for that.
--
-- A second live entry for the same (facility, user, window) raises 23505 on
-- uq_waitlist_live, which store.Classify maps to ErrAlreadyWaiting → 409. The
-- duplicate is rejected by the index, not by a prior SELECT: the same
-- read-then-write ban applies here as on the booking path.
INSERT INTO waitlist (facility_id, user_id, during, priority)
VALUES ($1, $2, tstzrange($3::timestamptz, $4::timestamptz, '[)'), $5)
RETURNING id, position, priority, created_at;
