-- Alternatives, question 2: the SAME time, a different court. §5.3.
--
-- The other half of "make losing useful". Someone who wanted 18:00 tennis
-- usually wanted 18:00 more than they wanted that particular court, so a free
-- Court 2 at the same hour converts the loss into a second attempt without
-- asking them to rearrange their evening.
--
-- Same rules as alternatives_same_facility.sql: replica only, after rollback,
-- inside the 40 ms budget, and the answer is advisory — the constraint still
-- decides when the user taps it.
--
-- $1 sport, $2 facility to exclude (the one they just lost),
-- $3 start, $4 end, $5 date (local), $6 timezone, $7 row limit.
SELECT f.id,
       f.name,
       f.sport,
       $3::timestamptz AS slot_start,
       $4::timestamptz AS slot_end
  FROM facilities f
 WHERE f.is_active
   AND f.sport = $1
   AND f.id <> $2
   -- Inside that facility's own opening hours, on its own local day. Comparing
   -- UTC here would reject an 18:00 IST slot against a 06:00-22:00 window.
   AND $3::timestamptz >= ($5::date + f.opens_at)  AT TIME ZONE $6
   AND $4::timestamptz <= ($5::date + f.closes_at) AT TIME ZONE $6
   AND ($4::timestamptz - $3::timestamptz) BETWEEN f.min_duration AND f.max_duration
   -- Shared facilities book on a fixed grid: slot_capacity is keyed on
   -- slot_start, so an off-grid start has no counter row to increment and the
   -- write path would reject it with 422 SLOT_NOT_ALIGNED. Suggesting something
   -- that cannot be booked is worse than suggesting nothing. Exclusive
   -- facilities carry a tstzrange and need no grid at all, so they skip this.
   AND (
        f.is_exclusive
        OR EXTRACT(epoch FROM ($3::timestamptz - (($5::date + f.opens_at) AT TIME ZONE $6)))::bigint
           % GREATEST(EXTRACT(epoch FROM f.granularity)::bigint, 1) = 0
       )
   AND NOT EXISTS (
         SELECT 1
           FROM bookings b
          WHERE b.facility_id = f.id
            AND (b.is_exclusive OR b.status = 'BLOCKED')
            AND b.status IN ('CONFIRMED','HELD','BLOCKED')
            AND b.during && tstzrange($3::timestamptz, $4::timestamptz, '[)')
       )
   AND NOT EXISTS (
         SELECT 1
           FROM slot_capacity sc
          WHERE sc.facility_id = f.id
            AND sc.slot_start >= $3::timestamptz
            AND sc.slot_start <  $4::timestamptz
            AND sc.booked >= sc.capacity
       )
 -- Stable order, so the same loss suggests the same court twice running.
 ORDER BY f.name
 LIMIT $7;
