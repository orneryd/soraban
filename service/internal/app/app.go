package app

import (
	"context"
	"fmt"

	"readiness/service/internal/importer"

	"readiness.local/postgres/lifecycle"
)

type Lifecycle interface {
	lifecycle.ImportLifecycle
	lifecycle.DeterminationLifecycle
	lifecycle.FilingLifecycle
	lifecycle.StatusLifecycle
}

type Service struct {
	lifecycle Lifecycle
}

func New(dataLifecycle Lifecycle) *Service {
	return &Service{lifecycle: dataLifecycle}
}

func (service *Service) Import(ctx context.Context, firmID string, taxYear int, inputPath string) (lifecycle.DatasetResult, error) {
	filePath, err := importer.Discover(inputPath, firmID)
	if err != nil {
		return lifecycle.DatasetResult{}, err
	}
	source, err := importer.Open(filePath)
	if err != nil {
		return lifecycle.DatasetResult{}, err
	}
	defer source.Close()
	result, err := service.lifecycle.ImportDataset(ctx, lifecycle.ImportDatasetCommand{FirmID: firmID, TaxYear: taxYear}, source)
	if err != nil {
		return lifecycle.DatasetResult{}, fmt.Errorf("import dataset: %w", err)
	}
	return result, nil
}

func (service *Service) Determine(ctx context.Context, firmID string, datasetID int64) (lifecycle.DeterminationResult, error) {
	result, err := service.lifecycle.DetermineDataset(ctx, lifecycle.DetermineDatasetCommand{
		FirmID: firmID, DatasetID: datasetID, RulesetVersion: lifecycle.RulesetNEC2025V1,
	})
	if err != nil {
		return lifecycle.DeterminationResult{}, fmt.Errorf("determine dataset: %w", err)
	}
	return result, nil
}

func (service *Service) Plan(ctx context.Context, firmID string, determinationID int64) (lifecycle.BatchPlanResult, error) {
	result, err := service.lifecycle.PlanBatches(ctx, lifecycle.PlanBatchesCommand{FirmID: firmID, DeterminationID: determinationID})
	if err != nil {
		return lifecycle.BatchPlanResult{}, fmt.Errorf("plan batches: %w", err)
	}
	return result, nil
}

func (service *Service) DetermineAndPlan(ctx context.Context, firmID string, datasetID int64) (lifecycle.DeterminationResult, lifecycle.BatchPlanResult, error) {
	determination, err := service.Determine(ctx, firmID, datasetID)
	if err != nil {
		return lifecycle.DeterminationResult{}, lifecycle.BatchPlanResult{}, err
	}
	plan, err := service.Plan(ctx, firmID, determination.DeterminationID)
	if err != nil {
		return lifecycle.DeterminationResult{}, lifecycle.BatchPlanResult{}, err
	}
	return determination, plan, nil
}

func (service *Service) FirmStatus(ctx context.Context, firmID string) (lifecycle.FirmStatus, error) {
	return service.lifecycle.FirmStatus(ctx, lifecycle.FirmStatusQuery{FirmID: firmID})
}

func (service *Service) ClientStatus(ctx context.Context, firmID, clientID string) (lifecycle.ClientStatus, error) {
	return service.lifecycle.ClientStatus(ctx, lifecycle.ClientStatusQuery{FirmID: firmID, ClientID: clientID})
}

func (service *Service) Exceptions(ctx context.Context, firmID, clientID string) ([]lifecycle.ExceptionGroup, error) {
	return service.lifecycle.Exceptions(ctx, lifecycle.ExceptionsQuery{FirmID: firmID, ClientID: clientID})
}

func (service *Service) PaymentExplanation(ctx context.Context, query lifecycle.PaymentExplanationQuery) (lifecycle.PaymentExplanation, error) {
	return service.lifecycle.PaymentExplanation(ctx, query)
}
