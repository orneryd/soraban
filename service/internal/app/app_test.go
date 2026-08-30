package app

import (
	"context"
	"errors"
	"testing"

	"readiness.local/postgres/lifecycle"
)

type fakeLifecycle struct {
	determineCalls int
	planCalls      int
}

func (fake *fakeLifecycle) ImportDataset(context.Context, lifecycle.ImportDatasetCommand, lifecycle.PaymentRowSource) (lifecycle.DatasetResult, error) {
	return lifecycle.DatasetResult{}, errors.New("unused")
}
func (fake *fakeLifecycle) DetermineDataset(_ context.Context, command lifecycle.DetermineDatasetCommand) (lifecycle.DeterminationResult, error) {
	fake.determineCalls++
	if command.RulesetVersion != lifecycle.RulesetNEC2025V1 {
		return lifecycle.DeterminationResult{}, errors.New("wrong ruleset")
	}
	return lifecycle.DeterminationResult{DeterminationID: 7}, nil
}
func (fake *fakeLifecycle) PaymentExplanation(context.Context, lifecycle.PaymentExplanationQuery) (lifecycle.PaymentExplanation, error) {
	return lifecycle.PaymentExplanation{}, nil
}
func (fake *fakeLifecycle) PlanBatches(_ context.Context, command lifecycle.PlanBatchesCommand) (lifecycle.BatchPlanResult, error) {
	fake.planCalls++
	if command.DeterminationID != 7 {
		return lifecycle.BatchPlanResult{}, errors.New("wrong determination")
	}
	return lifecycle.BatchPlanResult{CreatedBatchCount: 2}, nil
}
func (fake *fakeLifecycle) ClaimNextBatch(context.Context, lifecycle.ClaimBatchCommand) (lifecycle.BatchWork, bool, error) {
	return lifecycle.BatchWork{}, false, nil
}
func (fake *fakeLifecycle) RecordSubmitAccepted(context.Context, lifecycle.Claim, lifecycle.SubmitAccepted) error {
	return nil
}
func (fake *fakeLifecycle) RecordSubmitUnknown(context.Context, lifecycle.Claim, lifecycle.RetrySchedule) error {
	return nil
}
func (fake *fakeLifecycle) RecordReferenceFound(context.Context, lifecycle.Claim, lifecycle.ReferenceFound) error {
	return nil
}
func (fake *fakeLifecycle) RecordReferenceNotFound(context.Context, lifecycle.Claim, lifecycle.RetrySchedule) error {
	return nil
}
func (fake *fakeLifecycle) RecordAcknowledgmentPending(context.Context, lifecycle.Claim, lifecycle.RetrySchedule) error {
	return nil
}
func (fake *fakeLifecycle) RecordStatusUnavailable(context.Context, lifecycle.Claim, lifecycle.RetrySchedule) error {
	return nil
}
func (fake *fakeLifecycle) CompleteAcknowledgment(context.Context, lifecycle.Claim, []lifecycle.FilingOutcome) error {
	return nil
}
func (fake *fakeLifecycle) RecordRetryExhausted(context.Context, lifecycle.Claim, lifecycle.FailureCode) error {
	return nil
}
func (fake *fakeLifecycle) FailBatchInvariant(context.Context, lifecycle.Claim, lifecycle.FailureCode) error {
	return nil
}
func (fake *fakeLifecycle) FirmStatus(context.Context, lifecycle.FirmStatusQuery) (lifecycle.FirmStatus, error) {
	return lifecycle.FirmStatus{}, nil
}
func (fake *fakeLifecycle) ClientStatus(context.Context, lifecycle.ClientStatusQuery) (lifecycle.ClientStatus, error) {
	return lifecycle.ClientStatus{}, nil
}
func (fake *fakeLifecycle) Exceptions(context.Context, lifecycle.ExceptionsQuery) ([]lifecycle.ExceptionGroup, error) {
	return nil, nil
}

func TestDetermineAndPlanOrder(t *testing.T) {
	fake := &fakeLifecycle{}
	service := New(fake)
	determination, plan, err := service.DetermineAndPlan(context.Background(), "F001", 3)
	if err != nil {
		t.Fatal(err)
	}
	if determination.DeterminationID != 7 || plan.CreatedBatchCount != 2 || fake.determineCalls != 1 || fake.planCalls != 1 {
		t.Fatalf("unexpected orchestration: determination=%+v plan=%+v calls=%d/%d", determination, plan, fake.determineCalls, fake.planCalls)
	}
}
