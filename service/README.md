# Application Service: Formal Specification

Status: Draft for implementation
Date: 2026-08-30
Target: Go standard library plus the sibling `postgres` lifecycle module
Companions: [project requirements](../docs/README.md),
[system specification](../docs/formal-draft-spec.md),
[PostgreSQL lifecycle contract](../postgres/README.md), and
[IRS stub contract](../irs/README.md)

## 1. Scope

The `service` module is the executable application and orchestration layer. It:

- starts application commands and composes adapters;
- discovers, opens, decompresses, and parses source exports;
- adapts CSV records to PostgreSQL lifecycle DTOs;
- coordinates import, determination, and batch planning;
- claims durable filing work and performs the required IRS HTTP call;
- maps IRS outcomes back to exactly one lifecycle transition;
- serves status and exception views from lifecycle queries; and
- manages cancellation and graceful shutdown.

The service owns no durable business state. It MUST NOT issue SQL, import `pgx`,
name database tables, manage database transactions or locks, set RLS context, or
interpret PostgreSQL error codes. It MUST NOT import the IRS stub implementation.

The words MUST, MUST NOT, SHOULD, SHOULD NOT, and MAY are normative.

## 2. Required module boundary

The sibling `postgres` module must first expose its lifecycle contract and
constructor from public packages. Go prohibits `service` from importing
`postgres/internal/app` or `postgres/internal/store`.

The exported surface SHOULD be:

```text
postgres/lifecycle   persistence-neutral interfaces, DTOs, and sentinel errors
postgres/store       Open, Store, and PostgreSQL implementation
```

`BatchWork` MUST include the durable attempt number in addition to the fields in
the PostgreSQL lifecycle specification. The service uses that number to compute
retry delay without process-memory retry state.

The service imports only those exported packages. The composition root is the
only service code that imports `postgres/store`; all other service packages
depend on narrow lifecycle interfaces.

The repository root SHOULD provide a `go.work` file using `./service`,
`./postgres`, and `./irs` so local builds resolve sibling modules without a
published version. The service MUST add no third-party runtime dependency of its
own.

## 3. Package layout

```text
service/
  cmd/readiness/       executable and command dispatch
  internal/config/     flags and environment validation
  internal/importer/   discovery, gzip/CSV reader, PaymentRowSource adapter
  internal/app/        import, determination, and planning orchestration
  internal/filing/     worker loop, retry policy, IRS outcome mapping
  internal/irsclient/  typed HTTP/XML client
  internal/web/        handlers, templates, and project-owned CSS
```

No generic repository, dependency-injection framework, job framework, or web
framework is required.

## 4. Configuration and startup

### 4.1 Commands

One binary, `readiness`, MUST provide:

```text
readiness import --firm F001 --tax-year 2025 --input <file-or-directory>
readiness determine --firm F001 --dataset <id>
readiness plan --firm F001 --determination <id>
readiness worker --firm F001 [--firm F002]
readiness serve --firm F001 [--firm F002]
```

Each command validates only its own required settings. Unknown flags, missing
values, duplicate firm IDs, unsupported tax years, and invalid durations fail
before adapters start.

### 4.2 Environment

| Variable                       | Default                                            | Used by           |
| ------------------------------ | -------------------------------------------------- | ----------------- |
| `DATABASE_URL`                 | local restricted-role DSN from the PostgreSQL spec | all commands      |
| `IRS_BASE_URL`                 | `http://127.0.0.1:8081`                            | `worker`          |
| `IRS_BEARER_TOKEN`             | `local-irs-token`                                  | `worker`          |
| `HTTP_ADDR`                    | `127.0.0.1:8080`                                   | `serve`           |
| `WORKER_IDLE_DELAY`            | `1s`                                               | `worker`          |
| `WORKER_LEASE_DURATION`        | `90s`                                              | `worker`          |
| `HTTP_CONNECT_TIMEOUT`         | `3s`                                               | `worker`          |
| `HTTP_RESPONSE_HEADER_TIMEOUT` | `5s`                                               | `worker`          |
| `HTTP_TOTAL_TIMEOUT`           | `10s`                                              | `worker`          |
| `SHUTDOWN_TIMEOUT`             | `15s`                                              | `worker`, `serve` |

The lease duration MUST exceed the 60-second conservative rate-fence wait plus
the total HTTP timeout and a bounded persistence margin. Secrets MUST come from environment or an injected configuration source,
not flags, URLs, logs, or committed files.

### 4.3 Composition

Startup MUST:

1. parse and validate configuration;
2. configure `slog` without source data or secrets;
3. open the PostgreSQL lifecycle adapter;
4. fail before serving or claiming work on connectivity or schema mismatch;
5. construct the IRS client only for commands that require it; and
6. start only the requested command.

