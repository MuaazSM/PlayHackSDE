-- The booking row for a SHARED facility. IMPLEMENTATION.md §3.3, §4.4.
--
-- is_exclusive is false, which keeps this row out of the no_double_book
-- exclusion constraint entirely — that constraint's predicate is scoped on the
-- flag (migration 0003). Occupancy for shared facilities is decided by the
-- slot_capacity counter in capacity_take.sql, not by an overlap check, which is
-- the whole reason the two mechanisms can coexist.
INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status, idem_key)
VALUES ($1, $2, false, tstzrange($3::timestamptz, $4::timestamptz, '[)'), 'CONFIRMED', $5)
RETURNING id, created_at;
