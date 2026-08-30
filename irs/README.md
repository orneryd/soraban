# IRS Stub Service Specification

Status: Draft for implementation
Date: 2026-08-30
Scope: Standalone local service only

## 1. Purpose

This service is a deliberately small, in-memory simulation of the IRS
Information Returns Intake System (IRIS) Application-to-Application (A2A)
channel. It exists to test the filing client's behavior under delayed results,
ambiguous failures, retries, and process crashes.

The stub MUST run as a separate Go process and communicate only over HTTP. It
MUST NOT import application packages, connect to the application database, or
know the application's job state. The application is out of scope here.

The stub is ephemeral by design. Restarting it clears submissions, call history,
and idempotency state. Therefore, tests of application crash recovery MUST keep
the stub process alive. An in-memory stub cannot model IRS durability across a
stub restart, and no such claim is made.

## 2. Source contract and deliberate differences

The closest public IRS interface is IRIS A2A for processing year 2026 and tax
year 2025. This profile follows public IRS Publication 5718 where it does not
conflict with the exercise requirements:

| Concern | Public IRIS A2A | Stub profile |
| --- | --- | --- |
| Intake | `POST /IRIntakeAcceptanceA2A/1.0/irisa2a/v1/intake-acceptance` | Same path and method |
| Intake media | `multipart/form-data`; XML file part named `file` | Same |
| Status | `POST /IRIntakeAcceptanceA2A/1.0/iris/transstatusorack` | Same path and method |
| Status media | `application/xml` | Same |
| Authentication | OAuth 2.0 bearer token created from signed JWT grants | Presence/equality check against one configured bearer token |
| Caller reference | Unique Transmission Identifier (`UTID`) | `UTID` is the required stable reference |
| Server identifier | Opaque `ReceiptId` | `ReceiptId` is the exercise's submission ID |
| Status lookup | `ReceiptId` or `UTID` | Same |
| Payload | IRS annual XML schema package | Small project-owned XML profile below |
| Capacity | Up to 100 MB and potentially multiple submissions | Exactly one submission, one client, and 1-100 records |
| Successful intake | Receipt ID, UTID, and timestamp | Only `ReceiptId`, as required by the exercise |
| Acknowledgment | Transmission/submission statuses and error information | IRIS status plus required per-record outcomes |

The official annual XML schemas and business rules are distributed through the
IRS Secure Object Repository only to holders of an IRIS TCC. This repository
MUST NOT claim that its project-owned XML is an official IRS schema.

Authoritative sources:

- [IRS: E-file information returns with IRIS](https://www.irs.gov/filing/e-file-information-returns-with-iris)
- [IRS Publication 5718: IRIS A2A Specifications](https://www.irs.gov/pub/irs-pdf/p5718.pdf)
- [IRS: IRIS schemas and business rules](https://www.irs.gov/e-file-providers/iris-schemas-and-business-rules)
- [IRS: IRIS Assurance Testing System](https://www.irs.gov/e-file-providers/iris-assurance-testing-system-ats)

## 3. Runtime and licensing

The implementation MUST use Go's standard library only: `net/http`,
`encoding/xml`, `mime/multipart`, `sync`, `time`, and standard random/crypto
packages. It MUST have no runtime service, database, queue, SDK, or third-party
module. Go's standard library is BSD-3-Clause licensed and is compatible with an
MIT-licensed project.

One binary is sufficient:

```text
project/irs/
  README.md
  openapi.yaml
  go.mod
  cmd/irsstub/main.go
  internal/stub/config.go
  internal/stub/model.go
  internal/stub/server.go
  internal/stub/server_test.go
```

Package boundaries MAY be collapsed if the implementation remains readable.
No generic repository, service, or dependency-injection framework is allowed.

### Run locally

From `project/irs`:

```sh
go test -race ./...
go run ./cmd/irsstub
```

The server listens on `http://127.0.0.1:8081` by default. Verify it in another
terminal:

```sh
curl --fail http://127.0.0.1:8081/healthz
```

Submit an XML file conforming to Section 5:

```sh
curl --include \
  --header 'Authorization: Bearer local-irs-token' \
  --header 'Accept: application/xml' \
  --form 'file=@transmission.xml;type=text/xml' \
  http://127.0.0.1:8081/IRIntakeAcceptanceA2A/1.0/irisa2a/v1/intake-acceptance
```

Set either failure percentage to `100` with the other set to `0` to exercise a
specific ambiguous-failure path.

## 4. Configuration

Configuration is read once from environment variables at startup. Invalid
values MUST fail startup with a useful error. Percentage values are decimal
numbers from `0` through `100`, inclusive.

| Variable | Default | Meaning |
| --- | ---: | --- |
| `IRS_STUB_ADDR` | `127.0.0.1:8081` | Listen address |
| `IRS_STUB_BEARER_TOKEN` | `local-irs-token` | Required bearer token |
| `IRS_STUB_FAIL_BEFORE_RECORD_PERCENT` | `7` | Intake fails before any submission is recorded |
| `IRS_STUB_FAIL_AFTER_RECORD_PERCENT` | `5` | Intake is fully recorded, then returns the same failure |
| `IRS_STUB_NEVER_ACK_PERCENT` | `0` | Recorded submissions whose acknowledgment never becomes available |

`FAIL_BEFORE_RECORD_PERCENT + FAIL_AFTER_RECORD_PERCENT` MUST be at most `100`.
Each failure variable can therefore be set to `100` when the other is `0`:
before-record at `100` always leaves no submission; after-record at `100`
always records and then returns an error. `NEVER_ACK_PERCENT` is an independent
draw made only after a submission is recorded.

## 5. Exercise XML profile

One HTTP intake request represents one IRIS transmission containing one
submission for one firm/client and 1-100 Form 1099-NEC records.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<IRTransmission>
  <IRTransmissionManifest>
    <UniqueTransmissionId>550e8400-e29b-41d4-a716-446655440000:IRIS:F001::A</UniqueTransmissionId>
    <TransmitterControlCd>F001</TransmitterControlCd>
    <TransmissionTypeCd>O</TransmissionTypeCd>
    <TaxYr>2025</TaxYr>
  </IRTransmissionManifest>
  <IRSubmission1Grp>
    <IRSubmission1Header>
      <SubmissionId>SUB-C0001-0001</SubmissionId>
      <ClientId>C0001</ClientId>
      <FormTypeCd>1099-NEC</FormTypeCd>
      <ReportedRcpntFormCnt>1</ReportedRcpntFormCnt>
    </IRSubmission1Header>
    <Form1099NECDetail>
      <RecordId>filing-key-001</RecordId>
      <RecipientNm>Example Vendor</RecipientNm>
      <RecipientTIN>123456789</RecipientTIN>
      <NonemployeeCompensationAmt>600.00</NonemployeeCompensationAmt>
      <FederalIncomeTaxWithheldAmt>0.00</FederalIncomeTaxWithheldAmt>
    </Form1099NECDetail>
  </IRSubmission1Grp>
</IRTransmission>
```

The exercise firm IDs `F001` and `F002` occupy the real
`TransmitterControlCd` role even though a production IRIS TCC is five
characters. `RecipientTIN` MAY be empty so rejection behavior can be tested.
Money MUST be base-10 dollars with exactly two fractional digits and MUST fit a
signed 64-bit cent value. Floating point parsing is forbidden.

The intake MUST reject the complete request before fault injection when:

- XML is malformed or contains trailing non-whitespace data;
- the multipart request does not contain exactly one `file` part;
- the file is empty;
- `UniqueTransmissionId`, transmitter, submission, client, record, or required
  amount fields are absent;
- the UTID does not equal `<UUID>:IRIS:<TransmitterControlCd>::A`;
- `TransmissionTypeCd` is not `O`, `TaxYr` is not `2025`, or `FormTypeCd` is not
  `1099-NEC`;
- the header count differs from the number of detail records;
- there are fewer than 1 or more than 100 detail records;
- a `RecordId` is repeated within the request; or
- a money value cannot be represented as exact cents.

Unknown XML elements MUST be rejected. This keeps fixtures honest and catches
misspelled fields rather than silently ignoring them.

## 6. HTTP API

The machine-readable contract is [openapi.yaml](openapi.yaml). XML element names
and examples in this document are normative where OpenAPI cannot fully express
multipart XML constraints.

### 6.1 Submit transmission

```http
POST /IRIntakeAcceptanceA2A/1.0/irisa2a/v1/intake-acceptance
Authorization: Bearer local-irs-token
Accept: application/xml
Content-Type: multipart/form-data; boundary=...
```

The multipart form MUST contain exactly one file part named `file` with media
type `text/xml` or `application/xml`.

Success is `200 application/xml` and contains only the opaque submission ID:

```xml
<ReceiptId>rcpt_01J...</ReceiptId>
```

The service MUST NOT echo TINs or the payload in any response or log.

### 6.2 Get transmission status or acknowledgment

```http
POST /IRIntakeAcceptanceA2A/1.0/iris/transstatusorack
Authorization: Bearer local-irs-token
Accept: application/xml
Content-Type: application/xml
```

Lookup by the opaque submission ID:

```xml
<TransStatusOrAckRequest>
  <TransmitterControlCd>F001</TransmitterControlCd>
  <SearchParameterTypeCd>RECEIPTID</SearchParameterTypeCd>
  <SearchParameterTxt>rcpt_01J...</SearchParameterTxt>
</TransStatusOrAckRequest>
```

Lookup after an ambiguous intake response uses the original reference:

```xml
<TransStatusOrAckRequest>
  <TransmitterControlCd>F001</TransmitterControlCd>
  <SearchParameterTypeCd>UTID</SearchParameterTypeCd>
  <SearchParameterTxt>550e8400-e29b-41d4-a716-446655440000:IRIS:F001::A</SearchParameterTxt>
</TransStatusOrAckRequest>
```

An existing submission whose delay has not elapsed returns `200`:

```xml
<TransStatusOrAckResponse>
  <ReceiptId>rcpt_01J...</ReceiptId>
  <UniqueTransmissionId>550e8400-e29b-41d4-a716-446655440000:IRIS:F001::A</UniqueTransmissionId>
  <TransmissionStatusCd>Processing</TransmissionStatusCd>
</TransStatusOrAckResponse>
```

After acknowledgment, the same response includes exactly one result per input
record:

```xml
<TransStatusOrAckResponse>
  <ReceiptId>rcpt_01J...</ReceiptId>
  <UniqueTransmissionId>550e8400-e29b-41d4-a716-446655440000:IRIS:F001::A</UniqueTransmissionId>
  <TransmissionStatusCd>PartiallyAccepted</TransmissionStatusCd>
  <RecordResultGrp>
    <RecordId>filing-key-001</RecordId>
    <RecordStatusCd>Accepted</RecordStatusCd>
    <IRSRecordId>irsrec_01J...</IRSRecordId>
  </RecordResultGrp>
  <RecordResultGrp>
    <RecordId>filing-key-002</RecordId>
    <RecordStatusCd>Rejected</RecordStatusCd>
    <ErrorReasonCd>TIN_MISSING</ErrorReasonCd>
  </RecordResultGrp>
</TransStatusOrAckResponse>
```

Transmission status is derived as follows:

| Records | `TransmissionStatusCd` |
| --- | --- |
| Acknowledgment not available | `Processing` |
| All accepted | `Accepted` |
| Accepted and rejected | `PartiallyAccepted` |
| All rejected | `Rejected` |

`AcceptedWithErrors` is a real IRIS status but is not emitted because the
exercise defines only binary accepted/rejected record outcomes.

### 6.3 Health

`GET /healthz` returns `200 text/plain` with `ok\n`. It is not an IRS endpoint,
and does not require authentication.

## 7. Per-record acknowledgment rules

Outcomes are computed and frozen when a submission is recorded, but are hidden
until acknowledgment is available. Validation precedence is exact:

1. Blank `RecipientTIN` -> `TIN_MISSING`.
2. TIN is not exactly nine ASCII digits -> `TIN_MALFORMED`.
3. TIN begins with `000` -> `TIN_INVALID`.
4. `NonemployeeCompensationAmt` is zero or negative -> `AMOUNT_INVALID`.
5. Otherwise -> accepted with a new opaque `IRSRecordId`.

The list is exhaustive. A rejected result MUST contain one `ErrorReasonCd` and
no `IRSRecordId`. An accepted result MUST contain one `IRSRecordId` and no
`ErrorReasonCd`. Backup withholding is accepted as supplied and does not alter
these four exercise validation rules.

## 8. Failure model

Only valid, authenticated intake calls participate in random failure injection.
One random draw in `[0, 100)` selects a mutually exclusive intake outcome:

```text
[0, fail_before)                         -> fail before record
[fail_before, fail_before + fail_after) -> record, then fail
[fail_before + fail_after, 100)         -> record and return ReceiptId
```

Both injected intake failures return the same response so the caller cannot
infer whether the request committed:

```http
HTTP/1.1 503 Service Unavailable
Content-Type: application/xml
```

```xml
<ErrorResponse>
  <ErrorCd>SERVICE_UNAVAILABLE</ErrorCd>
  <ErrorMessageTxt>IRIS is temporarily unavailable.</ErrorMessageTxt>
</ErrorResponse>
```

For failure-before-record, no map is changed. For failure-after-record, the
complete submission and every frozen record outcome MUST be visible to a
subsequent UTID status lookup before the `503` is sent.

After recording, an independent random draw determines whether acknowledgment
never arrives. Otherwise, `acknowledgmentAvailableAt` is sampled uniformly over
10 through 30 seconds. A never-acknowledged submission remains `Processing`
forever and MUST remain findable.

Status calls are not randomly failed by default because the README assigns both
specified failure modes to submission calls.

## 9. In-memory state and atomicity

A single state object owns all mutable data:

```text
mutex
submissionsByFirmAndUTID map[(firm, utid)]*submission
submissionsByReceiptID   map[receiptID]*submission
originalRecords          map[(firm, client, taxYear, recordID)]recordFingerprint
randomSource
clock
```

The same mutex MUST cover random draws, duplicate checks, ID generation, and
insertion. A submission becomes visible atomically: status may observe all of
it or none of it, never a partial batch. Response writing occurs after releasing
the mutex.

The clock and random source MUST be replaceable in tests. Generated receipt/IRS
record IDs MUST be opaque, URL-safe, and collision-checked under the mutex.

### 9.1 Duplicate behavior

- A UTID is unique per firm, matching IRIS uniqueness behavior.
- Reusing an existing UTID MUST NOT create another submission and returns
  `409 DUPLICATE_UTID`; the caller must use the status operation.
- An original `RecordId` is also unique per firm/client/tax-year in this exercise.
  Reusing one under a different UTID returns `409 DUPLICATE_RECORD` and records
  nothing.
- Duplicate checks and insertion occur in one critical section, so concurrent
  requests cannot both win.

Global original-record uniqueness is an exercise safety extension, not a claim
about the real IRIS API. Corrections and replacements are out of scope.

## 10. Error contract

| HTTP | `ErrorCd` | Condition | Retry |
| ---: | --- | --- | --- |
| `400` | `INVALID_REQUEST` | Invalid multipart, XML, field, count, or money | No |
| `401` | `UNAUTHORIZED` | Missing or wrong bearer token | No |
| `404` | `NOT_FOUND` | Valid receipt ID or UTID lookup has no match | Status later or submit same UTID |
| `409` | `DUPLICATE_UTID` | Existing firm/UTID | Lookup existing UTID |
| `409` | `DUPLICATE_RECORD` | Existing original record under another UTID | No |
| `415` | `UNSUPPORTED_MEDIA_TYPE` | Wrong request/file media type | No |
| `503` | `SERVICE_UNAVAILABLE` | Injected before/after-record failure | Reconcile by UTID first |

All errors use the same `ErrorResponse` XML shape. Error messages MUST remain
stable enough for humans but callers MUST branch only on HTTP status and
`ErrorCd`.

## 11. Logging

Use `log/slog` JSON output. Log request ID, operation, firm ID, client ID,
UTID hash, receipt ID, record count, HTTP status, injected fault category, and
duration. Never log XML bodies, vendor names, TINs, raw UTIDs, or bearer tokens.

## 12. Acceptance tests

| Test | Required proof |
| --- | --- |
| `TestSubmitSuccess` | Valid multipart XML returns only one `ReceiptId` and records 1-100 items atomically. |
| `TestIRISPathsAndMediaTypes` | Both public IRIS-compatible paths and content types are enforced. |
| `TestSubmissionValidation` | Every structural rejection in Section 5 records nothing. |
| `TestRecordOutcomes` | Four rejection codes and precedence are exact; accepted records get IDs. |
| `TestFailureBeforeRecord100Percent` | With before=`100`, every valid intake returns `503` and UTID lookup is `404`. |
| `TestFailureAfterRecord100Percent` | With after=`100`, every valid intake returns `503` and UTID lookup finds one complete submission. |
| `TestDefaultFailureThresholds` | Scripted random draws verify the exact default 7% before-record and 5% after-record boundaries. |
| `TestDelayedAcknowledgment` | Fake clock returns `Processing` before the boundary and frozen results at/after it. |
| `TestNeverAcknowledges` | With never-ack=`100`, status remains `Processing` after arbitrary clock advance. |
| `TestLookupByReceiptAndUTID` | Both keys return identical state; UTID lookup recovers `ReceiptId`. |
| `TestDuplicateUTIDConcurrent` | Concurrent same-UTID calls create at most one submission. |
| `TestDuplicateRecordDifferentUTID` | Rebatching the same original record is rejected atomically. |
| `TestRace` | `go test -race ./...` passes under concurrent submit/status load. |
| `TestRestartClearsState` | A fresh server object has no submissions; documents the intentional ephemeral boundary. |

The failure-threshold test injects scripted random values.

## 13. Definition of done

The stub is ready when:

1. `go test -race ./...` passes;
2. `go vet ./...` passes;
3. the OpenAPI document parses and both operation paths match the handlers;
4. all acceptance tests above pass without sleeps or network flakiness;
5. `go list -m all` contains only the project module and Go standard library;
6. default configuration produces the required 7%/5% failure behaviors; and
7. its README explicitly continues to state that in-memory state is not durable
   across a stub restart.