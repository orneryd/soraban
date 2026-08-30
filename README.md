# Durable 1099-NEC Filing System

A local Go/PostgreSQL implementation of the Readiness Engineer project. It
streams the two supplied firm exports, determines 2025 1099-NEC obligations,
submits durable batches to the local IRS stub, recovers ambiguous submissions,
and renders committed filing status.

## Repository layout

| Path                     | Responsibility                                           |
| ------------------------ | -------------------------------------------------------- |
| [`data/`](data/)         | Checked-in F001 and F002 generated exports               |
| [`irs/`](irs/)           | Standalone in-memory IRS HTTP/XML stub                   |
| [`postgres/`](postgres/) | Versioned schema and PostgreSQL lifecycle adapter        |
| [`service/`](service/)   | CLI, streaming importer, worker, IRS client, and web UI  |
| [`docs/`](docs/)         | Original assignment, formal specification, and decisions |

## Prerequisites

- Docker Desktop or another local Docker runtime
- Go 1.26+
- `curl`

All commands below run from the repository root unless noted.

## 1. Start PostgreSQL

Create the dedicated PostgreSQL 18.4 container once:

```sh
docker run -d \
  --name readiness-postgres \
  --restart unless-stopped \
  -e POSTGRES_USER=readiness \
  -e POSTGRES_PASSWORD=readiness-local-only \
  -e POSTGRES_DB=readiness \
  -p 127.0.0.1:55432:5432 \
  -v readiness-postgres-data:/var/lib/postgresql \
  postgres:18.4-alpine
```

On later runs:

```sh
docker start readiness-postgres
docker exec readiness-postgres pg_isready -U readiness -d readiness
```

Create the restricted application role once:

```sh
if ! docker exec readiness-postgres psql -U readiness -d postgres -Atqc \
  "SELECT 1 FROM pg_roles WHERE rolname='readiness_app'" | grep -q 1; then
  docker exec readiness-postgres psql -U readiness -d postgres -v ON_ERROR_STOP=1 \
    -c "CREATE ROLE readiness_app LOGIN PASSWORD 'readiness-app-local-only' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS"
fi
```

Apply the versioned schema:

```sh
make -C postgres migrate
```

The local application DSN is:

```sh
export DATABASE_URL='postgres://readiness_app:readiness-app-local-only@127.0.0.1:55432/readiness?sslmode=disable'
```

These credentials and `sslmode=disable` are for local testing only.

## 2. Start the IRS stub

In a separate terminal:

```sh
cd irs
go test -race ./...
go run ./cmd/irsstub
```

Verify it from the repository root:

```sh
curl --fail http://127.0.0.1:8081/healthz
```

The default bearer token is `local-irs-token`. The stub intentionally keeps
state only for its process lifetime, so leave it running during crash-recovery
scenarios.

## 3. Build the service

```sh
mkdir -p bin
go build -o bin/readiness ./service/cmd/readiness
```

The root [`go.work`](go.work) connects the `service`, `postgres`, and `irs` Go
modules for local development.

## 4. Import the supplied exports

The fixtures are portable repository assets with 500,000 records each:

```sh
./bin/readiness import \
  --firm F001 \
  --tax-year 2025 \
  --input data/firm_F001_export.csv.gz

./bin/readiness import \
  --firm F002 \
  --tax-year 2025 \
  --input data/firm_F002_export.csv.gz
```

Each command prints JSON containing `DatasetID`, row count, content hash, and
whether the import already existed. Repeating the same command is idempotent.
Use the returned dataset IDs in the next step.

## 5. Determine and plan filings

Run once per imported firm, substituting the returned dataset ID:

```sh
./bin/readiness determine --firm F001 --dataset <F001_DATASET_ID>
./bin/readiness determine --firm F002 --dataset <F002_DATASET_ID>
```

Each command returns a `DeterminationID`. Build immutable batches:

```sh
./bin/readiness plan --firm F001 --determination <F001_DETERMINATION_ID>
./bin/readiness plan --firm F002 --determination <F002_DETERMINATION_ID>
```

Replaying determine or plan returns the existing durable work.

## 6. Run filing workers

With the IRS stub still running:

```sh
./bin/readiness worker --firm F001 --firm F002
```