The service MUST NOT apply database migrations automatically.

### 4.4 Local commands

From `service/`, with the documented PostgreSQL container running:

```sh
make build
make test
make test-race
make test-e2e
make test-live
make test-crash
```

`make test-e2e` creates and removes a disposable database while consuming both
checked-in exports. `make test-live` uses the running IRS stub at
`http://127.0.0.1:8081`. `make test-crash` starts a dedicated deterministic
after-record stub and takes about two minutes because it honors two conservative
60-second crash fences.

## 5. Import adapter

### 5.1 Discovery

The operator always supplies firm ID and tax year. Source rows do not identify a
firm and the service MUST NOT infer one from payment data.

When `--input` is a file, it MUST end in `.csv.gz`. When it is a directory, the
service discovers regular `.csv.gz` files in that directory, sorts them by name,
and requires exactly one match for the selected firm/year. Zero or multiple
matches are an operator-visible error; revised exports are out of scope.
Symlinks MUST NOT be followed implicitly.

### 5.2 Supplied real-data fixtures

Both required generated exports are checked into the repository under `data/`:

| Firm   | Repository-relative fixture path | Compressed SHA-256                                                 |
| ------ | -------------------------------- | ------------------------------------------------------------------ |
| `F001` | `data/firm_F001_export.csv.gz`   | `229ac7223dc4e316e6fed52644b793899c0f0c34b33084453e364e68ec1ea29c` |
| `F002` | `data/firm_F002_export.csv.gz`   | `09668327e446544c1e0fe1430dbe859838bfbb64ef0381c57f17ffd2d06f7e71` |

The mapping is explicit: the F001 path is imported with `--firm F001`, and the
F002 path with `--firm F002`. Tests MUST NOT infer firm identity from the
filename or CSV contents.

E2E and performance targets MUST resolve these paths from the repository root,
not the process working directory or an absolute machine path. They MUST fail
with a useful prerequisite error when either checked-in fixture is absent or its
compressed checksum differs; they MUST NOT generate replacement business data
silently. Dataset idempotency continues to use the required decompressed-stream
hash.

The final CLI must support these direct runs:

```sh
readiness import --firm F001 --tax-year 2025 \
  --input data/firm_F001_export.csv.gz
readiness import --firm F002 --tax-year 2025 \
  --input data/firm_F002_export.csv.gz
```

### 5.3 Streaming reader

For one source file, the importer MUST:

1. open the file and gzip reader;
2. tee the decompressed byte stream into SHA-256;
3. use `encoding/csv` incrementally;
4. require the exact UTF-8 header in this order:
   `client_id,vendor_name,vendor_tin,payment_date,amount,payment_method,backup_withholding,memo`;
5. reject invalid UTF-8, malformed quoting, or a record with other than eight
   fields;
6. assign one-based source record numbers after the header;
7. expose each record as a lifecycle `PaymentRow` containing the original field
   strings; and
8. expose the decompressed-stream digest only after EOF.

The adapter MUST keep at most one bounded record plus parser buffers in memory.
It MUST enforce a documented maximum record size and return the source record
number for every record-level error. It MUST reject an empty export.

Transport validation belongs here. Money, date, method, TIN, and withholding
business validation remains in the PostgreSQL lifecycle import so it is enforced
inside the atomic database transaction.

### 5.4 Import orchestration

The import command passes the stream directly to `ImportDataset`. It MUST NOT
materialize all rows. A matching replay is success and reports `Existing=true`.
`ErrConflict` is reported as `DATASET_CONFLICT`. Cancellation closes the file and
gzip reader and allows the database operation to roll back.

## 6. Determination and planning

The service exposes separate `determine` and `plan` commands. A convenience web
action MAY invoke both in order, but neither operation is hidden inside import.

- `determine` calls `DetermineDataset` with `nec-2025-v1` and reports durable ID,
  ready count, blocked count, and replay status.
- `plan` calls `PlanBatches` and reports existing and created batch counts.
- `ErrNotFound`, `ErrConflict`, and `ErrInvariant` are operator-visible failures.

The service does not aggregate payments, classify methods, generate filing keys,
partition batches, construct UTIDs, or serialize canonical filing XML. Those are
durable PostgreSQL lifecycle responsibilities.

## 7. IRS client

The client uses `net/http`, `encoding/xml`, and `mime/multipart`. The normative
paths and XML profiles are defined by [`irs/openapi.yaml`](../irs/openapi.yaml)
and [`irs/README.md`](../irs/README.md).

