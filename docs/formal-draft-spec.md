# Durable 1099-NEC Filing System: Formal Draft Specification

Status: Draft for implementation review
Date: 2026-08-30
Target: Go and PostgreSQL 18
Tax year: 2025
Filing deadline: 2026-02-02

## 1. Normative language

The words MUST, MUST NOT, SHOULD, SHOULD NOT, and MAY are normative. A behavior
is not complete until its listed acceptance tests pass.

## 2. Scope

The system imports the two supplied firm exports, determines original 2025
1099-NEC obligations, submits valid filings to the standalone IRS stub running
at `http://127.0.0.1:8081`, recovers from all specified application-process
failures while that stub remains running, and presents status derived from
durable application facts and reconciled stub responses.

The system does not implement corrections, revised exports, authentication,
account management, notifications, deployment, or client approval.

## 3. Correctness claim and boundary

### 3.1 Claim

For each original filing identity, the running IRS stub MUST contain at most one
filing record, regardless of application-process crashes, retries, ambiguous
submission responses, concurrent workers, or replay of a filing run.

The supplied stub is intentionally in-memory. Its state survives application
worker crashes but not a stub-process restart. Therefore this project does not
claim end-to-end recovery across a stub restart, host restart, or simultaneous
loss of local application and remote test state. E2E crash tests MUST keep the
same stub process alive for the full scenario.

### 3.2 Necessary remote contract

Exactly-once external effects cannot be guaranteed by the application database
alone. A process can crash after the remote service records a transmission and
before the local database records the response. Therefore, the zero-duplicate
claim within one live-stub lifetime depends on these receiver-enforced rules:

1. `UniqueTransmissionId` (UTID) is the stable transmission reference, not a
   per-HTTP-call ID.
2. Reusing a recorded UTID returns `409 DUPLICATE_UTID` and creates nothing.
3. A filing key is sent as `RecordId`; reusing it for the same firm, client, and
   tax year under another UTID returns `409 DUPLICATE_RECORD` and creates
   nothing.
4. Lookup by UTID is immediately authoritative for submission existence, even
   while acknowledgment remains `Processing`.
5. A found UTID returns the original opaque `ReceiptId` and frozen results.

The supplied stub implements these rules under one process mutex, not with a
database. It does not compare payload hashes when an existing UTID is reused;
the application MUST enforce byte-stable payload reuse locally. If a real IRS
API does not provide equivalent durable receiver-side uniqueness, the absolute
zero-duplicate requirement is impossible. The only available choices would then
be at-most-once with possible omission or at-least-once with possible
duplication.

## 4. System invariants

| ID     | Invariant                                                                                                                            |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------ |
| INV-01 | One successful immutable dataset exists per `(firm_id, tax_year)`.                                                                   |
| INV-02 | One payment exists per `(dataset_id, source_row_number)`.                                                                            |
| INV-03 | Every tenant-owned row carries `firm_id`; composite foreign keys cannot cross firms.                                                 |
| INV-04 | Money is represented as signed integer cents. Floating point is forbidden.                                                           |
| INV-05 | One original 1099-NEC filing exists per `(firm_id, client_id, tax_year, vendor_identity)`.                                           |
| INV-06 | `filing_key` is deterministic from INV-05 plus fixed schema, form, and revision constants and is globally unique.                    |
| INV-07 | A filing belongs to zero or one immutable batch.                                                                                     |
| INV-08 | A batch contains 1 through 100 filings, all from one firm and one client.                                                            |
| INV-09 | A batch UTID and canonical XML payload hash never change after creation.                                                             |
| INV-10 | Every submit retry for a batch uses the same UTID and byte-equivalent canonical XML payload.                                         |
| INV-11 | An ambiguous submit outcome can transition only to UTID lookup, never directly to a new submission.                                  |
| INV-12 | Accepted and rejected acknowledgment results are immutable.                                                                          |
| INV-13 | Rejected filings are never automatically resubmitted. Corrections are out of scope.                                                  |
| INV-14 | During one process lifetime, the stub atomically enforces unique `(firm_id, UTID)` and original `RecordId` per firm/client/tax-year. |
| INV-15 | All IRS calls for one firm share one durable rate gate.                                                                              |
| INV-16 | Status is computed from committed source rows and never from in-memory worker state.                                                 |
| INV-17 | A worker lease grants permission to work, not permission to duplicate an external effect.                                            |

## 5. Architecture

Use one small Go application codebase and one PostgreSQL application database.
The prebuilt `irs/` service runs as a separately addressed Go process at
`http://127.0.0.1:8081` and owns in-memory state. The application MUST interact
with it only over HTTP and MUST NOT import stub packages or share memory or a
transaction with it.

```text
CSV.GZ -> importer -> PostgreSQL application database
                         |             |
                         |             +-> server-rendered status UI
                         v
                    filing worker -> HTTP/XML -> live IRS stub (in memory)
```

PostgreSQL is also the work queue. `submission_batches` is the transactional
outbox and state machine; no Redis, Kafka, generic job framework, or workflow
engine is required.