Stop with `Ctrl+C`. Restarting the same command resumes from PostgreSQL. The
worker runs one goroutine per firm because firms have independent rate budgets;
work within a firm remains serialized through the durable rate permit.

A full run is intentionally rate limited and can take hours for roughly 2,000
batches.

## 7. Run the web UI

In another terminal:

```sh
./bin/readiness serve --firm F001 --firm F002
```

Open <http://127.0.0.1:8080>. The UI can import, determine/plan, start or stop
local workers, navigate clients, and display committed counts and exceptions.
It does not use an in-memory filing-status cache.

## Tests

Fast tests:

```sh
go test ./irs/... ./postgres/... ./service/...
go test -race ./service/...
go vet ./irs/... ./postgres/... ./service/...
```

PostgreSQL adapter E2E:

```sh
make -C postgres test-e2e
```

Real-data service E2E creates a disposable database, imports both checked-in
500,000-row files concurrently by firm, determines, plans, and verifies replay:

```sh
make -C service test-e2e
```

Live IRS E2E uses the stub running on port 8081:

```sh
make -C service test-live
```

The required process-kill test starts its own deterministic IRS stub, creates
101 filings across two batches, kills the worker after a remote commit, then
restarts and reconciles. It takes about two minutes because it honors two
60-second conservative crash fences:

```sh
make -C service test-crash
```

## Performance

The latest fresh-database run processed both firms concurrently:

| Firm |    Rows | Import | Determination | Batch planning |
| ---- | ------: | -----: | ------------: | -------------: |
| F001 | 500,000 | 11.86s |         3.65s |          4.24s |
| F002 | 500,000 | 12.03s |         3.61s |          4.17s |

The complete import, determination, planning, and replay E2E took 22.76 seconds.
See [`service/PERFORMANCE.md`](service/PERFORMANCE.md) for implementation notes.
A real two-firm status page returned in 0.038 seconds during local validation.

The import path is a bounded pull pipeline:

```text
gzip reader -> decompressed SHA-256 -> encoding/csv -> PaymentRow -> COPY staging
```

No slice contains the full export. Determination is one set-based aggregate and
insert per firm. Planning bulk-loads batch/link staging rows with COPY and then
uses set-based inserts and transitions.

## Current completeness

The application is operational end to end and the most important failure case is
proven: an after-record IRS error followed by process death recovers by stable
UTID without duplicate filings, and a second batch still completes.

The following assignment acceptance evidence is implemented and passing:

- both supplied files import and replay with 500,000 rows each;
- all six determination scenarios and payment explanations;
- tenant-scoped RLS reads/writes;
- deterministic batches of at most 100 filings;
- concurrent claims and stable ambiguous-submit reconciliation;
- shared per-firm durable rate serialization;
- live IRS submit, lookup, delayed acknowledgment, and result persistence;
- process-level crash recovery with 101 filings and two batches; and
- truthful firm/client status, grouped exceptions, and a light web UI.

The repository should **not yet claim every strict acceptance item is complete**.
These proofs remain to be added:

1. process-level importer kills specifically during COPY and merge;
2. measured 1x/4x peak-RSS comparison for flat-memory acceptance;
3. complete table checksums before and after import replay, beyond current
   identity/hash/count assertions;
4. cross-firm update and foreign-key-link attempts in addition to current
   cross-firm read/insert tests;
5. the full 599.99/600.00/600.01 and net-zero/negative determination matrix;
6. an asserted two-firm rolling-window rate test covering mixed submit/status,
   retries, and restarts, rather than only serialization/spacing evidence;
7. exhaustive status-precedence combinations before and after restart;
8. a web route for payment explanation (the lifecycle query exists, but the UI
   does not yet expose it);
9. automated 1x/4x RSS and 100-request status-p95 tasks/artifacts; and
10. automated dependency-license policy checking.

Repository sharing and the five-minute video remain submission steps outside the
codebase.

## Local reset

To erase only the dedicated local application schema and reapply migrations:

```sh
docker exec readiness-postgres psql -U readiness -d readiness -v ON_ERROR_STOP=1 \
  -c 'DROP SCHEMA public CASCADE' \
  -c 'CREATE SCHEMA public AUTHORIZATION readiness'
make -C postgres migrate
```

This is destructive. Do not run it against any database other than the dedicated
local `readiness` database.