The client MUST:

- use finite connect, response-header, and total request timeouts;
- require the configured bearer token and `Accept: application/xml`;
- submit exactly the canonical XML bytes supplied by `BatchWork` as the single
  multipart `file` part;
- support status lookup by UTID or ReceiptId;
- parse success and error XML with unknown fields rejected;
- limit response bodies before decoding;
- never log request or response bodies, bearer tokens, raw UTIDs, names, TINs,
  or memo data; and
- return typed transport, HTTP, and protocol outcomes rather than lifecycle
  transitions.

The client MUST NOT generate or modify UTIDs, filing keys, ReceiptIds, canonical
XML, or filing outcomes.

## 8. Filing worker

### 8.1 Loop

The worker enumerates configured firms fairly. For each firm it calls
`ClaimNextBatch`. If no work is due, it waits for `WORKER_IDLE_DELAY` or context
cancellation. It MUST NOT maintain an in-memory work queue.

Before submit, the worker recomputes SHA-256 over `BatchWork.CanonicalXML` and
compares it with `BatchWork.PayloadSHA256`. A mismatch calls
`FailBatchInvariant` and performs no HTTP request.

For each IRS request, the worker MUST:

1. acquire one `CallPermit` for the firm, batch, and operation;
2. defer permit closure;
3. perform exactly one HTTP request;
4. call `Finish` once with the observed call outcome; and
5. call exactly one lifecycle transition for the IRS outcome.

The service never persists state directly. A stale claim is abandoned; the next
worker claim recovers from durable state.

### 8.2 Action mapping

| `BatchWork.NextAction` | IRS operation                                                           | Allowed lifecycle result                                                                                    |
| ---------------------- | ----------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `submit`               | Intake using stable canonical XML                                       | `RecordSubmitAccepted`, `RecordSubmitUnknown`, or `FailBatchInvariant`                                      |
| `lookup_by_utid`       | Status by stable UTID                                                   | `RecordReferenceFound`, `RecordReferenceNotFound`, `RecordStatusUnavailable`, or `FailBatchInvariant`       |
| `poll_status`          | Status by ReceiptId, falling back to UTID only when ReceiptId is absent | `RecordAcknowledgmentPending`, `CompleteAcknowledgment`, `RecordStatusUnavailable`, or `FailBatchInvariant` |

Unknown actions are invariant failures and cause no HTTP call.

### 8.3 Outcome mapping

| Context        | Observed result                                                                        | Permit outcome    | Lifecycle call                |
| -------------- | -------------------------------------------------------------------------------------- | ----------------- | ----------------------------- |
| Submit         | `200` with one ReceiptId                                                               | `completed`       | `RecordSubmitAccepted`        |
| Submit         | transport error, `5xx`, or `429`                                                       | `ambiguous`       | `RecordSubmitUnknown`         |
| Submit         | `409 DUPLICATE_UTID`                                                                   | `completed`       | `RecordSubmitUnknown`         |
| Submit         | `400`, `401`, `415`, `409 DUPLICATE_RECORD`, malformed response                        | `terminal_error`  | `FailBatchInvariant`          |
| UTID lookup    | `200` found                                                                            | `completed`       | `RecordReferenceFound`        |
| UTID lookup    | `404 NOT_FOUND`                                                                        | `completed`       | `RecordReferenceNotFound`     |
| Lookup or poll | transport error, `5xx`, or `429`                                                       | `retryable_error` | `RecordStatusUnavailable`     |
| Poll           | `200 Processing`                                                                       | `completed`       | `RecordAcknowledgmentPending` |
| Poll           | completed per-filing results                                                           | `completed`       | `CompleteAcknowledgment`      |
| Lookup or poll | authentication, invalid request, identity mismatch, malformed or incomplete result set | `terminal_error`  | `FailBatchInvariant`          |

A submit ambiguity always becomes UTID lookup. It never schedules another submit
directly. A stale pending acknowledgment remains pollable and is never
resubmitted.

### 8.4 Retry policy

Retry delay is exponential with bounded jitter and a cap:

```text
base = min(initial_delay * 2^(attempt_number-1), maximum_delay)
delay = base + deterministic_testable_jitter
```

The worker derives `attempt_number` from `BatchWork`, not process memory. The
clock and jitter source are interfaces in tests. Exhaustion calls
`RecordRetryExhausted`; it never silently drops work.

## 9. Status and web

Every page request queries the lifecycle API. Process memory may track HTTP
request execution but MUST NOT be a source of filing status.

Required views:

- firm client list with headline and required, blocked, ready, pending, accepted,
  and rejected counts;