### 5.1 Go package boundaries

```text
cmd/readiness       CLI: serve, worker, import, determine
internal/domain     money, ruleset, filing identity, state transitions
internal/app        orchestration, data lifecycle interfaces, API DTOs
internal/importer   gzip/CSV streaming
internal/store      data lifecycle implementation, PostgreSQL queries and transactions
internal/filing     batch planner, submit/reconcile worker, rate gate
internal/irsclient  typed HTTP adapter
internal/status     truthful status projection
internal/web        net/http handlers and html/template views
db/migrations       forward-only numbered SQL applied separately by psql
```

The standalone stub is already implemented under `irs/`; it is a test
dependency rather than an application package. Its
[`README.md`](../irs/README.md) and [`openapi.yaml`](../irs/openapi.yaml) are the
authoritative remote contract.

The domain package MUST NOT import HTTP or PostgreSQL packages. Interfaces MUST
be introduced only where a use case needs to replace an adapter in a test. A
generic repository layer is forbidden.

Only `internal/store` may import `pgx` or contain application SQL. It implements
the data lifecycle interfaces owned by `internal/app`; domain and orchestration
code do not import the concrete store. Core packages must not receive
transactions, rows, SQL strings, or database error codes.
Schema migrations are an operator action and MUST NOT run automatically during
application startup.

## 6. Technology decision

The runtime stack MUST be:

- Go standard library for HTTP, templates, CSV, gzip, hashing, logging, and tests.
- `github.com/jackc/pgx/v5` as the only required application library.
- PostgreSQL 18, pinned to a stable minor release in the local environment.
- Plain numbered SQL migrations applied by `psql`.
- Server-rendered HTML with small project-owned CSS; no JavaScript framework.

Durable tables MUST be ordinary logged PostgreSQL tables. `fsync`,
`full_page_writes`, and `synchronous_commit` MUST remain enabled. Development
configuration MUST NOT trade correctness for benchmark results.

### 6.1 Local PostgreSQL test instance

Local integration and E2E tests MAY use the dedicated Docker instance currently
running with these settings:

| Setting           | Value                                                      |
| ----------------- | ---------------------------------------------------------- |
| Container         | `readiness-postgres`                                       |
| Image             | `postgres:18.4-alpine`                                     |
| Address           | `127.0.0.1:55432`                                          |
| Database          | `readiness`                                                |
| Persistent volume | `readiness-postgres-data` mounted at `/var/lib/postgresql` |
| Migration owner   | `readiness` / `readiness-local-only`                       |
| Application role  | `readiness_app` / `readiness-app-local-only`               |

These credentials are local test values only and MUST NOT be reused outside the
developer machine. Use the owner connection only to apply migrations:

```sh
export MIGRATION_DATABASE_URL='postgres://readiness:readiness-local-only@127.0.0.1:55432/readiness?sslmode=disable'
```

Application processes and tenant-isolation tests MUST use the non-owner role,
which is neither a superuser nor a `BYPASSRLS` role:

```sh
export DATABASE_URL='postgres://readiness_app:readiness-app-local-only@127.0.0.1:55432/readiness?sslmode=disable'
```

The container has an `unless-stopped` restart policy. Manage and verify it with:

```sh
docker start readiness-postgres
docker stop readiness-postgres
docker exec readiness-postgres pg_isready -U readiness -d readiness
docker exec -it readiness-postgres psql -U readiness -d readiness
```

The persistent volume survives container stops and removal. Destructive tests
MUST reset only the `readiness` database schema and MUST NOT remove the shared
Docker volume implicitly. The instance binds only to localhost and uses
`sslmode=disable`; it is suitable for local testing, not deployment.

## 7. Data interpretation

### 7.1 Import contract

The importer MUST accept a firm ID, tax year, and `.csv.gz` stream. It MUST:

1. decompress and parse incrementally with bounded buffers;
2. validate the UTF-8 header exactly against the eight required columns;
3. assign a one-based source record number after the header;
4. compute SHA-256 over the decompressed CSV byte stream;
5. `COPY FROM STDIN` into a transaction-local staging table;
6. validate every staged row with set-based SQL;
7. create the dataset and merge payments in the same transaction; and
8. commit only after row counts and constraints agree.

No malformed row may be silently skipped. An error MUST identify the source
record number and MUST roll back the complete import.

Because PostgreSQL does not support `COPY FROM` into an RLS-enabled table, the
temporary staging table is not tenant-visible. Only a tenant-scoped merge writes
the permanent RLS-protected tables.

Two concurrent imports of the same firm/year MUST serialize on a firm/year
advisory transaction lock. The loser MUST return the already imported dataset if
the content hash matches. A different content hash for the same firm/year MUST
return `DATASET_CONFLICT`; revised exports are out of scope.

Identical rows at different source record numbers are distinct payments. A retry
of the same file is not distinct because INV-01 and INV-02 prevent reinsertion.

### 7.2 Field validation

