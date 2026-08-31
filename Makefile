# PlayHack — IMPLEMENTATION.md §2.3.
# CLAUDE.md promises this exact target set; the doc is a contract, keep it true.

COMPOSE := docker compose -f deploy/docker-compose.yml

# Host ports. Override these if you already run PostgreSQL or Redis locally —
# a host-installed server binds 127.0.0.1 first and silently wins over Docker's
# wildcard bind, so `localhost:5432` would reach the wrong database.
#   POSTGRES_HOST_PORT=55432 PGBOUNCER_HOST_PORT=56432 REDIS_HOST_PORT=56379 make dev
POSTGRES_HOST_PORT  ?= 5432
PGBOUNCER_HOST_PORT ?= 6432
REDIS_HOST_PORT     ?= 6379
export POSTGRES_HOST_PORT PGBOUNCER_HOST_PORT REDIS_HOST_PORT

# Through PgBouncer (transaction mode) — this is what the app uses.
DB_URL         ?= postgres://playhack:playhack@localhost:$(PGBOUNCER_HOST_PORT)/playhack?sslmode=disable
# Direct to Postgres. golang-migrate takes a SESSION advisory lock, which a
# transaction-mode pooler cannot honour, so migrations must bypass PgBouncer.
DB_MIGRATE_URL ?= postgres://playhack:playhack@localhost:$(POSTGRES_HOST_PORT)/playhack?sslmode=disable
# Also direct, and for the same family of reason: the outbox dispatcher holds a
# LISTEN, which is session state. Through PgBouncer the subscription would look
# established and then quietly receive nothing — the dispatcher would fall back
# to its 5s ticker and nobody would be told why.
DB_LISTEN_URL  ?= $(DB_MIGRATE_URL)
REDIS_URL      ?= redis://localhost:$(REDIS_HOST_PORT)
# The read/write split. Empty means availability reads fall back to the primary,
# which is a supported configuration and not a degraded one — IMPLEMENTATION.md
# §2.1. `make dev-replica` brings the standby up and this is what points at it:
#   DB_REPLICA_URL=postgres://playhack:playhack@localhost:5433/playhack?sslmode=disable make run
DB_REPLICA_URL ?=

# WRITE_QUEUE_DEPTH, measured on THIS hardware rather than assumed.
#
# The code default and this operational default are both 24. Keeping them equal
# prevents direct `go run` startup from silently selecting the known-slow
# 128-depth profile. The depth remains per-environment and overridable.
#
# `make load`, n=500, one contended slot, on this laptop:
#
#   depth   409s   409 p99 (repeat runs)
#      16     15   103.5ms
#      24     23   113.2ms  85.0ms  96.6ms
#      32     31    88.3ms  91.8ms  110.2ms  130.0ms  138.6ms
#      48     47   147.7ms
#      64     63   142.9ms
#     128    127   368.7ms                            <- misses the budget outright
#
# 24 is the pick. 48 and 64 "pass" by a hair, which is not passing — the next
# run's noise takes them over. 32 passes every time but drifts as high as
# 139ms, which is inside the target and outside the ~25% margin the constant's
# own note asks for. The price of 24 over 32 is eight more students out of five
# hundred getting a fast 429 instead of a slow 409, and both of them lost.
WRITE_QUEUE_DEPTH ?= 24
export WRITE_QUEUE_DEPTH

# Use the migrate CLI if it is installed; otherwise run it straight from the
# module deps so a fresh clone needs no extra install step.
MIGRATE := $(shell command -v migrate 2>/dev/null || echo "go run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate")

N ?= 500

.PHONY: dev dev-replica verify-replica down logs migrate-up migrate-down migrate-drop seed run worker test test-race race-demo race-reset load chaos audit audit-live lint tidy psql

## dev: bring up postgres, pgbouncer, redis and wait for health
dev:
	$(COMPOSE) up -d
	@echo "waiting for services..."
	@for i in $$(seq 1 60); do \
	  if [ "$$($(COMPOSE) ps --format '{{.Health}}' postgres 2>/dev/null)" = "healthy" ] && \
	     [ "$$($(COMPOSE) ps --format '{{.Health}}' redis 2>/dev/null)" = "healthy" ]; then \
	    echo "postgres + redis healthy"; break; \
	  fi; \
	  sleep 1; \
	done
	@$(COMPOSE) ps

## dev-replica: additionally bring up the streaming standby on :5433.
##
## Opt-in. §2.1 calls the replica a nice-to-have that must not block anything,
## and DB_REPLICA_URL falls back to DB_URL — so a standby that will not come up
## costs the read/write split and nothing else.
dev-replica: dev
	$(COMPOSE) --profile replica up -d postgres-replica
	@$(COMPOSE) --profile replica ps

