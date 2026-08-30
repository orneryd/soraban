# AI-Assisted Development Self-Review

Date: 2026-08-30
Project: Readiness Engineer take-home
Elapsed time reported by candidate: approximately 3 hours
Perspective: LLM-as-judge of the candidate's prompting, context strategy, and use
of an AI coding agent

## Executive assessment

**Overall AI-pilot grade: A- (8.7/10)**

This was strong, senior-leaning use of an AI coding agent. The candidate did not
use the model merely as a code generator. They progressively established system
boundaries, corrected inaccurate assumptions, demanded live integrations, moved
test data into the repository for portability, and repeatedly required
executable evidence.

The resulting system is unusually substantial for a three-hour take-home:
streaming million-row ingestion, set-based determination, durable batch state,
rate serialization, a real IRS HTTP adapter, a process-kill recovery test, and a
working status UI. The strongest signal is not volume of generated code; it is
that the candidate kept returning the agent to correctness boundaries and
observable tests.

The principal weakness was sequencing. Several important constraints were
introduced after earlier design work had already been completed: the existing
IRS stub, the separate PostgreSQL module, the service split, portable fixture
locations, and the cross-module Go API. This caused repeated specification
rewrites and avoidable implementation churn. A single source-of-truth checklist
and module map at the beginning would have made the session shorter and safer.

## Scorecard

| Dimension               | Grade | Assessment                                                                                                                     |
| ----------------------- | ----- | ------------------------------------------------------------------------------------------------------------------------------ |
| Problem decomposition   | A     | Broke the problem into IRS, PostgreSQL lifecycle, and service orchestration with clear ownership.                              |
| Reliability thinking    | A     | Consistently prioritized idempotency, ambiguous outcomes, leases, durable facts, and process death.                            |
| Context grounding       | A-    | Supplied concrete files, URLs, fixtures, and live services; some critical context arrived late.                                |
| Specification strategy  | A-    | Strong invariants and acceptance framing, but initially over-specified and duplicated across documents.                        |
| Prompt precision        | B+    | Intent was consistently clear despite typos; some prompts used broad terms such as "full E2E" without an explicit test matrix. |
| Verification discipline | A     | Repeatedly requested real PostgreSQL, live IRS, real assets, race tests, and process-level failure tests.                      |
| Scope control           | B+    | Correctly rejected fluff later, but the session accumulated three overlapping specifications and some unnecessary churn.       |
| Performance direction   | A     | Explicitly required streaming, bounded memory, batching, useful concurrency, and mechanical sympathy.                          |
| Iteration efficiency    | B+    | Three hours for this result is excellent, but earlier boundary decisions could have removed several rework loops.              |
| Final artifact quality  | A-    | Strong operational prototype; strict assignment evidence still has documented gaps.                                            |

## What the candidate did especially well

### 1. Directed architecture instead of dictating implementation trivia

The candidate established useful ownership boundaries:

- `/irs` owns the remote test contract;
- `/postgres` owns SQL, transactions, RLS, claims, and durable serialization;
- `/service` owns process startup, file adaptation, orchestration, HTTP, and UI.

This is a high-value prompting pattern. It gives the agent freedom inside a
well-defined boundary while preventing accidental coupling.

The question "the postgres layer only connects to postgres and manages the
expected queries and serialization there, right?" was particularly effective.
It forced an architectural check rather than assuming the previous design was
correct. That review exposed the Go `internal` package issue before service
implementation.

### 2. Repeatedly anchored work in running systems

The prompts supplied concrete integration targets:

- IRS stub at `http://127.0.0.1:8081`;
- a dedicated Docker PostgreSQL instance;
- real F001 and F002 exports;
- checked-in portable fixture locations under `data/`.

This prevented the agent from replacing hard integration work with mocks. Moving
the fixtures into `data/` and then updating the plan was a good context-management
correction: it converted machine-specific knowledge into repository-owned test
state.

### 3. Used specifications as executable constraints

The candidate repeatedly asked for specifications before implementation, then
asked that they remain aligned with the original assignment and contain no
extra fluff. This produced explicit contracts for:

- deterministic identities;
- migration ownership;
- the persistence-neutral lifecycle API;
- service-to-database boundaries;
- IRS outcome mapping; and
- acceptance tests.

The best part of this strategy was asking for a clean lifecycle API that lets the
orchestrator know nothing about PostgreSQL. That decision made service unit tests
possible without a database and clarified where transactions belong.