| Field                | Rule                                                                                  |
| -------------------- | ------------------------------------------------------------------------------------- |
| `client_id`          | Non-empty; stored under the explicitly selected firm.                                 |
| `vendor_name`        | Non-empty after trimming outer whitespace; original text retained.                    |
| `vendor_tin`         | Blank becomes null; otherwise original text retained and a canonical form derived.    |
| `payment_date`       | Strict ISO date from 2025-01-01 through 2025-12-31.                                   |
| `amount`             | Strict decimal with at most two fractional digits; converted exactly to signed cents. |
| `payment_method`     | Exactly one of `check`, `ach`, `wire`, `cash`, `credit_card`, `paypal`.               |
| `backup_withholding` | Strict decimal cents; MUST be non-negative.                                           |
| `memo`               | UTF-8 text; empty is allowed.                                                         |

The source contains no service-category field. Therefore all payments are
assumed to be for services, subject to payment-method exclusion.

### 7.3 Vendor identity

Nonblank TINs identify vendors. Canonicalization removes ASCII spaces and
hyphens only. If the result is nine digits, the vendor identity is `tin:<digits>`.
If not, it is `malformed-tin:<trimmed-original>`, remains explainable, and is a
human exception before transmission.

A missing TIN cannot satisfy the rule that vendors are identified by TIN.
Missing-TIN rows are provisionally grouped only within one client by a
conservative normalized name: Unicode-independent ASCII case fold, trim, and
collapse internal ASCII whitespace. No fuzzy matching is allowed. The provisional
group uses the same threshold and withholding test as a valid-TIN group. A
qualifying group becomes a `TIN_MISSING` preflight exception so a human can
resolve identity before transmission; a nonqualifying group remains explainable
but does not create a filing obligation.

For a valid-TIN vendor, the display name is the most frequent trimmed spelling;
ties are broken lexicographically. Every original spelling remains visible in
the explanation view.

### 7.4 Payment classification ruleset `nec-2025-v1`

For each payment:

- `credit_card` is excluded with reason `PAYMENT_PROCESSOR_REPORTED`.
- `paypal` is excluded with reason `THIRD_PARTY_NETWORK_REPORTED`.
- `check`, `ach`, `wire`, and `cash` count toward the reportable amount.
- Negative eligible payments count and reduce the reportable amount.
- Backup withholding is summed for the vendor regardless of payment method.

For each client/vendor identity:

```text
reportable_cents = sum(amount_cents for counted payments)
withholding_cents = sum(backup_withholding_cents for all payments)
requires_form = reportable_cents >= 60000 OR withholding_cents > 0
```

The threshold is inclusive. If withholding creates an obligation but the
reportable amount is zero or negative, the obligation is retained and an
`AMOUNT_INVALID` preflight exception is opened; it is never silently dropped.

The classification is defined once as an immutable versioned SQL view. Both the
aggregate determination and payment explanation query MUST use that view.

## 8. Logical database schema

The [PostgreSQL data model and storage boundary specification](postgres-data-model-spec.md)
defines the authoritative physical model, migration protocol, role grants,
transaction boundaries, and `internal/store` API. Numbered implementation
migrations remain authoritative for exact SQL. This table is the cross-system
summary of minimum shape and load-bearing constraints.

| Table                | Purpose and required constraints                                                                                                                                 |
| -------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `firms`              | `id` primary key.                                                                                                                                                |
| `clients`            | Primary key `(firm_id, id)`; foreign key to `firms`.                                                                                                             |
| `datasets`           | Immutable import metadata; unique `(firm_id, tax_year)` and `(firm_id, content_sha256)`; unique `(id, firm_id)`.                                                 |
| `payments`           | Source facts; unique `(dataset_id, source_row_number)`; composite FKs to dataset and client including `firm_id`; no update/delete application grants.            |
| `determinations`     | Immutable completed-ruleset marker; unique `(dataset_id, ruleset_version)`.                                                                                      |
| `filings`            | Immutable original filing identity and amounts; globally unique `filing_key`; unique business identity from INV-05; preflight state and one-way eventual result. |
| `submission_batches` | Immutable payload identity plus durable state, retry schedule, lease, UTID, ReceiptId, and acknowledgment timestamps; unique `(firm_id, utid)` and `receipt_id`. |
| `batch_filings`      | Composite tenant/client FKs; unique `filing_id`; unique `(batch_id, slot)`; slot check 1 through 100.                                                            |
| `rate_gates`         | One row per firm with next call time and conservative crash fence.                                                                                               |
| `api_call_log`       | Append-only call intent/outcome audit for both submit and status calls.                                                                                          |

### 8.1 Required indexes

- `payments (dataset_id, client_id, vendor_identity)` including amount, method,
  and withholding columns used by determination.
- `submission_batches (next_action_at, lease_expires_at, id)` partial index for
  nonterminal states; the claim query filters absent or expired leases using
  database time.
- `filings (firm_id, client_id, state)` for status aggregation.
- `filings (rejection_reason)` partial index for rejected rows.
- `api_call_log (firm_id, started_at desc)` for rolling-budget verification.

Do not add indexes until a query or invariant requires them. In particular, do
not add GIN indexes to payload JSON.

