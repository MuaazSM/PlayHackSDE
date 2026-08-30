-- Reserve the freed window for the promoted student. IMPLEMENTATION.md §6.2.
--
-- A promotion offer that did not actually reserve the court would be a lie: the
-- next student to open the discovery grid would see the slot free, book it, and
-- the offer would evaporate. So the offer IS a booking row, in status HELD, and
-- HELD is inside no_double_book's predicate — the promoted student genuinely
-- holds the court for the length of their claim window.
--
-- That also means this INSERT can lose. If another cancel's promotion already
-- covered this range, or a student booked it in the gap, Postgres raises 23P01
-- exactly as it does on the main write path. The caller runs this inside a
-- SAVEPOINT and rolls the promotion back alone: a cancel must never fail
-- because a promotion failed.
--
-- No SELECT precedes it, here as everywhere else on the write path.
--
-- is_exclusive is true because only exclusive facilities promote today: a HELD
-- row reserves nothing on a shared facility, whose occupancy is the
-- slot_capacity counter rather than the exclusion constraint. The composite FK
-- (facility_id, is_exclusive) turns that assumption into a constraint — this
-- statement cannot silently insert a fake hold against the gymnasium.
--
-- $5 is the claim window in seconds, from PROMOTION_TTL_MIN.
INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status, held_until)
VALUES ($1, $2, true,
        tstzrange($3::timestamptz, $4::timestamptz, '[)'),
        'HELD',
        now() + make_interval(secs => $5::double precision))
RETURNING id, held_until, created_at;
