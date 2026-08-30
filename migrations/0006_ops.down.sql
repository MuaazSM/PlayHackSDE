DROP TRIGGER IF EXISTS outbox_notify ON outbox;
DROP FUNCTION IF EXISTS notify_outbox();
DROP TABLE IF EXISTS booking_events;
DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS policies;
DROP TABLE IF EXISTS check_ins;
