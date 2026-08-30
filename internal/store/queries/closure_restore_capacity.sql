-- Reopen: give a slot its capacity back. The mirror of closure_zero_capacity.sql.
--
-- Two guards, both load-bearing:
--
--   capacity = 0   — only ever set by a closure, so this can never overwrite a
--                    capacity somebody edited on the facility. A slot that was
--                    not closed is left alone, and reopening twice is a no-op
--                    rather than a second restoration.
--
--   NOT EXISTS …   — another closure may still cover this slot. Two managers can
--                    close overlapping windows for different reasons; withdrawing
--                    one of them must not reopen the gym while the other stands.
--                    The reopen's own BLOCKED row is already CANCELLED earlier in
--                    this transaction, so it does not match itself.
--
-- Zero rows returned is not an error. It means the slot was never closed, or is
-- still closed by someone else, and both are correct outcomes for this statement.
--
-- $1 facility id, $2 slot start, $3 the facility's declared capacity.
UPDATE slot_capacity sc
   SET capacity = $3
 WHERE sc.facility_id = $1
   AND sc.slot_start = $2::timestamptz
   AND sc.capacity = 0
   AND NOT EXISTS (
         SELECT 1
           FROM bookings b
          WHERE b.facility_id = sc.facility_id
            AND b.status = 'BLOCKED'
            AND b.during && tstzrange(sc.slot_start, sc.slot_end, '[)')
       )
RETURNING sc.slot_start, sc.booked, sc.capacity;
