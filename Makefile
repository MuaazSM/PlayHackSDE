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
REDIS_URL      ?= redis://localhost:$(REDIS_HOST_PORT)

# Use the migrate CLI if it is installed; otherwise run it straight from the
# module deps so a fresh clone needs no extra install step.
MIGRATE := $(shell command -v migrate 2>/dev/null || echo "go run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate")

N ?= 500

.PHONY: dev down logs migrate-up migrate-down migrate-drop seed run worker test test-race race-demo race-reset lint tidy psql

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
	DB_URL="$(DB_URL)" REDIS_URL="$(REDIS_URL)" go run ./cmd/api

worker:
	DB_URL="$(DB_URL)" REDIS_URL="$(REDIS_URL)" go run ./cmd/worker

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

lint:
	golangci-lint run

tidy:
	go mod tidy

psql:
	$(COMPOSE) exec postgres psql -U playhack -d playhack
