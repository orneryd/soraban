MIGRATION_DATABASE_URL ?= postgres://readiness:readiness-local-only@127.0.0.1:55432/readiness?sslmode=disable
DATABASE_URL ?= postgres://readiness_app:readiness-app-local-only@127.0.0.1:55432/readiness?sslmode=disable

.PHONY: migrate migrate-docker test test-race test-e2e test-live

migrate:
	MIGRATION_DATABASE_URL='$(MIGRATION_DATABASE_URL)' ./scripts/migrate.sh

migrate-docker:
	docker exec -i readiness-postgres psql -U readiness -d readiness -v ON_ERROR_STOP=1 < db/migrations/000001_initial.sql

test:
	go test ./...

test-race:
	go test -race ./...

test-e2e: migrate
	RUN_POSTGRES_E2E=1 DATABASE_URL='$(DATABASE_URL)' MIGRATION_DATABASE_URL='$(MIGRATION_DATABASE_URL)' go test ./internal/store

test-live: migrate
	RUN_LIVE_E2E=1 DATABASE_URL='$(DATABASE_URL)' MIGRATION_DATABASE_URL='$(MIGRATION_DATABASE_URL)' go test ./internal/store -run TestLiveLifecycle -v