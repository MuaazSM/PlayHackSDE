-- Mechanism A. IMPLEMENTATION.md §4.3.
--
-- This is the entire concurrency control for exclusive facilities. There is no
-- SELECT before it and there must never be one: Postgres serialises conflicting
-- inserts on the GiST index and every loser raises 23P01.
--
-- Bounds are '[)' so 18:00-19:00 and 19:00-20:00 do not overlap.
INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status, idem_key)
VALUES ($1, $2, true, tstzrange($3::timestamptz, $4::timestamptz, '[)'), 'CONFIRMED', $5)
RETURNING id, created_at;
