package lifecycle

import internal "readiness.local/postgres/internal/app"

var (
	ErrSchemaVersion     = internal.ErrSchemaVersion
	ErrConflict          = internal.ErrConflict
	ErrNotFound          = internal.ErrNotFound
	ErrInvalidTransition = internal.ErrInvalidTransition
	ErrStaleClaim        = internal.ErrStaleClaim
	ErrInvariant         = internal.ErrInvariant
)

const RequiredSchemaVersion = internal.RequiredSchemaVersion

type RulesetVersion = internal.RulesetVersion

const RulesetNEC2025V1 = internal.RulesetNEC2025V1

type PaymentMethod = internal.PaymentMethod

const (
	PaymentCheck      = internal.PaymentCheck
	PaymentACH        = internal.PaymentACH
	PaymentWire       = internal.PaymentWire
	PaymentCash       = internal.PaymentCash
	PaymentCreditCard = internal.PaymentCreditCard
	PaymentPayPal     = internal.PaymentPayPal
)

type PreflightReason = internal.PreflightReason

const (
	ReasonTINMissing    = internal.ReasonTINMissing
	ReasonTINMalformed  = internal.ReasonTINMalformed
	ReasonTINInvalid    = internal.ReasonTINInvalid
	ReasonAmountInvalid = internal.ReasonAmountInvalid
)

type ExclusionReason = internal.ExclusionReason

const (
	ExcludedPaymentProcessor  = internal.ExcludedPaymentProcessor
	ExcludedThirdPartyNetwork = internal.ExcludedThirdPartyNetwork
)

type BatchAction = internal.BatchAction

const (
	ActionSubmit       = internal.ActionSubmit
	ActionLookupByUTID = internal.ActionLookupByUTID
	ActionPollStatus   = internal.ActionPollStatus
)

type CallOperation = internal.CallOperation

const (
	OperationSubmit = internal.OperationSubmit
	OperationStatus = internal.OperationStatus
)

type CallOutcome = internal.CallOutcome

const (
	CallCompleted      = internal.CallCompleted
	CallAmbiguous      = internal.CallAmbiguous
	CallRetryableError = internal.CallRetryableError
	CallTerminalError  = internal.CallTerminalError
)

type HeadlineState = internal.HeadlineState

const (
	HeadlineNeedsAttention = internal.HeadlineNeedsAttention
	HeadlineAwaitingIRS    = internal.HeadlineAwaitingIRS
	HeadlineFullyFiled     = internal.HeadlineFullyFiled
	HeadlinePartiallyFiled = internal.HeadlinePartiallyFiled
)

type FailureCode = internal.FailureCode
type DataLifecycle = internal.DataLifecycle
type ImportLifecycle = internal.ImportLifecycle
type DeterminationLifecycle = internal.DeterminationLifecycle
type FilingLifecycle = internal.FilingLifecycle
type RateLifecycle = internal.RateLifecycle
type StatusLifecycle = internal.StatusLifecycle
type PaymentRowSource = internal.PaymentRowSource
type PaymentRow = internal.PaymentRow
type ImportDatasetCommand = internal.ImportDatasetCommand
type DatasetResult = internal.DatasetResult
type DetermineDatasetCommand = internal.DetermineDatasetCommand
type DeterminationResult = internal.DeterminationResult
type PaymentExplanationQuery = internal.PaymentExplanationQuery
type ExplainedPayment = internal.ExplainedPayment
type PaymentExplanation = internal.PaymentExplanation
type PlanBatchesCommand = internal.PlanBatchesCommand
type BatchPlanResult = internal.BatchPlanResult
type ClaimBatchCommand = internal.ClaimBatchCommand
type Claim = internal.Claim
type BatchWork = internal.BatchWork
type SubmitAccepted = internal.SubmitAccepted
type ReferenceFound = internal.ReferenceFound
type RetrySchedule = internal.RetrySchedule
type FilingOutcome = internal.FilingOutcome
type AcquireCallCommand = internal.AcquireCallCommand
type CallPermit = internal.CallPermit
type CallResult = internal.CallResult
type FirmStatusQuery = internal.FirmStatusQuery
type ClientStatusQuery = internal.ClientStatusQuery
type ExceptionsQuery = internal.ExceptionsQuery
type StatusCounts = internal.StatusCounts
type ClientStatus = internal.ClientStatus
type FirmStatus = internal.FirmStatus
type ExceptionItem = internal.ExceptionItem
type ExceptionGroup = internal.ExceptionGroup