### 8.2 Tenant isolation

Every permanent business table MUST enable and force row-level security. The
application role MUST not own the tables and MUST not have `BYPASSRLS`.
Transactions set a validated firm context with `SET LOCAL`. Policies compare the
row's `firm_id` to that context. Composite foreign keys additionally enforce
same-firm relationships.

Workers enumerate configured firm IDs, set one validated firm context, and claim
work for only that firm in each transaction. No application or worker runtime
role receives `BYPASSRLS`. All worker mutations also include explicit `firm_id`
predicates. Automated tests MUST attempt cross-firm reads, inserts, updates, and
foreign-key links.

## 9. Deterministic identities and canonical payloads

### 9.1 Filing key

The filing key is SHA-256 over a length-prefixed canonical sequence:

```text
schema_version = "filing-key-v1"
firm_id
client_id
tax_year = 2025
form_type = "1099-NEC"
vendor_identity = canonical valid TIN identity
revision = 0
```

Raw TINs, names, and other sensitive values MUST NOT appear in keys or logs.

### 9.2 Batch construction

Within each client, ready filings are sorted by filing key bytes and partitioned
into consecutive groups of at most 100. Batch creation and all `batch_filings`
inserts occur in one transaction.

Compute SHA-256 over `batch-ref-v1`, firm, client, tax year, and the ordered
filing keys. The first 16 digest bytes, with RFC 4122 version and variant bits
set, form a deterministic UUID. The transmitted UTID is:

```text
<uuid>:IRIS:<firm_id>::A
```

This satisfies the stub's required UTID syntax and reconstructs the same remote
identity when the same immutable filing set exists. The XML `SubmissionId` is a
separate deterministic identifier for the batch and MUST NOT be confused with
the opaque `ReceiptId` returned by the stub.

The canonical XML payload uses the exact project-owned `IRTransmission` profile
defined by `irs/README.md`. Element ordering is fixed, money is rendered as
base-10 dollars with exactly two fractional digits, optional null elements are
omitted only where the stub profile permits, and `Form1099NECDetail` elements are
ordered by filing key. Each filing key is sent as `RecordId`. The canonical XML
bytes and SHA-256 are stored before the batch becomes runnable. A worker MUST
recompute and compare the hash before every submission. A mismatch is a terminal
invariant failure and a human exception. Multipart boundaries need not be stable;
the enclosed XML bytes MUST be stable.

## 10. State machines

### 10.1 Filing state

```text
blocked_preflight (terminal in this project; future resolution creates a superseding filing)

ready -> batched -> accepted
                  -> rejected
```

There is no automatic transition out of `accepted` or `rejected`.

### 10.2 Batch state

```text
planned
  -> submitting
  -> submit_unknown
  -> submitted
  -> awaiting_ack
  -> acknowledged

Any nonterminal state -> invariant_failed
```

Allowed behavior:

| State                                                          | Next action                                                    |
| -------------------------------------------------------------- | -------------------------------------------------------------- |
| `planned`                                                      | Submit stable UTID and canonical XML payload.                  |
| `submitting` with live lease                                   | No other worker acts.                                          |
| `submitting` with expired lease                                | Treat as `submit_unknown`; lookup by UTID.                     |
| `submit_unknown`                                               | Lookup by UTID only.                                           |
| Lookup `found`                                                 | Store `ReceiptId` and schedule acknowledgment polling.         |
| Lookup definitive `404 NOT_FOUND`                              | Return to `planned`; retry the same UTID and payload.          |
| Lookup error or rate limit                                     | Remain `submit_unknown`; retry lookup with backoff.            |
| `submitted` or `awaiting_ack`                                  | Poll by `ReceiptId` or stable UTID.                            |
| Status `Processing`                                            | Remain `awaiting_ack`; schedule bounded-backoff poll.          |
| Complete acknowledgment                                        | Atomically insert every filing result and mark `acknowledged`. |
| Local payload/UTID invariant failure or `409 DUPLICATE_RECORD` | `invariant_failed`; never submit again.                        |
| `409 DUPLICATE_UTID`                                           | Reconcile by UTID; never generate a replacement UTID.          |

The worker MUST claim due rows with one short transaction using `FOR UPDATE SKIP
LOCKED`, set a lease owner/expiry, and commit before network I/O. Leases are
recoverability metadata and MUST NOT be used as duplicate prevention.

## 11. Live IRS stub contract

The E2E target is the already implemented standalone service under `irs/`.
Application configuration MUST expose `IRS_BASE_URL` and
`IRS_BEARER_TOKEN`, defaulting locally to `http://127.0.0.1:8081` and
`local-irs-token`. Before an E2E run, the harness MUST require
`GET /healthz` to return `200` with `ok`; it MUST fail fast rather than silently
substitute an in-process fake.

### 11.1 Submit

- `POST /IRIntakeAcceptanceA2A/1.0/irisa2a/v1/intake-acceptance`.
- Require `Authorization: Bearer <token>` and `Accept: application/xml`.
- Send `multipart/form-data` containing exactly one `file` part with media type
  `text/xml` or `application/xml`.