## verify-replica: prove the compose standby is read-only and replaying WAL
verify-replica:
	bash scripts/verify-replica.sh

## down: stop the stack (keeps the volume)
down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f

## migrate-up: apply all migrations
migrate-up:
	$(MIGRATE) -path migrations -database "$(DB_MIGRATE_URL)" up

## migrate-down: roll back one migration
migrate-down:
	$(MIGRATE) -path migrations -database "$(DB_MIGRATE_URL)" down 1

## migrate-drop: roll everything back to a clean database
migrate-drop:
	$(MIGRATE) -path migrations -database "$(DB_MIGRATE_URL)" down -all

## seed: seven facilities, one global policy, twelve users. Idempotent.
seed:
	DB_URL="$(DB_URL)" go run ./cmd/seed

## run: API on :8080
run:
	DB_URL="$(DB_URL)" DB_REPLICA_URL="$(DB_REPLICA_URL)" WRITE_QUEUE_DEPTH="$(WRITE_QUEUE_DEPTH)" DB_LISTEN_URL="$(DB_LISTEN_URL)" REDIS_URL="$(REDIS_URL)" go run ./cmd/api

## worker: outbox dispatcher + sweepers as a separate process. Not needed for
## the demo — `make run` embeds them (EMBED_WORKERS defaults to true).
worker:
	DB_URL="$(DB_URL)" DB_LISTEN_URL="$(DB_LISTEN_URL)" REDIS_URL="$(REDIS_URL)" go run ./cmd/worker

## test: unit + integration
test:
	go test ./... -short

## test-race: the concurrency suite. Slow. Do not skip.
test-race:
	go test ./test/concurrency/... -v -timeout 300s

## race-demo: fire N concurrent requests, print the outcome split
##
## Clears the slot first, so it is re-runnable back to back on stage. Override
## with RACE_ARGS: `make race-demo N=500 RACE_ARGS=-reset=false` is the
## "fire again — still 1" beat.
race-demo:
	DB_URL="$(DB_URL)" go run ./cmd/racedemo -n $(N) $(RACE_ARGS)

## race-reset: clear the demo slot without racing
race-reset:
	DB_URL="$(DB_URL)" go run ./cmd/racedemo -reset-only

## load: the 6 PM surge, as a pass/fail gate. IMPLEMENTATION.md §14, PRD §8.2.
##
## N virtual users at ONE slot, over real HTTP through the whole middleware
## chain. Exits non-zero if p99(409) >= 150ms, p99(201) >= 250ms, or anything
## returned 5xx. Needs only the compose Postgres: the API is started in-process
## on a loopback port, so there is no k6, no vegeta and no host dependency to
## install at a venue.
##
##   make load N=500
##   make load LOAD_ARGS=-json
##   make load LOAD_ARGS="-url http://localhost:8080"   # against a running API
load:
	DB_URL="$(DB_URL)" DB_REPLICA_URL="$(DB_REPLICA_URL)" WRITE_QUEUE_DEPTH="$(WRITE_QUEUE_DEPTH)" go run ./test/load -n $(N) $(LOAD_ARGS)

## chaos: break Redis, the read path, the API and the connection pool while the
## write path is running, and check the invariant survived each one.
##
## Excluded from `make test` (which runs -short) on purpose: each of these
## starts its own Postgres and Redis and fires a 200-request storm, and running
## them alongside every other package saturates a laptop Docker VM badly enough
## to knock over the latency-budgeted tests elsewhere.
chaos:
	go test ./test/chaos/... -v -count=1 -timeout 600s

## audit: the continuous-invariant suite. Safe to run at any time, including
## mid-demo, because it only reads.
##
## Self-contained by default: starts a throwaway Postgres, generates real
## contended state (a 200-way race, a capacity burst, a closure, a hold) and then
## audits it. A clean database satisfies every invariant trivially, so auditing
## one would prove nothing.
audit:
	go test ./test/invariants/... -v -count=1 -timeout 300s

## audit-live: point the same suite at the running compose database.
##
## This is the stage version — fire `make race-demo N=500`, then run this and
## show a judge that the invariants held on the database they just hammered,
## rather than on a scenario rebuilt for the occasion. Writes nothing.
audit-live:
	AUDIT_DB_URL="$(DB_URL)" go test ./test/invariants/... -v -count=1 -timeout 300s

lint:
	golangci-lint run

tidy:
	go mod tidy

psql:
	$(COMPOSE) exec postgres psql -U playhack -d playhack