### 4. Applied healthy pressure for real evidence

The candidate did not stop at generated code. They explicitly asked to:

- start actual PostgreSQL;
- use the already running IRS stub;
- consume the checked-in 500,000-row exports;
- run full E2E tests;
- use process-level crash testing; and
- optimize for memory and CPU behavior.

This pressure uncovered real defects that ordinary unit tests would miss:

- `bytea` scanning into `[32]byte`;
- PostgreSQL infinity timestamps into `time.Time`;
- lookup retry state handling;
- N+1 status queries;
- per-filing SQL during determination and planning; and
- lease expiry while waiting behind the 60-second crash fence.

Finding the final issue is especially strong evidence of good AI piloting. The
candidate's insistence on a real process-crash test exposed a cross-layer timing
bug that code review alone could easily miss.

### 5. Asked for mechanical sympathy at the right abstraction level

The candidate requested streaming, batching, useful goroutine parallelism, and
no wasted CPU work without prescribing an unsafe implementation. The resulting
choices were sound:

- one ordered streaming pipeline per file;
- PostgreSQL COPY instead of row inserts;
- set-based determination;
- bulk staging for batch planning; and
- one worker goroutine per independent firm, not per row or filing.

The fresh two-firm E2E completed in about 23 seconds, with each 500,000-row import
around 12 seconds and each determination around 3.6 seconds.

## Where the prompting strategy created avoidable churn

### 1. The source-of-truth hierarchy was not fixed at the start

The session moved among:

- `docs/README.md` as the original assignment;
- `docs/formal-draft-spec.md` as system design;
- `postgres/README.md` as database design; and
- `service/README.md` as service design.

This eventually became coherent, but only after several rewrites. The first
prompt should have declared:

1. `docs/README.md` is immutable product truth;
2. the formal draft may clarify but not expand requirements;
3. component READMEs define implementation contracts; and
4. tests are the completion evidence.

That would have reduced drift and prevented temporary contradictions such as a
durable PostgreSQL-backed stub versus the already implemented in-memory stub.

### 2. Major boundaries arrived serially

The interaction sequence was sensible but not maximally efficient:

1. adapt the formal spec to the existing IRS stub;
2. start PostgreSQL;
3. create a PostgreSQL spec;
4. simplify that spec;
5. add the lifecycle API;
6. implement PostgreSQL;
7. define the service split;
8. implement service;
9. relocate fixtures and update paths.

Each decision was individually good. Collectively, they show that the initial
architecture inventory was incomplete. A single opening prompt naming all three
modules, live dependencies, fixture locations, and deliverables would likely
have saved 20-30% of the interaction.

### 3. "Full E2E" needed an explicit definition

The candidate often used strong but broad language: "full e2e tests," "completely
satisfied," and "high-performance algorithms." The agent sometimes interpreted
these too generously.

A stronger prompt would enumerate the exact completion gates:

- real F001/F002 import and replay;
- process kill during importer COPY and merge;
- process kill after remote commit;
- 1x/4x RSS measurement;
- two-firm rolling-window rate assertion;
- exhaustive status precedence;
- browser payment explanation; and
- one command that runs each suite.

This matters because the final audit found that the application is operationally
complete but still lacks several strict acceptance proofs.

### 4. The session needed an explicit checkpoint ledger

The one-word prompt "continue" was workable because the agent retained context,
but it is fragile in a long coding session. A better resume prompt would be:

> Continue from the PostgreSQL implementation. Preserve current user edits.
> Remaining gates: migration E2E, real-data E2E, live IRS E2E, Mermaid, and full
> race validation. Do not start service work yet.

For a multi-hour take-home, maintain a small root `TASKS.md` or a checklist in
session memory with:

- requirement;
- owner module;
- implementation status;
- test command; and
- evidence artifact.

That would reduce rediscovery after compaction or interruption.

### 5. Specification should have converged before implementation

The candidate correctly asked to remove fluff, but only after the first database
spec grew to more than 500 lines. This is a useful lesson: ask for the smallest
falsifiable contract first.

For this project, the initial database spec only needed:

- tables and invariants;
- transaction boundaries;
- RLS model;
- lifecycle interfaces; and
- named acceptance tests.

Migration checksum policy, broad operational guidance, and exhaustive prose
could have waited until implementation exposed a need.

## Context strategy assessment

### Strong choices

- Attached the complete formal draft and original assignment rather than
  paraphrasing tax and failure semantics.
