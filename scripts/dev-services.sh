#!/usr/bin/env bash
# Starts/stops local Postgres (with pgvector) and Redis from the project venv.
# Usage: scripts/dev-services.sh {start|stop|status}
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck disable=SC1091
source "$ROOT/.venv/bin/activate"
DATA="$ROOT/.data"
PGDATA="$DATA/postgres"
PGPORT="${PGPORT:-5432}"
REDIS_PORT="${REDIS_PORT:-6379}"
mkdir -p "$DATA"

start() {
  if [ ! -f "$PGDATA/PG_VERSION" ]; then
    initdb -D "$PGDATA" -U llmgw --auth=trust -E UTF8 >/dev/null
  fi
  if ! pg_ctl -D "$PGDATA" status >/dev/null 2>&1; then
    pg_ctl -D "$PGDATA" -o "-p $PGPORT -k $DATA -c max_connections=200 -c shared_buffers=256MB" -l "$DATA/postgres.log" -w start >/dev/null
  fi
  psql -h localhost -p "$PGPORT" -U llmgw -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='llmgateway'" | grep -q 1 \
    || createdb -h localhost -p "$PGPORT" -U llmgw llmgateway
  if ! redis-cli -p "$REDIS_PORT" ping >/dev/null 2>&1; then
    redis-server --port "$REDIS_PORT" --daemonize yes --save "" --appendonly no \
      --dir "$DATA" --logfile "$DATA/redis.log" --pidfile "$DATA/redis.pid"
    for _ in $(seq 1 50); do redis-cli -p "$REDIS_PORT" ping >/dev/null 2>&1 && break; sleep 0.1; done
  fi
  echo "postgres: postgres://llmgw@localhost:$PGPORT/llmgateway  redis: localhost:$REDIS_PORT"
}

stop() {
  pg_ctl -D "$PGDATA" -m fast stop >/dev/null 2>&1 || true
  redis-cli -p "$REDIS_PORT" shutdown nosave >/dev/null 2>&1 || true
  echo "stopped"
}

status() {
  pg_ctl -D "$PGDATA" status || true
  redis-cli -p "$REDIS_PORT" ping || true
}

case "${1:-start}" in
  start) start ;;
  stop) stop ;;
  status) status ;;
  *) echo "usage: $0 {start|stop|status}" >&2; exit 2 ;;
esac
