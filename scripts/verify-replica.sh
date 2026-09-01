#!/usr/bin/env sh
set -eu

compose="docker compose -f deploy/docker-compose.yml --profile replica"
probe="replica-$(date +%s)"

primary() {
  $compose exec -T postgres psql -v ON_ERROR_STOP=1 -U playhack -d playhack "$@"
}

replica() {
  $compose exec -T postgres-replica psql -v ON_ERROR_STOP=1 -U playhack -d playhack "$@"
}

if [ "$(replica -Atc 'select pg_is_in_recovery()')" != "t" ]; then
  echo "postgres-replica is not in recovery" >&2
  exit 1
fi

primary -c 'CREATE TABLE IF NOT EXISTS replication_probe(id integer PRIMARY KEY, note text NOT NULL)' >/dev/null
primary -c "INSERT INTO replication_probe(id, note) VALUES (1, '$probe') ON CONFLICT (id) DO UPDATE SET note = EXCLUDED.note" >/dev/null

i=0
while [ "$i" -lt 50 ]; do
  if [ "$(replica -Atc 'select note from replication_probe where id = 1' 2>/dev/null || true)" = "$probe" ]; then
    echo "streaming replica verified: $probe"
    primary -c 'DROP TABLE replication_probe' >/dev/null
    exit 0
  fi
  i=$((i + 1))
  sleep 0.2
done

echo "replica did not replay the primary probe within 10 seconds" >&2
exit 1
