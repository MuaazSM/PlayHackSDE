-- A closure. IMPLEMENTATION.md §10.4 step 1.
--
-- THERE IS NO CLOSURES TABLE, and that is the whole idea: a closure is a booking
-- row with no user and status BLOCKED, so the constraint that stops two students
-- sharing a court is the same one that stops a student booking a closed one. No
-- second mechanism, no second thing to keep in sync.
--
-- is_exclusive is passed in from the facility rather than hardcoded, because it
-- decides which mechanism does the blocking:
--
--   * true  — no_double_book covers this row, so the insert itself is rejected
--             with 23P01 if a booking already sits inside the window, and every
--             later booking attempt is rejected the same way.
--   * false — the constraint's predicate excludes this row entirely. The row
--             still makes the slot read as 'closed' on the grid, but it blocks
--             NOTHING on the write path. closure_zero_capacity.sql is what
--             actually closes a shared facility; see §10.4 step 2.
--
-- idem_key stays NULL. uq_bookings_user_idem is keyed on (user_id, idem_key) and
-- a closure's user_id is NULL, which a unique index treats as distinct from every
-- other NULL — so the index cannot deduplicate closures and pretending otherwise
-- would be ceremony. A replayed closure converges through closure_find.sql
-- instead.
--
-- Bounds are '[)' so a closure of 18:00-19:00 does not close the 19:00 slot.
INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status, idem_key)
VALUES ($1, NULL, $2, tstzrange($3::timestamptz, $4::timestamptz, '[)'), 'BLOCKED', NULL)
RETURNING id, created_at;