- client detail with the same counts;
- grouped exceptions with exact reason;
- payment explanation showing counted/excluded payments and aggregate totals.

Required actions:

- import a selected firm export;
- determine and plan a selected dataset; and
- start or stop the local worker for configured firms.

Actions use the same application functions as CLI commands. Handlers validate
all path and form values and use POST for mutations. Long-running actions MAY
run asynchronously for the local demo, but their in-memory progress MUST NOT be
rendered as durable filing status. A crash leaves idempotent actions safe to
invoke again.

The UI uses `net/http`, `html/template`, and project-owned CSS. Authentication,
accounts, notifications, and client approval remain out of scope.

## 10. Shutdown and security

The root context is canceled by `SIGINT` or `SIGTERM`. Shutdown MUST:

1. stop HTTP acceptance and new batch claims;
2. cancel idle worker waits and IRS requests;
3. allow in-flight lifecycle persistence up to `SHUTDOWN_TIMEOUT`; and
4. close the PostgreSQL adapter after workers and HTTP handlers stop.

Correctness MUST also survive `SIGKILL`; leases, idempotent identities, and UTID
reconciliation provide recovery.

Logs may include request ID, operation, firm ID, client ID, batch ID, state,
counts, duration, HTTP status, and sanitized error code. Logs MUST NOT include
DSNs, credentials, raw UTIDs, canonical XML, TINs, names, memos, or source rows.

## 11. Tests and acceptance

### 11.1 Unit and adapter tests

| Test                      | Required proof                                                                                                                                      |
| ------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TEST-SVC-STARTUP-01`     | Each command validates only required configuration, composes adapters, and fails on lifecycle open/schema errors.                                   |
| `TEST-SVC-IMPORT-01`      | Discovery, gzip errors, exact header, UTF-8, CSV quoting/newlines, row numbers, bounded record size, cancellation, and decompressed hash are exact. |
| `TEST-SVC-ORCHESTRATE-01` | Fake lifecycle proves import, determination, and planning order, replay handling, and error mapping without PostgreSQL.                             |
| `TEST-SVC-IRSCLIENT-01`   | `httptest.Server` verifies paths, headers, multipart bytes, lookup XML, response limits, and strict decoding.                                       |
| `TEST-SVC-WORKER-01`      | Fake lifecycle plus recording transport maps every action/outcome row in Section 8 and performs at most one HTTP call per permit.                   |
| `TEST-SVC-RATE-01`        | Worker always finishes/closes permits on success, transport failure, decode failure, cancellation, and panic-safe cleanup paths.                    |
| `TEST-SVC-STATUS-01`      | HTTP handlers render only lifecycle query results and preserve headline precedence and counts.                                                      |
| `TEST-SVC-SHUTDOWN-01`    | Shutdown stops claims, cancels waits/HTTP, permits bounded persistence, and exits within its deadline.                                              |

Unit tests MUST use fakes and `httptest`; they MUST NOT require PostgreSQL or the
IRS stub. Fuzz tests cover CSV quoting/newlines, XML decoding, and outcome mapping.

### 11.2 Cross-module E2E

E2E tests use the restricted PostgreSQL role and the IRS stub only through HTTP.
They MUST include:

- import `data/firm_F001_export.csv.gz` and
  `data/firm_F002_export.csv.gz` through the real gzip/CSV adapter into
  PostgreSQL, asserting their exact firm mapping and resulting source row
  counts;
- replay each supplied file and prove dataset/payment counts and values are
  unchanged;
- determination, batch planning, worker submission, delayed acknowledgment, and
  final status through service entry points;
- default live-stub operation at `http://127.0.0.1:8081`;
- process-level worker restart with the stub kept alive;
- deterministic after-record failure followed by `SIGKILL`, UTID reconciliation,
  and exactly one remote filing per expected key;
- more than 100 filings for one client so a second batch also completes; and
- durable call-log verification of at most 20 calls in every rolling 60 seconds
  per firm.

Performance tests use both supplied fixture paths and record machine details,
per-file import wall time, combined determination wall time, status p95, and
1x/4x peak RSS. Each full firm import MUST complete in under 120 seconds and the
combined determination in under 60 seconds on the documented machine.

## 12. Definition of done

The service layer is complete when:

1. the PostgreSQL lifecycle API is importable from the sibling service module;
2. all commands and package boundaries above are implemented;
3. unit tests pass without PostgreSQL or the IRS stub;
4. cross-module live and process-crash E2E tests pass repeatedly;
5. import, determination, status, and memory budgets pass on supplied data;
6. the light web UI can start actions and render truthful committed status; and
7. setup, command examples, state mapping, timing output, and known limitations
   are documented.
