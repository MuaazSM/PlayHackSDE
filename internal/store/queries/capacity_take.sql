-- Mechanism B. IMPLEMENTATION.md §4.4.
--
-- One statement that lazily creates the counter row and increments it under the
-- cap. Zero rows returned means the slot is full.
--
-- ON CONFLICT DO UPDATE takes a row lock for the statement's duration, so
-- concurrent increments serialise correctly. Multi-slot bookings run one of
-- these per slot in ascending slot_start order — consistent ordering across all
-- callers is what keeps it deadlock-free.
INSERT INTO slot_capacity (facility_id, slot_start, slot_end, capacity, booked)
VALUES ($1, $2::timestamptz, $3::timestamptz, $4, 1)
ON CONFLICT (facility_id, slot_start) DO UPDATE
   SET booked = slot_capacity.booked + 1
 WHERE slot_capacity.booked < slot_capacity.capacity
RETURNING booked, capacity;
