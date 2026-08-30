-- Side effects go through the outbox. IMPLEMENTATION.md §4.1 step 7,
-- CLAUDE.md non-negotiable #7.
--
-- Written INSIDE the booking transaction. The AFTER INSERT trigger's pg_notify
-- fires on commit, not on statement, so a notification physically cannot be
-- dispatched for a booking that rolled back.
INSERT INTO outbox (topic, payload)
VALUES ($1, $2::jsonb)
RETURNING id;
