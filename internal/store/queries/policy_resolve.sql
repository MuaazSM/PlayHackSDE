-- Resolve the fair-use policy that governs one facility. IMPLEMENTATION.md §11.
--
-- At most two rows can match: the facility's own override, and the global row
-- (facility_id IS NULL). `UNIQUE NULLS NOT DISTINCT (facility_id)` in migration
-- 0006 is what guarantees "at most one of each" — without NULLS NOT DISTINCT a
-- unique index treats every NULL as distinct and nothing would stop a second,
-- contradictory global row from existing.
--
-- NULLS LAST puts the override ahead of the global row, so LIMIT 1 picks the
-- most specific policy. Ordering by facility_id among non-NULLs is meaningless
-- and harmless: only one non-NULL row can match `facility_id = $1`.
--
-- No row at all means no policy has been configured, which means unlimited. A
-- caps system that fails CLOSED on a missing configuration row would refuse
-- every booking in a database nobody has seeded — the opposite of what a soft
-- quota should do when it does not know the answer.
SELECT facility_id IS NOT NULL AS facility_specific,
       max_forward_bookings,
       max_weekly_hours,
       no_show_penalty_days
  FROM policies
 WHERE facility_id = $1 OR facility_id IS NULL
 ORDER BY facility_id NULLS LAST
 LIMIT 1;