- Provided live endpoint and exact workspace paths.
- Corrected fixture paths immediately after moving them.
- Asked the agent to inspect current files after user/formatter changes.
- Kept architecture decisions in repository documents, reducing dependence on
  chat memory.
- Used separate component READMEs to make module contracts inspectable.

### Improvements

- Begin with a repository manifest: modules, running services, ports, data paths,
  and immutable source documents.
- Rank sources of truth explicitly.
- State whether the agent may change sibling modules before implementation.
- Define acceptance commands before code generation.
- Use one prompt per phase with a completion gate rather than progressively
  appending constraints.
- Start a fresh session or compact after each major module. The local index shows
  13 substantive prompts over roughly 106 minutes of the indexed planning
  portion; the reported three-hour session was long enough for context drift and
  repeated file rediscovery to become material risks.

## Result quality versus assignment completeness

### Engineering result: A-

The implementation demonstrates the qualities most relevant to a Readiness
Engineer interview:

- explicit failure semantics;
- durable state machines;
- idempotent identities;
- cross-tenant controls;
- real integration tests;
- process-level failure recovery;
- measured performance; and
- willingness to fix architectural hot paths instead of hiding them behind
  larger timeouts.

This would likely produce a strong technical discussion in an interview.

### Strict assignment completion: B+

The root README correctly avoids claiming perfect completion. Remaining evidence
or functionality includes:

1. process-killed importer tests during COPY and merge;
2. measured 1x/4x RSS acceptance;
3. complete table checksums across import replay;
4. cross-firm update and foreign-key-link tests;
5. the full monetary boundary matrix;
6. asserted two-firm rolling-window rate tests across retries/restarts;
7. exhaustive status-precedence tests;
8. a web payment-explanation route;
9. automated status-p95 and memory artifacts; and
10. automated dependency-license policy checks.

These are mostly acceptance-evidence gaps rather than flaws in the core design,
but an evaluator can reasonably distinguish "designed for" from "proved."

## Three-hour efficiency assessment

For approximately three hours, the output is excellent. The candidate produced
more than a typical take-home prototype and tested the hardest failure path.
The time was used effectively once implementation began, especially when
performance measurements drove set-based rewrites:

- per-filing determination became one set-based insert;
- batch planning moved from tens of thousands of statements to COPY plus
  set-based transitions;
- firm status moved from N+1 queries to one aggregate query.

The main opportunity is not typing faster. It is reducing early documentation
and boundary churn so more of the three-hour window can be spent closing the
remaining explicit acceptance gaps.

## Recommended prompting playbook

Use this structure for future take-homes:

```text
Goal
- Implement the attached assignment end to end in <timebox>.

Sources of truth, in order
1. docs/README.md: immutable product requirements
2. existing service contracts and code
3. component specs we create

Existing assets
- modules and ownership
- running services and URLs
- local database/container
- fixture paths

Architecture constraints
- explicit package/module boundaries
- allowed dependencies
- data ownership

Acceptance gates
- exact commands and tests required
- performance/memory thresholds
- required process-kill scenarios
- browser/UI walkthrough

Execution rules
- implement one vertical slice first
- validate immediately after each slice
- keep a live requirement-to-test checklist
- do not claim complete without every gate
```

### Example opening prompt

> Treat `docs/README.md` as immutable product truth. We have three sibling Go
> modules: `/irs` is already implemented and running at `127.0.0.1:8081`,
> `/postgres` owns all SQL and durable lifecycle operations, and `/service` owns
> files, orchestration, HTTP, CLI, and UI. Fixtures are checked into `/data`.
> First produce a one-page requirement-to-test matrix and identify blockers.
> Then implement the thinnest complete vertical slice using real PostgreSQL and
> the live stub. Expand only after that passes. Completion requires the exact
> import-kill, worker-kill, memory, rate-window, status, performance, and browser
> tests listed in the assignment. Keep a checkpoint ledger and do not mark the
> project complete while any row lacks evidence.

## Final judgment

The candidate operates the AI like an engineering collaborator rather than an
autocomplete tool. They show strong instincts for architecture, durability,
performance, and verification, and they are willing to challenge both the model
and their own earlier assumptions. The work product is strong enough to support
a serious interview conversation.

The next level is tighter upfront convergence: establish all module boundaries,
sources of truth, and acceptance gates before asking for implementation. Doing
that would preserve the same technical quality with less churn and make the
candidate's already strong three-hour performance more repeatable.
