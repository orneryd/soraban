# Research and Architecture Decisions

Status: Draft
Date: 2026-08-30
Companions: [Formal draft specification](formal-draft-spec.md) and
[PostgreSQL data model specification](postgres-data-model-spec.md)

## 1. Decision summary

Build the application in Go with PostgreSQL and `pgx`. Use ordinary PostgreSQL
tables for source facts, determinations, the transactional outbox, worker leases,
rate accounting, acknowledgments, and status. Keep the existing in-memory IRS
stub in a separate process and communicate with it only over HTTP.

This is the smallest design found that offers all required primitives without
adding a second consistency system:

- streaming import with `COPY FROM STDIN`;
- exact uniqueness and foreign-key constraints;
- transactional creation of filing intent and runnable work;
- concurrent claims with `FOR UPDATE SKIP LOCKED`;
- advisory locks for firm-wide serialization;
- row-level security and composite tenant keys;
- partial indexes for due work;
- crash-durable logged storage; and
- one source of truth for fast, truthful status.

The system does not claim that a transactional outbox alone provides exactly-once
effects. Zero duplicate filings follows from an end-to-end protocol: immutable
local intent, stable permanent reference, immutable payload hash, receiver-side
unique filing key, and reconciliation by the same reference after every
ambiguous outcome.

## 2. Database evaluation

| Candidate     | License posture                                                            | Fit                                                                                                                      | Decision |
| ------------- | -------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ | -------- |
| PostgreSQL    | PostgreSQL License, permissive                                             | Native `COPY`, constraints, RLS, advisory locks, `SKIP LOCKED`, partial indexes, strong transactions                     | Select   |
| SQLite        | Public domain                                                              | Excellent embedded database, but serialized writes and no `SKIP LOCKED` make multi-worker/rate-gate coordination awkward | Reject   |
| MySQL/MariaDB | GPL family                                                                 | Capable SQL databases, but fail a strict permissive-only runtime policy                                                  | Reject   |
| CockroachDB   | Source-available and enterprise licensing applies to current distributions | Distributed SQL is unnecessary and licensing is outside the allowlist                                                    | Reject   |
| YugabyteDB    | Apache-2.0 core                                                            | Permissive core but materially more operational surface than this local project needs                                    | Reject   |
| FoundationDB  | Apache-2.0                                                                 | Strong transactional substrate but would require rebuilding SQL/data-access features already present in PostgreSQL       | Reject   |
| Redis         | Current licensing includes AGPL/source-available choices depending release | Adds another durable system and is unnecessary for this queue                                                            | Reject   |

PostgreSQL is not selected merely because it can act as a queue. It is selected
because the work item and the business state it represents can be committed in
the same transaction. A broker would improve neither the remote idempotency
boundary nor the local atomicity proof.

## 3. PostgreSQL evidence

The following official documentation controls implementation details:

| Primitive         | Relevant conclusion                                                                                                | Source                                                                                                           |
| ----------------- | ------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------- |
| `COPY`            | `COPY FROM` moves rows from a stream into a table; pgx exposes the protocol without retaining the full file        | [PostgreSQL COPY](https://www.postgresql.org/docs/current/sql-copy.html)                                         |
| Row locking       | `SKIP LOCKED` gives an inconsistent view unsuitable for ordinary queries but explicitly suits queue-like consumers | [PostgreSQL SELECT locking clause](https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE) |
| Partial indexes   | A due-work index can exclude terminal rows and reduce write/index cost                                             | [PostgreSQL partial indexes](https://www.postgresql.org/docs/current/indexes-partial.html)                       |
| Advisory locks    | Application-defined locks can serialize one firm's rate gate across processes                                      | [PostgreSQL advisory locks](https://www.postgresql.org/docs/current/explicit-locking.html#ADVISORY-LOCKS)        |
| RLS               | Enabled tables default-deny without a policy; owners normally bypass unless row security is forced                 | [PostgreSQL row security](https://www.postgresql.org/docs/current/ddl-rowsecurity.html)                          |
| RLS and `COPY`    | `COPY FROM` is not supported for tables with row-level security enabled                                            | [PostgreSQL COPY notes](https://www.postgresql.org/docs/current/sql-copy.html#SQL-COPY-NOTES)                    |
| Durability        | `fsync`, `full_page_writes`, and synchronous commit settings define acknowledged-commit durability                 | [PostgreSQL WAL reliability](https://www.postgresql.org/docs/current/wal-reliability.html)                       |
| Numeric exactness | Integer types are exact and suit cents without decimal parsing ambiguity                                           | [PostgreSQL numeric types](https://www.postgresql.org/docs/current/datatype-numeric.html)                        |

Two cautions shape the design:

1. `SKIP LOCKED` is only a claiming mechanism. A lease may expire while a slow
   request is still live, so it cannot provide external duplicate prevention.
2. A committed local transaction and a committed remote transaction cannot be
   made atomic without a shared transaction protocol. Reconciliation is part of
   the normal state machine, not an exceptional repair path.

## 4. Live queue schema survey

The survey used maintained projects' live schemas to avoid inventing queue
metadata from memory. Their code is not copied and none is a runtime dependency.

| Project         | Useful schema lessons                                                                                                                       | License / decision                                  | Source                                                                                                                                                       |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| River           | Explicit states, attempts, scheduling/finalization times, uniqueness metadata, appendable errors, and indexes specialized by runnable state | MPL-2.0; reject under strict permissive-only policy | [River repository](https://github.com/riverqueue/river), [license](https://github.com/riverqueue/river/blob/master/LICENSE)                                  |
| Graphile Worker | Database functions claim jobs atomically; rows include run time, attempt limits, lock owner/time, queue identity, and deduplication key     | MIT; reference only because application is Go       | [Graphile Worker schema](https://github.com/graphile/worker/tree/main/src/generated/sql), [license](https://github.com/graphile/worker/blob/main/LICENSE.md) |
| pg-boss         | Retry/backoff, expiration, singleton/debounce keys, archive strategy, and explicit job states are durable row metadata                      | MIT; reference only because application is Go       | [pg-boss repository](https://github.com/timgit/pg-boss), [license](https://github.com/timgit/pg-boss/blob/master/LICENSE)                                    |
| Que             | Priority/run time, error history, and advisory-lock coordination demonstrate a small PostgreSQL-backed queue                                | MIT; reference only because application is Go       | [Que repository](https://github.com/que-rb/que), [license](https://github.com/que-rb/que/blob/master/LICENSE.txt)                                            |

The local schema deliberately omits generic-queue features that the filing state
machine does not use: arbitrary task names, generic JSON arguments, cron,
workflows, tags, job cancellation APIs, and job archives. A filing batch is the
work item, so its domain state is also its queue state.

## 5. Exactly-once and workflow research

### 5.1 Receiver idempotency is the decisive boundary

AWS's idempotent API guidance describes caller-provided request identifiers as a
way to distinguish retries from new intent and requires semantic equivalence to
be checked when identifiers are reused. This supports the stable reference plus
payload-hash contract.

Source: [Making retries safe with idempotent APIs](https://aws.amazon.com/builders-library/making-retries-safe-with-idempotent-APIs/).

Stripe documents a practical idempotency contract that returns a previous result
for a reused key and rejects changed parameters. It also documents automatic key
pruning after at least 24 hours. The semantic pattern is useful, but an expiring
retention window is insufficient for the word "ever" in this project. Filing
keys and references must be permanent.

Source: [Stripe idempotent requests](https://docs.stripe.com/api/idempotent_requests).

Apache Flink's end-to-end exactly-once discussion makes the same boundary clear:
internal checkpointing is insufficient unless external sinks participate through
transactions or idempotent writes. The selected design uses idempotent receiver
writes because the application and stub cannot share one transaction.

Source: [An overview of end-to-end exactly-once processing](https://flink.apache.org/2018/03/15/an-overview-of-end-to-end-exactly-once-processing-in-apache-flink/).

### 5.2 Durable execution does not erase business semantics

Stonebraker, Zhou, Kraft, and Li's CIDR 2026 paper argues that durable execution
is essential but insufficient for correct data-oriented workflows. Exactly-once
step execution and guaranteed compensation do not by themselves establish whole
workflow consistency and correctness; the paper proposes workflow-level AC/DC
semantics.

That result reinforces two local choices:

- state transitions and allowed compensations are explicit domain rules rather
  than hidden behind a generic workflow retry promise; and
- acceptance is proved using end-state invariants under injected failures, not
  merely by proving that each worker function ran once.

Source: Michael Stonebraker, Xinjing Zhou, Peter Kraft, and Qian Li,
[Consistency and Correctness in Data-Oriented Workflow Systems](https://www.vldb.org/cidrdb/2026/consistency-and-correctness-in-data-oriented-workflow-systems.html),
CIDR 2026.

DBOS's 2025 project update demonstrates that database-oriented durable workflows
are viable and can provide provenance and language-integrated execution. It is
useful validation for database-backed workflow state, but adding a durable
execution framework would not remove the IRS idempotency requirement and would
increase this project's implementation surface.

Source: Qian Li, Peter Kraft, Christos Kozyrakis, Matei Zaharia, and Michael
Stonebraker, [DBOS: three years later](https://mast.stanford.edu/pubs/dbos_three_years_later/),
The International Journal on Very Large Data Bases, 2025,
[DOI 10.1007/s00778-024-00899-0](https://doi.org/10.1007/s00778-024-00899-0).

## 6. Job and workflow alternatives

| Alternative     | License                                     | Why it is not selected                                                                                           |
| --------------- | ------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| River           | MPL-2.0                                     | Good Go/PostgreSQL fit, but file-level copyleft violates the requested strict permissive stack.                  |
| Temporal        | MIT for open-source server and Go SDK       | Operationally large for four explicit batch states; remote activities still need idempotency.                    |
| DBOS            | Permissive open-source SDKs vary by package | No framework can make a nontransactional IRS side effect atomic; unnecessary abstraction for this state machine. |
| Kafka           | Apache-2.0                                  | Adds brokers and an event-log consistency model while local intent already lives transactionally in PostgreSQL.  |
| Graphile Worker | MIT                                         | Strong PostgreSQL design, but TypeScript runtime mismatch and generic queue features are unnecessary.            |
| pg-boss         | MIT                                         | Same reasoning as Graphile Worker.                                                                               |
| Que             | MIT                                         | Ruby runtime mismatch; Rails was only a preference, not a requirement.                                           |

Handwritten SQL is not a commitment to build a general queue. It is a commitment
to implement only the filing state transitions named in the specification.

## 7. Selected dependency license matrix

| Component                         | Use                                              | License                         | Policy result              |
| --------------------------------- | ------------------------------------------------ | ------------------------------- | -------------------------- |
| Go toolchain and standard library | Application language/runtime                     | BSD-3-Clause                    | Allow                      |
| PostgreSQL                        | Application database and `psql` migration runner | PostgreSQL License              | Allow                      |
| `github.com/jackc/pgx/v5`         | PostgreSQL driver, pool, and streaming COPY      | MIT                             | Allow                      |
| Project-owned Go/SQL/HTML/CSS     | Product code                                     | Repository license to be chosen | No third-party restriction |

Sources:

- [Go license](https://go.dev/LICENSE)
- [PostgreSQL license](https://www.postgresql.org/about/licence/)
- [pgx license](https://github.com/jackc/pgx/blob/master/LICENSE)

No ORM, migration library, JavaScript package, CSS framework, icon package,
embedded font, container image layer, queue library, or workflow engine is
required by the design.

Before a release, CI MUST enumerate every Go module in the resolved build list,
record its version and license, fail unknown licenses, and generate a third-party
notices file. The allowlist is MIT, ISC, BSD-2-Clause, BSD-3-Clause, the
PostgreSQL License, and Apache-2.0. GPL, AGPL, LGPL, MPL, SSPL, BSL, Commons
Clause, noncommercial, and unknown/custom licenses are denied unless the policy
is explicitly revised. Build/test-only tools MUST be checked separately and are
not shipped merely because their license is acceptable.

## 8. Rejected shortcuts

- **Retry submit after every error:** duplicates failure-mode-B filings.
- **Generate a new reference per retry:** defeats receiver idempotency.
- **Expire idempotency rows:** weakens "ever" to a retention window.
- **Use a lease as exactly-once protection:** leases necessarily expire and can
  overlap slow or partitioned work.
- **Mark submitted before the HTTP call:** avoids duplicates but can permanently
  omit a filing after a pre-record failure.
- **Mark submitted only after success:** retries ambiguous committed submissions.
- **Transactional outbox without remote deduplication:** makes intent atomic but
  cannot make the external side effect atomic.
- **Hold a database row transaction over HTTP:** increases contention and still
  cannot make two databases commit atomically.
- **Status from logs or in-memory progress:** becomes false after restart and
  cannot express ambiguous outcomes.
- **Per-client rate limiters:** violate the one shared firm channel.
- **Floating-point money:** introduces representation and boundary errors.
- **Fuzzy matching missing-TIN vendors:** can merge distinct legal recipients and
  create a false filing.

## 9. Open assumptions to validate during implementation

1. PostgreSQL 18.4 is available for local testing in the dedicated
   `readiness-postgres` Docker container at `127.0.0.1:55432`; connection details
   and lifecycle commands are defined in the formal specification.
2. The supplied CSV values conform to the documented columns; strict inspection
   will determine maximum field sizes and exact malformed-data fixtures.
3. The local IRS stub's in-memory UTID and original-record uniqueness model the
   receiver guarantee only while one stub process remains alive. A production
   adapter must verify equivalent durable IRS semantics before reuse.
4. Performance budgets must be recorded with CPU, memory, storage, Go,
   PostgreSQL, and configuration details; no research result substitutes for the
   required benchmark on the supplied files.
5. The final repository license must be selected before distribution. This
   architecture is compatible with choosing MIT for project-owned code.