- Send one firm, one client, tax year 2025, transmission type `O`, form type
  `1099-NEC`, and 1 through 100 records.
- Use the firm ID as `TransmitterControlCd`, the stable UTID as
  `UniqueTransmissionId`, and each filing key as `RecordId`.
- Render compensation and withholding as exact dollars with two decimal places.
- Treat `200` as success and parse the response body containing only one opaque
  `ReceiptId`.
- Treat `503 SERVICE_UNAVAILABLE` as ambiguous and reconcile by UTID before any
  retry.

At default configuration, valid intake calls fail before record at roughly 7
percent and after record at roughly 5 percent. These are mutually exclusive
random outcomes. The stub records the whole transmission atomically in memory
before an after-record `503` is sent.

### 11.2 Status

Call `POST /IRIntakeAcceptanceA2A/1.0/iris/transstatusorack` with
`Content-Type: application/xml`, `Accept: application/xml`, and the bearer
token. The XML request identifies the firm and searches by either `RECEIPTID`
or `UTID`. A recorded transmission is immediately visible. A missing lookup
returns `404 NOT_FOUND`. A found response returns both `ReceiptId` and UTID.

Before acknowledgment, `TransmissionStatusCd` is `Processing` and record results
are absent. Otherwise it is `Accepted`, `PartiallyAccepted`, or `Rejected`, with
exactly one `RecordResultGrp` per submitted `RecordId`. The default acknowledgment
delay is a random duration from 10 through 30 seconds. A stub started with
`IRS_STUB_NEVER_ACK_PERCENT=100` remains `Processing` indefinitely.

### 11.3 Acknowledgment

Each filing is accepted with an opaque `IRSRecordId` or rejected with exactly
one of:

- `TIN_MISSING`
- `TIN_MALFORMED`
- `TIN_INVALID`
- `AMOUNT_INVALID`

For a filing with multiple defects, deterministic precedence is the order above.
Production candidates are preflighted, but the stub MUST independently validate
all inputs.

### 11.4 Idempotency and lifetime

During one stub process lifetime, a single mutex atomically enforces:

- unique `(firm_id, UTID)`, returning `409 DUPLICATE_UTID` on reuse;
- unique original `RecordId` per firm, client, and tax year, returning
  `409 DUPLICATE_RECORD` if it appears under another UTID; and
- one frozen acknowledgment result per recorded item.

The stub does not accept or validate a canonical payload hash and does not return
the original `ReceiptId` from a duplicate intake call. The client MUST resolve a
duplicate UTID through status lookup and MUST enforce payload stability in
PostgreSQL. Restarting the stub clears submissions, call history, and
idempotency state; test orchestration MUST make that boundary explicit.

## 12. Retry policy

Transport errors, HTTP 5xx, and HTTP 429 are retryable, subject to the ambiguous
submit rule: an intake transport error or 5xx transitions to UTID lookup before
another intake attempt. `409 DUPLICATE_UTID` also transitions to UTID lookup.
Validation errors, authentication errors, `409 DUPLICATE_RECORD`, local payload
invariant failures, and impossible state transitions are terminal. Retryable
work uses exponential backoff with bounded jitter and a cap. Tests inject a
clock and deterministic jitter; production uses database time for due-work
comparisons.

No retry count may cause a batch to be abandoned silently. Exhausted retries
open an exception and leave durable state available for explicit operator
action.

## 13. Rate budget

The limit is 20 calls in every rolling 60-second window per firm, shared by
submit and status calls across all clients and processes.

The client-side limiter MUST:

1. acquire a firm-scoped PostgreSQL advisory session lock;
2. wait until the durable firm gate permits a call;
3. persist an `api_call_log` intent and conservative crash fence before I/O;
4. make exactly one HTTP call while retaining the firm lock;
5. persist completion and a next-call time at least 3.1 seconds after completion;
6. retain a 60-second conservative fence if completion is ambiguous; and
7. release the advisory lock.

There is no catch-up burst after downtime. The supplied stub neither enforces a
rolling rate limit nor exposes call history. Tests MUST evaluate committed
`api_call_log.started_at` timestamps and MUST also verify, with a recording HTTP
transport in an integration test, that each application log intent corresponds
to at most one outbound call. A live-stub E2E run proves interoperability and
shared client-side pacing, not receiver-side enforcement.

A production receiver-side limit would remain the final safety boundary for a
pathological failure between committing a local call intent and the network
stack transmitting it. That boundary is outside what this stub can prove.

## 14. Truthful status projection

The UI MUST query committed database facts. It MUST not read process memory or a
separately refreshed status cache.

For a completed determination, a client headline status is derived in this
precedence order:

1. `needs_attention`: one or more open preflight, rejection, stale acknowledgment,
   exhausted retry, or invariant exceptions exist.
2. `awaiting_the_irs`: no attention item exists and at least one batch has an
   unknown, submitted, or pending acknowledgment outcome.
3. `fully_filed`: every transmittable required filing is accepted and there are
   no open exceptions; this includes a client with no required filings.
