-- Audit trail for a booking's status transitions. Written inside the same
-- transaction as the booking itself, so a rolled-back booking leaves no event.
INSERT INTO booking_events (booking_id, actor_id, from_status, to_status, reason)
VALUES ($1, $2, $3, $4::booking_status, $5);
