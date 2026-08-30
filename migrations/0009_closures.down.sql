-- Reverse 0009. Both restorations NARROW the allowed states, so they fail if any
-- row is currently in one of the states 0009 admitted — a reopened closure, or a
-- closed slot holding bookings. That is the honest behaviour: reverting is only
-- safe once those rows are gone, and silently deleting somebody's closure to make
-- a down migration succeed would be worse than refusing.
ALTER TABLE slot_capacity DROP CONSTRAINT IF EXISTS slot_capacity_not_overbooked;
ALTER TABLE slot_capacity ADD CHECK (booked <= capacity);

ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_user_required;
ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_blocked_has_no_user;
ALTER TABLE bookings ADD CHECK ((status = 'BLOCKED') = (user_id IS NULL));
