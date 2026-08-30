package app

import (
	"context"
	"errors"
	"time"
)

var (
	ErrSchemaVersion     = errors.New("schema version mismatch")
	ErrConflict          = errors.New("immutable input conflict")
	ErrNotFound          = errors.New("durable identity not found")
	ErrInvalidTransition = errors.New("invalid lifecycle transition")
	ErrStaleClaim        = errors.New("stale batch claim")
	ErrInvariant         = errors.New("durable invariant violated")
)

const RequiredSchemaVersion int64 = 1

type RulesetVersion string

const RulesetNEC2025V1 RulesetVersion = "nec-2025-v1"

type PaymentMethod string

const (
	PaymentCheck      PaymentMethod = "check"
	PaymentACH        PaymentMethod = "ach"
	PaymentWire       PaymentMethod = "wire"
	PaymentCash       PaymentMethod = "cash"
	PaymentCreditCard PaymentMethod = "credit_card"
	PaymentPayPal     PaymentMethod = "paypal"
)

type PreflightReason string

const (
	ReasonTINMissing    PreflightReason = "TIN_MISSING"
	ReasonTINMalformed  PreflightReason = "TIN_MALFORMED"
	ReasonTINInvalid    PreflightReason = "TIN_INVALID"
	ReasonAmountInvalid PreflightReason = "AMOUNT_INVALID"
)

type ExclusionReason string

const (
	ExcludedPaymentProcessor  ExclusionReason = "PAYMENT_PROCESSOR_REPORTED"
	ExcludedThirdPartyNetwork ExclusionReason = "THIRD_PARTY_NETWORK_REPORTED"
)

type BatchAction string

const (
	ActionSubmit       BatchAction = "submit"
	ActionLookupByUTID BatchAction = "lookup_by_utid"
	ActionPollStatus   BatchAction = "poll_status"
)

type CallOperation string

const (
	OperationSubmit CallOperation = "submit"
	OperationStatus CallOperation = "status"
)

type CallOutcome string

const (
	CallCompleted      CallOutcome = "completed"
	CallAmbiguous      CallOutcome = "ambiguous"
	CallRetryableError CallOutcome = "retryable_error"
	CallTerminalError  CallOutcome = "terminal_error"
)

type HeadlineState string

const (
	HeadlineNeedsAttention HeadlineState = "needs_attention"
	HeadlineAwaitingIRS    HeadlineState = "awaiting_the_irs"
	HeadlineFullyFiled     HeadlineState = "fully_filed"
	HeadlinePartiallyFiled HeadlineState = "partially_filed"
)

type FailureCode string

type DataLifecycle interface {
	ImportLifecycle
	DeterminationLifecycle
	FilingLifecycle
	RateLifecycle
	StatusLifecycle
}

type ImportLifecycle interface {
	ImportDataset(context.Context, ImportDatasetCommand, PaymentRowSource) (DatasetResult, error)
}

type DeterminationLifecycle interface {
	DetermineDataset(context.Context, DetermineDatasetCommand) (DeterminationResult, error)
	PaymentExplanation(context.Context, PaymentExplanationQuery) (PaymentExplanation, error)
}

type FilingLifecycle interface {
	PlanBatches(context.Context, PlanBatchesCommand) (BatchPlanResult, error)
	ClaimNextBatch(context.Context, ClaimBatchCommand) (BatchWork, bool, error)
	RecordSubmitAccepted(context.Context, Claim, SubmitAccepted) error
	RecordSubmitUnknown(context.Context, Claim, RetrySchedule) error
	RecordReferenceFound(context.Context, Claim, ReferenceFound) error
	RecordReferenceNotFound(context.Context, Claim, RetrySchedule) error
	RecordAcknowledgmentPending(context.Context, Claim, RetrySchedule) error
	RecordStatusUnavailable(context.Context, Claim, RetrySchedule) error
	CompleteAcknowledgment(context.Context, Claim, []FilingOutcome) error
	RecordRetryExhausted(context.Context, Claim, FailureCode) error
	FailBatchInvariant(context.Context, Claim, FailureCode) error
}

type RateLifecycle interface {
	AcquireCallPermit(context.Context, AcquireCallCommand) (CallPermit, error)
}

