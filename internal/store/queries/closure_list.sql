-- Closures for the manager console. §10.4.
--
-- Only live closures: a reopened one is a CANCELLED row and is history, not a
-- closure. Availability derives the same way, so the console and the grid cannot
-- disagree about what is closed (non-negotiable #4).
--
-- Both filters are optional and applied with the ($n IS NULL OR …) idiom, so one
-- query serves "everything", "one facility", "one day" and "one facility on one
-- day" rather than four.
--
-- The day window arrives as two timestamptz bounds rather than as a date and a
-- timezone name. Localisation happens at the edge (CLAUDE.md), and passing a zone
-- string into SQL would also mean trusting Postgres' abbreviation table — where
-- 'IST' is Israel, not India, which is exactly the class of slot-boundary bug the
-- time rules exist to prevent.
--
-- $1 facility id or NULL, $2 window start or NULL, $3 window end or NULL.
SELECT b.id,
       b.facility_id,
       f.name,
       f.is_exclusive,
       lower(b.during),
       upper(b.during),
       b.created_at,
       ev.reason,
       ev.actor_id
  FROM bookings b
  JOIN facilities f ON f.id = b.facility_id
  LEFT JOIN LATERAL (
    SELECT e.reason, e.actor_id
      FROM booking_events e
     WHERE e.booking_id = b.id
       AND e.to_status = 'BLOCKED'
     ORDER BY e.at, e.id
     LIMIT 1
  ) ev ON true
 WHERE b.status = 'BLOCKED'
   AND ($1::uuid IS NULL OR b.facility_id = $1)
   AND ($2::timestamptz IS NULL
        OR b.during && tstzrange($2::timestamptz, $3::timestamptz, '[)'))
 ORDER BY lower(b.during), f.name;
