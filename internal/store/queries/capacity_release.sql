-- The mirror of capacity_take, for cancel and no-show. IMPLEMENTATION.md §4.5.
--
-- The booked > 0 guard is what stops a double-cancel underflowing the counter
-- below zero.
UPDATE slot_capacity
   SET booked = booked - 1
 WHERE facility_id = $1 AND slot_start = $2::timestamptz AND booked > 0
RETURNING booked, capacity;