type StatusLifecycle interface {
	FirmStatus(context.Context, FirmStatusQuery) (FirmStatus, error)
	ClientStatus(context.Context, ClientStatusQuery) (ClientStatus, error)
	Exceptions(context.Context, ExceptionsQuery) ([]ExceptionGroup, error)
}

type PaymentRowSource interface {
	Next(context.Context) (PaymentRow, bool, error)
	SHA256() ([32]byte, error)
}

type PaymentRow struct {
	SourceRowNumber   int64
	ClientID          string
	VendorName        string
	VendorTIN         string
	PaymentDate       string
	Amount            string
	PaymentMethod     string
	BackupWithholding string
	Memo              string
}

type ImportDatasetCommand struct {
	FirmID  string
	TaxYear int
}

type DatasetResult struct {
	DatasetID     int64
	ContentSHA256 [32]byte
	RowCount      int64
	Existing      bool
}

type DetermineDatasetCommand struct {
	FirmID         string
	DatasetID      int64
	RulesetVersion RulesetVersion
}

type DeterminationResult struct {
	DeterminationID int64
	ReadyCount      int64
	BlockedCount    int64
	Existing        bool
}

type PaymentExplanationQuery struct {
	FirmID          string
	DeterminationID int64
	ClientID        string
	VendorIdentity  string
}

type ExplainedPayment struct {
	SourceRowNumber  int64
	VendorName       string
	PaymentDate      time.Time
	AmountCents      int64
	PaymentMethod    PaymentMethod
	WithholdingCents int64
	Counted          bool
	ExclusionReason  ExclusionReason
}

type PaymentExplanation struct {
	Payments         []ExplainedPayment
	ReportableCents  int64
	WithholdingCents int64
}

type PlanBatchesCommand struct {
	FirmID          string
	DeterminationID int64
}

type BatchPlanResult struct {
	ExistingBatchCount int64
	CreatedBatchCount  int64
}

type ClaimBatchCommand struct {
	FirmID        string
	WorkerID      string
	LeaseDuration time.Duration
}

type Claim struct {
	batchID int64
	firmID  string
	token   string
}

// NewStoreClaim and StoreClaimParts are adapter bridge functions. Business
// orchestration carries Claim values but has no reason to call either one.
func NewStoreClaim(batchID int64, firmID, token string) Claim {
	return Claim{batchID: batchID, firmID: firmID, token: token}
}

func StoreClaimParts(claim Claim) (batchID int64, firmID, token string) {
	return claim.batchID, claim.firmID, claim.token
}

type BatchWork struct {
	Claim         Claim
	BatchID       int64
	FirmID        string
	ClientID      string
	TaxYear       int
	AttemptCount  int
	NextAction    BatchAction
	UTID          string
	CanonicalXML  []byte
	PayloadSHA256 [32]byte
	ReceiptID     string
}

type SubmitAccepted struct {
	ReceiptID string
	PollDelay time.Duration
}

type ReferenceFound struct {
	ReceiptID string
	PollDelay time.Duration
}

type RetrySchedule struct {
	Delay       time.Duration
	FailureCode FailureCode
}

type FilingOutcome struct {
	FilingKey       string
	IRSRecordID     string
	RejectionReason PreflightReason
}

type AcquireCallCommand struct {
	FirmID    string
	BatchID   int64
	Operation CallOperation
}

type CallPermit interface {
	Finish(context.Context, CallResult) error
	Close() error
}

type CallResult struct {
	Outcome    CallOutcome
	HTTPStatus int
	ErrorCode  FailureCode
}

type FirmStatusQuery struct {
	FirmID string
}

type ClientStatusQuery struct {
	FirmID   string
	ClientID string
}

type ExceptionsQuery struct {
	FirmID   string
	ClientID string
}

type StatusCounts struct {
	Required int64
	Blocked  int64
	Ready    int64
	Pending  int64
	Accepted int64
	Rejected int64
}

type ClientStatus struct {
	FirmID   string
	ClientID string
	Counts   StatusCounts
	Headline HeadlineState
}

type FirmStatus struct {
	FirmID   string
	Counts   StatusCounts
	Headline HeadlineState
	Clients  []ClientStatus
}

type ExceptionItem struct {
	ClientID          string
	FilingID          int64
	BatchID           int64
	VendorDisplayName string
	FailureCode       FailureCode
}

type ExceptionGroup struct {
	Type  string
	Count int64
	Items []ExceptionItem
}
