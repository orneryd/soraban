#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
database_name="readiness_service_e2e_$$"
runtime_url="postgres://readiness_app:readiness-app-local-only@127.0.0.1:55432/${database_name}?sslmode=disable"

cleanup() {
    docker exec readiness-postgres psql -U readiness -d postgres -v ON_ERROR_STOP=1 \
        -c "DROP DATABASE IF EXISTS ${database_name} WITH (FORCE)" >/dev/null
}
trap cleanup EXIT INT TERM

docker exec readiness-postgres psql -U readiness -d postgres -v ON_ERROR_STOP=1 \
    -c "CREATE DATABASE ${database_name} OWNER readiness" >/dev/null
docker exec -i readiness-postgres psql -U readiness -d "$database_name" -v ON_ERROR_STOP=1 \
    < "$repo_root/postgres/db/migrations/000001_initial.sql" >/dev/null

cd "$repo_root/service"
RUN_SERVICE_E2E=1 DATABASE_URL="$runtime_url" \
    go test ./internal/app -run '^TestRealDataImportDetermineAndReplayE2E$' -v