4. `partially_filed`: a run has started but neither of the preceding states
   applies, including ready, batched, or mixed accepted/unsent filings.

The page MUST also show counts for required, blocked, ready, pending, accepted,
and rejected filings so the headline cannot hide mixed progress.

The exception list is a SQL projection grouped by:

- missing TIN;
- malformed or invalid TIN preflight;
- invalid nonpositive amount;
- rejected filing and exact reason;
- submission unacknowledged beyond configurable threshold;
- retry exhausted;
- payload/UTID invariant failure; and
- unknown state or local/remote reconciliation mismatch.

Stale pending submissions remain pollable after being shown as exceptions. They
MUST NOT be resubmitted.

## 15. Edge-case decisions

| Case                                                                   | Required behavior                                                                                                                            |
| ---------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| Same TIN, three names                                                  | One vendor aggregate and filing; all names visible.                                                                                          |
| Gross $800, eligible reversal to net $250                              | No threshold filing unless withholding is positive.                                                                                          |
| Exactly $600.00                                                        | Filing required.                                                                                                                             |
| Missing TIN                                                            | Provisional name group plus human exception; never silently skipped.                                                                         |
| $2,400 total, $1,900 credit card                                       | Reportable amount $500; no threshold filing unless withholding is positive.                                                                  |
| $400 with backup withholding                                           | Filing required for $400 and withholding reported.                                                                                           |
| PayPal payment                                                         | Excluded as third-party network payment.                                                                                                     |
| Negative eligible payment                                              | Reduces reportable amount.                                                                                                                   |
| Negative excluded-method payment                                       | Excluded; does not reduce reportable amount.                                                                                                 |
| Duplicate identical source rows                                        | Both retained if source record numbers differ.                                                                                               |
| Same file imported twice                                               | Existing dataset returned; payment count and values unchanged.                                                                               |
| Same firm/year, different file                                         | `DATASET_CONFLICT`; no merge.                                                                                                                |
| Import process killed during COPY or merge                             | Transaction rolls back; retry imports all rows once.                                                                                         |
| Concurrent same-file imports                                           | One commit; one returns existing dataset.                                                                                                    |
| Wrong firm selected by operator                                        | Data remains isolated under selected firm; source has no trustworthy firm field, so operator intent cannot be inferred.                      |
| Empty export                                                           | Reject; supplied full export is expected.                                                                                                    |
| Invalid UTF-8, header, date, method, or money                          | Fail whole import with source record number.                                                                                                 |
| More than two decimal places                                           | Reject rather than round.                                                                                                                    |
| Valid TIN with punctuation                                             | ASCII spaces/hyphens removed before nine-digit validation.                                                                                   |
| TIN starts with `000`                                                  | Locally preflighted as invalid and supported independently by stub rejection.                                                                |
| Missing TIN with name variants                                         | No fuzzy merge; every provisional group remains visible for human resolution.                                                                |
| Withholding plus zero/negative reportable amount                       | Obligation retained; `AMOUNT_INVALID` attention item.                                                                                        |
| Submission failure before record                                       | UTID lookup returns definitive `404 NOT_FOUND`; retry the same UTID and XML.                                                                 |
| Submission recorded then errors                                        | UTID lookup finds the original `ReceiptId`; never submit a new UTID.                                                                         |
| Crash before submit                                                    | Expired lease returns batch to normal processing.                                                                                            |
| Crash during/after submit                                              | Expired lease becomes `submit_unknown`; lookup first.                                                                                        |
| Crash after response before local commit                               | Same as unknown; lookup reconstructs local state.                                                                                            |
| Status call fails                                                      | Retry status only.                                                                                                                           |
| Acknowledgment never arrives                                           | Continue low-frequency polls and open stale exception; never resubmit.                                                                       |
| Partial per-filing rejection                                           | Accepted results remain final; rejected results need attention.                                                                              |
| Replayed filing run                                                    | Filing and batch uniqueness return existing durable identities.                                                                              |
| UTID paired with changed payload                                       | Terminal local invariant failure before HTTP; the stub does not compare payloads.                                                            |
| Application database restored while the same stub process remains live | Deterministic keys reconstruct UTIDs and RecordIds; reconcile by UTID before new sends.                                                      |
| Stub process restarts                                                  | Remote state and uniqueness history are lost; stop workers and start a fresh isolated E2E scenario. No cross-restart recovery claim is made. |
| Worker clock skew                                                      | Database time controls leases, due work, and limiter state.                                                                                  |
| Worker panic                                                           | Lease expires; durable state remains authoritative.                                                                                          |
| Two firms run concurrently                                             | Separate 20-call budgets and strict data isolation.                                                                                          |

## 16. Durability, security, and operations

- The application MUST use transactions for every state transition.
- Network calls MUST NOT occur inside ordinary row-locking transactions.
- PostgreSQL statement and lock timeouts MUST be finite.
- HTTP connect, response-header, and total request timeouts MUST be finite.
- Graceful shutdown stops claims, cancels idle work, and allows bounded in-flight
  persistence; correctness MUST also survive `SIGKILL`.
