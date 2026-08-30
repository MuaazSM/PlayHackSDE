-- 0006 — operations tables: check_ins, policies, outbox (+ notify trigger),
-- booking_events.

CREATE TABLE check_ins (
  booking_id  uuid PRIMARY KEY REFERENCES bookings(id),
  at          timestamptz NOT NULL DEFAULT now(),
  method      text NOT NULL DEFAULT 'QR',
  token_id    text
);

CREATE TABLE policies (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  facility_id            uuid REFERENCES facilities(id),  -- NULL = global
  max_forward_bookings   int NOT NULL DEFAULT 3,
  max_weekly_hours       int NOT NULL DEFAULT 10,
  no_show_penalty_days   int NOT NULL DEFAULT 0,
  UNIQUE NULLS NOT DISTINCT (facility_id)
);

CREATE TABLE outbox (
  id           bigserial PRIMARY KEY,
  topic        text NOT NULL,
  payload      jsonb NOT NULL,
  status       text NOT NULL DEFAULT 'PENDING',   -- PENDING | SENT | FAILED
  attempts     int NOT NULL DEFAULT 0,
  created_at   timestamptz NOT NULL DEFAULT now(),
  sent_at      timestamptz
);

CREATE TABLE booking_events (
  id          bigserial PRIMARY KEY,
  booking_id  uuid NOT NULL REFERENCES bookings(id),
  actor_id    uuid REFERENCES users(id),
  from_status booking_status,
  to_status   booking_status NOT NULL,
  reason      text,
  at          timestamptz NOT NULL DEFAULT now()
);

-- Wake the dispatcher the instant the enclosing transaction commits.
--
-- pg_notify inside a trigger is delivered ON COMMIT, not on statement. That
-- single property is what makes the outbox pattern honest here: a notification
-- physically cannot be dispatched for a booking that rolled back.
CREATE FUNCTION notify_outbox() RETURNS trigger AS $$
BEGIN
  PERFORM pg_notify('outbox_new', NEW.id::text);
  RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER outbox_notify AFTER INSERT ON outbox
  FOR EACH ROW EXECUTE FUNCTION notify_outbox();
