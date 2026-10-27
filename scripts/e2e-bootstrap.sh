#!/usr/bin/env bash
# Shared bootstrap for the self-contained e2e tasks (e2e-oidc, e2e-full).
#
# This file is sourced, not executed: it provides e2e_provision_db and
# e2e_wait_http so the throwaway-database recreate, the Atlas migrate, and
# the readiness waits live in one place. Callers run under
# `set -euo pipefail`, which the helpers rely on for failure propagation.
# Requires psql, docker, and curl.

# e2e_provision_db drops and recreates the throwaway database $1, then
# applies the Atlas migrations, so a previous run's rows never leak into
# the checks.
e2e_provision_db() {
  local db="$1"
  export PGPASSWORD=gorm
  psql -h localhost -p 9921 -U gorm -d postgres -c "DROP DATABASE IF EXISTS ${db} WITH (FORCE)" >/dev/null
  psql -h localhost -p 9921 -U gorm -d postgres -c "CREATE DATABASE ${db}" >/dev/null
  docker run --rm --network host -v "$PWD/migrations:/migrations:ro" arigaio/atlas:1.3.0@sha256:aa51fd9b86d27a4dde60e51e4ceeaf5b8080b5b37b94c6c591cd9f9526a32dc1 \
    migrate apply \
    --url "postgres://gorm:gorm@localhost:9921/${db}?sslmode=disable" \
    --dir "file:///migrations/versions" >/dev/null
}

# e2e_wait_http polls $1 until it answers 2xx: $2 tries, $3 seconds apart.
e2e_wait_http() {
  local url="$1" tries="$2" sleep_s="$3"
  for _ in $(seq 1 "$tries"); do
    curl -fsS "$url" >/dev/null 2>&1 && break
    sleep "$sleep_s"
  done
}