- TINs and payment memos MUST NOT appear in logs, metrics, URLs, or idempotency
  keys. UI detail SHOULD mask TINs by default.
- SQL values MUST use parameters. Dynamic identifiers are forbidden in request
  paths.
- Production guidance MUST require TLS, encrypted storage, backups, point-in-time
  recovery, synchronous replication for lower data-loss risk, and restore drills.
- A single local PostgreSQL process is crash durable when correctly configured,
  but it is not available through host or disk loss. Remote deterministic
  idempotency remains necessary even with replication.

## 17. Performance design and budgets

### 17.1 Import

- One pass through gzip/CSV, bounded buffers, and `COPY FROM STDIN` to staging.
- One set-based validation/merge transaction.
- No per-row SQL and no slice containing the full export.
- Acceptance: each approximately 500,000-row file completes in under 120 seconds
  on the documented normal development machine.
- Memory acceptance: peak RSS for a 4x repeated-row fixture is no more than
  baseline plus 1.5 times the peak incremental RSS of the 1x fixture.

### 17.2 Determination

- One indexed `GROUP BY`/`FILTER` pass over the immutable dataset.
- Insert only vendor-level filing candidates; payment explanations are queried
  from the versioned classification view on demand.
- Acceptance: both supplied datasets complete in under 60 seconds total on the
  documented machine.

### 17.3 Status

- Aggregate from candidate/result/batch tables, not the million-row payment table.
- Acceptance: per-firm status and exception pages have p95 database time below
  250 ms over 100 local requests after determination.

Transmission throughput is intentionally bounded by the IRS budget. The limiter
MUST favor correctness over utilization after ambiguous failures.

## 18. Acceptance test plan

### 18.1 Required critical tests

`TEST-TX-CRASH-01` is mandatory:

1. Create more than 100 valid filings for one client so at least two batches exist.
2. Start a dedicated stub process with before-record failure `0`, after-record
   failure `100`, and never-acknowledge `0`; wait for `/healthz` before workers.
3. Kill the worker process after its intake receives the ambiguous `503` and
   before it persists the response, using a deterministic worker test hook.
4. Start a new worker process with no in-memory state from the first process.
5. Wait for reconciliation and acknowledgment.
6. Query each batch by UTID and assert one stable `ReceiptId` and exactly one
   result for every expected `RecordId`, with no missing or repeated IDs.
7. Query by each `ReceiptId` and assert it returns the same UTID and results.
8. Assert local accepted/rejected outcomes and IRS record IDs equal those public
   stub responses.
9. Assert the second batch also completes and no filing is lost. Keep the same
   stub process alive until all assertions finish.
10. Assert every rolling 60-second window in the durable application call log
    contains at most 20 calls for the firm.

The ordinary live E2E suite MUST target `IRS_BASE_URL`, defaulting to the already
running `http://127.0.0.1:8081`, and treat the service as a black box. It MUST
not construct `internal/stub.Server`, read stub memory, or require stub database
access. Tests that require `100` percent fault modes MAY launch a dedicated
`irs/cmd/irsstub` process on an isolated address with the documented environment
variables; they MUST never reconfigure or restart a user-owned live process.

Because the live stub retains state for its process lifetime, each E2E run MUST
use unique test client IDs, producing unique deterministic filing keys and
firm-scoped UTIDs under the normal identity algorithms. Cleanup by remote
deletion is unavailable. A run MUST NOT assume the server is empty, and a
collision with state from an earlier run is a test-fixture failure rather than
permission to generate a new identity for an in-progress batch.

Additional mandatory tests:

| Test                       | Proof                                                                                                                                           |
| -------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `TEST-IMP-IDEMP-01`        | Import same file twice; compare complete table checksums/counts before and after.                                                               |
| `TEST-IMP-KILL-01`         | Kill importer during COPY and during merge; retry produces exact source row count once.                                                         |
| `TEST-IMP-MEM-01`          | 1x and 4x streaming fixtures meet flat-memory criterion.                                                                                        |
| `TEST-TENANT-01`           | Cross-firm read/write/link attempts fail or return no rows.                                                                                     |
| `TEST-DET-RULES-01`        | All six supplied situations produce exact expected totals, inclusion reasons, and obligations.                                                  |
| `TEST-DET-BOUNDARY-01`     | 599.99, 600.00, 600.01; positive withholding below threshold; net zero/negative.                                                                |
| `TEST-TX-MATRIX-01`        | Deterministic failure injection at every transition in Section 15 converges after restart.                                                      |
| `TEST-TX-CONCURRENCY-01`   | Multiple workers converge to one UTID/ReceiptId result set; duplicate intake attempts create no second visible transmission.                    |
| `TEST-TX-PAYLOAD-01`       | A changed canonical XML hash for a stored UTID fails locally before HTTP; stable XML is used for every retry.                                   |
| `TEST-LIVE-E2E-01`         | Health check, multipart XML intake, UTID/ReceiptId lookup, delayed acknowledgment, and local result persistence work against `IRS_BASE_URL`.    |
| `TEST-RATE-01`             | Two firms, many clients, submit/status mix, retries, and restarts never exceed either firm budget in durable logs and a recording transport.    |
| `TEST-STATUS-01`           | Each status and precedence combination matches committed facts before and after restart.                                                        |
| Existing `irs/` race suite | `go test -race ./...` in `irs/` proves wire validation, fault thresholds, duplicate handling, delayed acknowledgment, and rejection precedence. |

