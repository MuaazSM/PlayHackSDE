-- Subscribe the dispatcher's dedicated connection to the outbox trigger. §8.
--
-- The channel name cannot be parameterised, which is exactly why it is pinned in
-- a file next to the trigger that publishes it (migration 0006) rather than
-- assembled from a string somewhere in Go.
--
-- This connection must talk DIRECTLY to Postgres. LISTEN is session state, and a
-- transaction-mode pooler hands the backend to somebody else between
-- transactions — the subscription would silently evaporate.
LISTEN outbox_new;
