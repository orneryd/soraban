DATABASE_URL ?= postgres://readiness_app:readiness-app-local-only@127.0.0.1:55432/readiness?sslmode=disable

.PHONY: test test-race test-e2e test-live test-crash build

build:
	go build ./cmd/readiness

test:
	go test ./...

test-race:
	go test -race ./...

test-e2e:
	./scripts/test-e2e.sh

test-live:
	RUN_LIVE_SERVICE_E2E=1 DATABASE_URL='$(DATABASE_URL)' go test ./internal/filing -run TestLiveServiceLifecycleE2E -v

test-crash:
	RUN_CRASH_SERVICE_E2E=1 DATABASE_URL='$(DATABASE_URL)' go test -count=1 ./internal/filing -run TestProcessCrashAfterRemoteCommitE2E -v
