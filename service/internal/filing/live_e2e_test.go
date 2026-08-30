package filing_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"readiness/service/internal/filing"
	"readiness/service/internal/irsclient"

	"readiness.local/postgres/lifecycle"
	postgresstore "readiness.local/postgres/store"
)

const defaultDatabaseURL = "postgres://readiness_app:readiness-app-local-only@127.0.0.1:55432/readiness?sslmode=disable"

type oneRowSource struct {
	row       lifecycle.PaymentRow
	delivered bool
	digest    [32]byte
}

func (source *oneRowSource) Next(context.Context) (lifecycle.PaymentRow, bool, error) {
	if source.delivered {
		return lifecycle.PaymentRow{}, false, nil
	}
	source.delivered = true
	return source.row, true, nil
}

func (source *oneRowSource) SHA256() ([32]byte, error) { return source.digest, nil }

func TestLiveServiceLifecycleE2E(t *testing.T) {
	if os.Getenv("RUN_LIVE_SERVICE_E2E") != "1" {
		t.Skip("set RUN_LIVE_SERVICE_E2E=1 to run PostgreSQL plus IRS E2E")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = defaultDatabaseURL
	}
	baseURL := os.Getenv("IRS_BASE_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8081"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := postgresstore.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	firmID := fmt.Sprintf("SVC%d", time.Now().UnixNano())
	source := &oneRowSource{
		row: lifecycle.PaymentRow{
			SourceRowNumber: 1, ClientID: "C-LIVE", VendorName: "Service Live Vendor",
			VendorTIN: "987654321", PaymentDate: "2025-06-15", Amount: "600.00",
			PaymentMethod: "check", BackupWithholding: "0.00", Memo: "",
		},
		digest: sha256.Sum256([]byte(firmID)),
	}
	dataset, err := database.ImportDataset(ctx, lifecycle.ImportDatasetCommand{FirmID: firmID, TaxYear: 2025}, source)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	determination, err := database.DetermineDataset(ctx, lifecycle.DetermineDatasetCommand{FirmID: firmID, DatasetID: dataset.DatasetID, RulesetVersion: lifecycle.RulesetNEC2025V1})
	if err != nil {
		t.Fatalf("determine: %v", err)
	}
	if _, err := database.PlanBatches(ctx, lifecycle.PlanBatchesCommand{FirmID: firmID, DeterminationID: determination.DeterminationID}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	client, err := irsclient.New(irsclient.Config{
		BaseURL: baseURL, BearerToken: "local-irs-token", ConnectTimeout: time.Second,
		ResponseHeaderTimeout: 3 * time.Second, TotalTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filing.NewWorker(database, client, "service-e2e", 15*time.Second, 100*time.Millisecond, filing.Backoff{
		Initial: 100 * time.Millisecond, Maximum: 500 * time.Millisecond, MaxAttempts: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for {
		worked, err := worker.ProcessOne(ctx, firmID)
		if err != nil {
			t.Fatalf("worker: %v", err)
		}
		status, err := database.ClientStatus(ctx, lifecycle.ClientStatusQuery{FirmID: firmID, ClientID: "C-LIVE"})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if status.Headline == lifecycle.HeadlineFullyFiled && status.Counts.Accepted == 1 {
			return
		}
		if !worked {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}
