-- 0005 — waitlist.
--
-- `position` as bigserial gives a monotonic, gapless-enough FIFO order for free:
-- no counter to increment, no race to order the queue itself. Promotion uses
-- FOR UPDATE SKIP LOCKED (§6.2) so concurrent cancellations promote DIFFERENT
-- students.
CREATE TABLE waitlist (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  facility_id   uuid NOT NULL REFERENCES facilities(id),
  user_id       uuid NOT NULL REFERENCES users(id),
  during        tstzrange NOT NULL,
  priority      int NOT NULL DEFAULT 0,
  position      bigserial NOT NULL,
  status        waitlist_status NOT NULL DEFAULT 'WAITING',
  booking_id    uuid REFERENCES bookings(id),   -- set on promotion
  offer_expires timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);

-- one live queue entry per user per slot
CREATE UNIQUE INDEX uq_waitlist_live
  ON waitlist (facility_id, user_id, during)
  WHERE status IN ('WAITING','PROMOTED');
