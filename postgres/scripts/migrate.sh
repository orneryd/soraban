#!/bin/sh
set -eu

database_url=${MIGRATION_DATABASE_URL:-postgres://readiness:readiness-local-only@127.0.0.1:55432/readiness?sslmode=disable}
default_database_url=postgres://readiness:readiness-local-only@127.0.0.1:55432/readiness?sslmode=disable

run_psql() {
    if command -v psql >/dev/null 2>&1; then
        psql "$database_url" "$@"
        return
    fi
    if [ "$database_url" != "$default_database_url" ]; then
        echo "psql is required for a non-default MIGRATION_DATABASE_URL" >&2
        exit 1
    fi
    docker exec -i readiness-postgres psql -U readiness -d readiness "$@"
}

for migration in db/migrations/*.sql; do
    version=$(basename "$migration" | cut -d_ -f1 | sed 's/^0*//')
    if run_psql -v ON_ERROR_STOP=1 -Atqc "SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'schema_migrations'" | grep -q 1 &&
       run_psql -v ON_ERROR_STOP=1 -Atqc "SELECT 1 FROM schema_migrations WHERE version = $version" | grep -q 1; then
        continue
    fi
    run_psql -v ON_ERROR_STOP=1 < "$migration"
done