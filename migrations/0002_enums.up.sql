-- 0002 — enums.
--
-- HELD is not in the PRD's enum list but is in FR-02's slot states
-- (free / held / booked / closed). It is the state of a slot reserved for a
-- promoted waitlist user during their claim window (§6.3), so it must
-- participate in the exclusion constraint — otherwise a promotion offer would
-- not actually reserve anything.
CREATE TYPE booking_status  AS ENUM ('CONFIRMED','HELD','BLOCKED','CANCELLED','NO_SHOW','COMPLETED');
CREATE TYPE user_role       AS ENUM ('STUDENT','CAPTAIN','MANAGER','SECRETARY');
CREATE TYPE waitlist_status AS ENUM ('WAITING','PROMOTED','CLAIMED','EXPIRED','CANCELLED');