Property/fuzz tests MUST cover money parsing, canonicalization, deterministic
filing/batch keys, CSV quoting/newlines, and state-transition rejection.

### 18.2 Requirement traceability

| Requirement                                          | Acceptance evidence                                                                                                   |
| ---------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| Import both supplied firm files                      | `TEST-IMP-E2E-01`, row counts by F001/F002.                                                                           |
| Full firm import well under two minutes              | `TEST-PERF-IMP-01`, logged wall time and machine details.                                                             |
| Same file twice has exactly same state               | `TEST-IMP-IDEMP-01`.                                                                                                  |
| Interrupted import retries without loss/duplication  | `TEST-IMP-KILL-01`.                                                                                                   |
| Memory remains roughly flat                          | `TEST-IMP-MEM-01`.                                                                                                    |
| One firm never touches another                       | `TEST-TENANT-01`.                                                                                                     |
| Explain counted/excluded payments and total          | `TEST-DET-EXPLAIN-01` against versioned view.                                                                         |
| Aggregate vendors by TIN, not name                   | Situation 1 in `TEST-DET-RULES-01`.                                                                                   |
| Net reversals                                        | Situation 2 in `TEST-DET-RULES-01`.                                                                                   |
| Inclusive $600 threshold                             | Situation 3 and `TEST-DET-BOUNDARY-01`.                                                                               |
| Missing TIN remains an obligation/exception          | Situation 4 and `TEST-STATUS-01`.                                                                                     |
| Card/network payments excluded                       | Situation 5 plus PayPal case in `TEST-DET-RULES-01`.                                                                  |
| Backup withholding requires filing                   | Situation 6 and `TEST-DET-BOUNDARY-01`.                                                                               |
| Full determination under one minute                  | `TEST-PERF-DET-01`, logged wall time and machine details.                                                             |
| Submission has at most 100 filings, one client       | Database constraints, `TEST-LIVE-E2E-01`, and stub `TestServerTransmissionValidation`.                                |
| Success returns only `ReceiptId`                     | `TEST-LIVE-E2E-01` and stub `TestServerSubmitAndAcknowledgeMixedResults`.                                             |
| Random before-record failure around 7 percent        | Stub `TestDefaultFailureThresholds` verifies deterministic threshold boundaries.                                      |
| Random after-record failure around 5 percent         | Stub `TestDefaultFailureThresholds`; `TEST-TX-CRASH-01` proves client recovery.                                       |
| Lookup by `ReceiptId` or UTID                        | `TEST-LIVE-E2E-01` and stub `TestServerSubmitAndAcknowledgeMixedResults`.                                             |
| Configurable delayed/never acknowledgment            | Stub `TestServerSubmitAndAcknowledgeMixedResults`, `TestServerNeverAcknowledges`, and stale case in `TEST-STATUS-01`. |
| Per-filing exhaustive accepted/rejected result       | Stub `TestServerSubmitAndAcknowledgeMixedResults`.                                                                    |
| Shared 20/60 rolling rate budget per firm            | Client-side evidence from `TEST-RATE-01` and step 10 of `TEST-TX-CRASH-01`; receiver enforcement is not modeled.      |
| Zero duplicate filings within one live-stub lifetime | INV-05 through INV-14, `TEST-TX-CRASH-01`, concurrency and replay tests.                                              |
| Kill mid-batch and resume test                       | `TEST-TX-CRASH-01`.                                                                                                   |
| Interoperate with the running stub                   | `TEST-LIVE-E2E-01` against `http://127.0.0.1:8081`.                                                                   |
| Per-client truthful status                           | `TEST-STATUS-01`.                                                                                                     |
| Grouped human exception list                         | `TEST-STATUS-01`.                                                                                                     |
| Light UI can kick off actions and navigate           | `TEST-WEB-01` HTTP integration test and walkthrough.                                                                  |
| Local setup/import/run/state-model documentation     | Project README acceptance review.                                                                                     |
| Import and determination timing task/log             | `make perf` output committed as a sample artifact after implementation.                                               |

## 19. Definition of done

Implementation is complete only when:

1. every invariant has a database constraint or a named test where a constraint
   is impossible;
2. every traceability row has passing evidence;
3. `TEST-TX-CRASH-01` passes repeatedly under process-level `SIGKILL`;
4. black-box UTID and ReceiptId responses contain exactly one result per expected
   filing key, and application-side rate evidence contains no over-budget window;
5. performance and memory budgets pass on both supplied files;
6. dependency licenses pass the policy in the research document; and
7. setup, state model, timing output, and known limitations, including ephemeral
   stub state and absent receiver-side rate enforcement, are documented.
