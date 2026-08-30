package filing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"readiness/service/internal/irsclient"

	"readiness.local/postgres/lifecycle"
)

type Lifecycle interface {
	lifecycle.FilingLifecycle
	lifecycle.RateLifecycle
}

type IRSClient interface {
	Submit(context.Context, []byte) (irsclient.SubmitResult, error)
	Status(context.Context, string, irsclient.SearchType, string) (irsclient.StatusResult, error)
}

type Backoff struct {
	Initial     time.Duration
	Maximum     time.Duration
	MaxAttempts int
	Jitter      func(time.Duration) time.Duration
}

func (backoff Backoff) Delay(attempt int) (time.Duration, bool) {
	if attempt <= 0 {
		attempt = 1
	}
	if backoff.MaxAttempts > 0 && attempt >= backoff.MaxAttempts {
		return 0, false
	}
	delay := backoff.Initial
	if delay <= 0 {
		delay = time.Second
	}
	maximum := backoff.Maximum
	if maximum <= 0 {
		maximum = 5 * time.Minute
	}
	for step := 1; step < attempt && delay < maximum; step++ {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	if backoff.Jitter != nil {
		delay += backoff.Jitter(delay)
	}
	if delay < 0 || delay > maximum {
		delay = maximum
	}
	return delay, true
}

type Worker struct {
	lifecycle     Lifecycle
	irs           IRSClient
	workerID      string
	leaseDuration time.Duration
	idleDelay     time.Duration
	backoff       Backoff
	afterIRSCall  func()
}

type Option func(*Worker)

func WithAfterIRSCallHook(hook func()) Option {
	return func(worker *Worker) { worker.afterIRSCall = hook }
}

func NewWorker(dataLifecycle Lifecycle, irs IRSClient, workerID string, leaseDuration, idleDelay time.Duration, backoff Backoff, options ...Option) (*Worker, error) {
	if dataLifecycle == nil || irs == nil || workerID == "" || leaseDuration <= 0 || idleDelay <= 0 {
		return nil, errors.New("invalid worker configuration")
	}
	worker := &Worker{lifecycle: dataLifecycle, irs: irs, workerID: workerID, leaseDuration: leaseDuration, idleDelay: idleDelay, backoff: backoff}
	for _, option := range options {
		option(worker)
	}
	return worker, nil
}

func (worker *Worker) ProcessOne(ctx context.Context, firmID string) (bool, error) {
	work, found, err := worker.lifecycle.ClaimNextBatch(ctx, lifecycle.ClaimBatchCommand{
		FirmID: firmID, WorkerID: worker.workerID, LeaseDuration: worker.leaseDuration,
	})
	if err != nil || !found {
		return found, err
	}
	if sha256.Sum256(work.CanonicalXML) != work.PayloadSHA256 {
		return true, worker.lifecycle.FailBatchInvariant(ctx, work.Claim, "PAYLOAD_HASH_MISMATCH")
	}
	switch work.NextAction {
	case lifecycle.ActionSubmit:
		return true, worker.submit(ctx, work)
	case lifecycle.ActionLookupByUTID:
		return true, worker.status(ctx, work, irsclient.SearchUTID, work.UTID)
	case lifecycle.ActionPollStatus:
		searchType, searchValue := irsclient.SearchReceiptID, work.ReceiptID
		if searchValue == "" {
			searchType, searchValue = irsclient.SearchUTID, work.UTID
		}
		return true, worker.status(ctx, work, searchType, searchValue)
	default:
		return true, worker.lifecycle.FailBatchInvariant(ctx, work.Claim, "UNKNOWN_ACTION")
	}
}

func (worker *Worker) submit(ctx context.Context, work lifecycle.BatchWork) error {
	permit, err := worker.lifecycle.AcquireCallPermit(ctx, lifecycle.AcquireCallCommand{
		FirmID: work.FirmID, BatchID: work.BatchID, Operation: lifecycle.OperationSubmit,
	})
	if err != nil {
		return err
	}
	defer permit.Close()
	result, callErr := worker.irs.Submit(ctx, work.CanonicalXML)
	if worker.afterIRSCall != nil {
		worker.afterIRSCall()
	}
	if callErr != nil {
		var protocolErr *irsclient.ProtocolError
		if errors.As(callErr, &protocolErr) {
			if err := permit.Finish(ctx, lifecycle.CallResult{Outcome: lifecycle.CallTerminalError, ErrorCode: "MALFORMED_RESPONSE"}); err != nil {
				return err
			}
			return worker.lifecycle.FailBatchInvariant(ctx, work.Claim, "MALFORMED_RESPONSE")
		}
		if err := permit.Finish(ctx, lifecycle.CallResult{Outcome: lifecycle.CallAmbiguous, ErrorCode: "TRANSPORT_ERROR"}); err != nil {
			return err
		}
		return worker.retrySubmit(ctx, work, "TRANSPORT_ERROR")
	}

	switch {
	case result.StatusCode == http.StatusOK && result.ReceiptID != "":
		if err := finishPermit(ctx, permit, result.HTTPResult, lifecycle.CallCompleted); err != nil {
			return err
		}
		return worker.lifecycle.RecordSubmitAccepted(ctx, work.Claim, lifecycle.SubmitAccepted{ReceiptID: result.ReceiptID, PollDelay: worker.delay(work.AttemptCount)})
	case result.StatusCode == http.StatusConflict && result.ErrorCode == "DUPLICATE_UTID":
		if err := finishPermit(ctx, permit, result.HTTPResult, lifecycle.CallCompleted); err != nil {
			return err
		}
		return worker.retrySubmit(ctx, work, result.ErrorCode)
	case result.StatusCode == http.StatusTooManyRequests || result.StatusCode >= 500:
		if err := finishPermit(ctx, permit, result.HTTPResult, lifecycle.CallAmbiguous); err != nil {
			return err
		}
		return worker.retrySubmit(ctx, work, result.ErrorCode)
	default:
		if err := finishPermit(ctx, permit, result.HTTPResult, lifecycle.CallTerminalError); err != nil {
			return err
		}
		return worker.lifecycle.FailBatchInvariant(ctx, work.Claim, failureOr(result.ErrorCode, "SUBMIT_TERMINAL"))
	}
}

func (worker *Worker) status(ctx context.Context, work lifecycle.BatchWork, searchType irsclient.SearchType, searchValue string) error {
	permit, err := worker.lifecycle.AcquireCallPermit(ctx, lifecycle.AcquireCallCommand{
		FirmID: work.FirmID, BatchID: work.BatchID, Operation: lifecycle.OperationStatus,
	})
	if err != nil {
		return err
	}
	defer permit.Close()
	result, callErr := worker.irs.Status(ctx, work.FirmID, searchType, searchValue)
	if worker.afterIRSCall != nil {
		worker.afterIRSCall()
	}
	if callErr != nil {
		var protocolErr *irsclient.ProtocolError
		if errors.As(callErr, &protocolErr) {
			if err := permit.Finish(ctx, lifecycle.CallResult{Outcome: lifecycle.CallTerminalError, ErrorCode: "MALFORMED_RESPONSE"}); err != nil {
				return err
			}
			return worker.lifecycle.FailBatchInvariant(ctx, work.Claim, "MALFORMED_RESPONSE")
		}
		if err := permit.Finish(ctx, lifecycle.CallResult{Outcome: lifecycle.CallRetryableError, ErrorCode: "TRANSPORT_ERROR"}); err != nil {
			return err
		}
		return worker.retryStatus(ctx, work, "TRANSPORT_ERROR")
	}
	if result.StatusCode == http.StatusTooManyRequests || result.StatusCode >= 500 {
		if err := finishPermit(ctx, permit, result.HTTPResult, lifecycle.CallRetryableError); err != nil {
			return err
		}
		return worker.retryStatus(ctx, work, failureOr(result.ErrorCode, "STATUS_UNAVAILABLE"))
	}
	if result.StatusCode != http.StatusOK {
		if err := finishPermit(ctx, permit, result.HTTPResult, lifecycle.CallCompleted); err != nil {
			return err
		}
		if searchType == irsclient.SearchUTID && result.StatusCode == http.StatusNotFound && result.ErrorCode == "NOT_FOUND" {
			return worker.lifecycle.RecordReferenceNotFound(ctx, work.Claim, lifecycle.RetrySchedule{Delay: worker.delay(work.AttemptCount), FailureCode: result.ErrorCode})
		}
		return worker.lifecycle.FailBatchInvariant(ctx, work.Claim, failureOr(result.ErrorCode, "STATUS_TERMINAL"))
	}
	if result.UTID != work.UTID || (work.ReceiptID != "" && result.ReceiptID != work.ReceiptID) {
		if err := finishPermit(ctx, permit, result.HTTPResult, lifecycle.CallTerminalError); err != nil {
			return err
		}
		return worker.lifecycle.FailBatchInvariant(ctx, work.Claim, "REMOTE_IDENTITY_MISMATCH")
	}
	if err := finishPermit(ctx, permit, result.HTTPResult, lifecycle.CallCompleted); err != nil {
		return err
	}
	if searchType == irsclient.SearchUTID {
		return worker.lifecycle.RecordReferenceFound(ctx, work.Claim, lifecycle.ReferenceFound{ReceiptID: result.ReceiptID, PollDelay: worker.delay(work.AttemptCount)})
	}
	if result.Status == "Processing" {
		return worker.lifecycle.RecordAcknowledgmentPending(ctx, work.Claim, lifecycle.RetrySchedule{Delay: worker.delay(work.AttemptCount)})
	}
	return worker.lifecycle.CompleteAcknowledgment(ctx, work.Claim, result.Outcomes)
}

func (worker *Worker) retrySubmit(ctx context.Context, work lifecycle.BatchWork, code lifecycle.FailureCode) error {
	delay, retry := worker.backoff.Delay(work.AttemptCount)
	if !retry {
		return worker.lifecycle.RecordRetryExhausted(ctx, work.Claim, failureOr(code, "SUBMIT_RETRY_EXHAUSTED"))
	}
	return worker.lifecycle.RecordSubmitUnknown(ctx, work.Claim, lifecycle.RetrySchedule{Delay: delay, FailureCode: failureOr(code, "SUBMIT_UNKNOWN")})
}

func (worker *Worker) retryStatus(ctx context.Context, work lifecycle.BatchWork, code lifecycle.FailureCode) error {
	delay, retry := worker.backoff.Delay(work.AttemptCount)
	if !retry {
		return worker.lifecycle.RecordRetryExhausted(ctx, work.Claim, failureOr(code, "STATUS_RETRY_EXHAUSTED"))
	}
	return worker.lifecycle.RecordStatusUnavailable(ctx, work.Claim, lifecycle.RetrySchedule{Delay: delay, FailureCode: failureOr(code, "STATUS_UNAVAILABLE")})
}

func (worker *Worker) delay(attempt int) time.Duration {
	delay, retry := worker.backoff.Delay(attempt)
	if !retry {
		return worker.backoff.Maximum
	}
	return delay
}

func finishPermit(ctx context.Context, permit lifecycle.CallPermit, result irsclient.HTTPResult, outcome lifecycle.CallOutcome) error {
	return permit.Finish(ctx, lifecycle.CallResult{Outcome: outcome, HTTPStatus: result.StatusCode, ErrorCode: result.ErrorCode})
}

func failureOr(code lifecycle.FailureCode, fallback lifecycle.FailureCode) lifecycle.FailureCode {
	if code == "" {
		return fallback
	}
	return code
}

func (worker *Worker) Run(ctx context.Context, firmIDs []string) error {
	if len(firmIDs) == 0 {
		return errors.New("worker requires at least one firm")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsChannel := make(chan error, len(firmIDs))
	var waitGroup sync.WaitGroup
	for _, firmID := range firmIDs {
		firmID := firmID
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for {
				worked, err := worker.ProcessOne(runCtx, firmID)
				if err != nil {
					errorsChannel <- fmt.Errorf("firm %s: %w", firmID, err)
					cancel()
					return
				}
				if worked {
					continue
				}
				timer := time.NewTimer(worker.idleDelay)
				select {
				case <-runCtx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return
				case <-timer.C:
				}
			}
		}()
	}
	waitGroup.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		return err
	}
	return nil
}
