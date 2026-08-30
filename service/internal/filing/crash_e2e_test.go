package filing_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"readiness/service/internal/filing"
	"readiness/service/internal/irsclient"

	"readiness.local/postgres/lifecycle"
	postgresstore "readiness.local/postgres/store"
)

type sliceSource struct {
	rows   []lifecycle.PaymentRow
	index  int
	digest [32]byte
}

func (source *sliceSource) Next(context.Context) (lifecycle.PaymentRow, bool, error) {
	if source.index == len(source.rows) {
		return lifecycle.PaymentRow{}, false, nil
	}
	row := source.rows[source.index]
	source.index++
	return row, true, nil
}

func (source *sliceSource) SHA256() ([32]byte, error) { return source.digest, nil }

func TestCrashWorkerHelper(t *testing.T) {
	if os.Getenv("READINESS_CRASH_HELPER") != "1" {
		t.Skip("helper process only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := postgresstore.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		os.Exit(91)
	}
	defer database.Close()
	client, err := irsclient.New(irsclient.Config{
		BaseURL: os.Getenv("IRS_BASE_URL"), BearerToken: "local-irs-token",
		ConnectTimeout: time.Second, ResponseHeaderTimeout: 3 * time.Second, TotalTimeout: 5 * time.Second,
	})
	if err != nil {
		os.Exit(92)
	}
	worker, err := filing.NewWorker(database, client, "crash-worker", time.Second, 50*time.Millisecond,
		filing.Backoff{Initial: 50 * time.Millisecond, Maximum: 250 * time.Millisecond, MaxAttempts: 100},
		filing.WithAfterIRSCallHook(func() { os.Exit(99) }),
	)
	if err != nil {
		os.Exit(93)
	}
	if _, err := worker.ProcessOne(ctx, os.Getenv("TEST_FIRM_ID")); err != nil {
		os.Exit(94)
	}
	os.Exit(95)
}

func TestProcessCrashAfterRemoteCommitE2E(t *testing.T) {
	if os.Getenv("RUN_CRASH_SERVICE_E2E") != "1" {
		t.Skip("set RUN_CRASH_SERVICE_E2E=1 to run process-crash E2E")
	}
	root := crashRepositoryRoot(t)
	stubBinary := filepath.Join(t.TempDir(), "irsstub")
	build := exec.Command("go", "build", "-o", stubBinary, "./irs/cmd/irsstub")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build IRS stub: %v: %s", err, output)
	}
	var stubOutput bytes.Buffer
	stub := exec.Command(stubBinary)
	stub.Env = append(os.Environ(),
		"IRS_STUB_ADDR=127.0.0.1:18081",
		"IRS_STUB_FAIL_BEFORE_RECORD_PERCENT=0",
		"IRS_STUB_FAIL_AFTER_RECORD_PERCENT=100",
		"IRS_STUB_NEVER_ACK_PERCENT=0",
	)
	stub.Stdout, stub.Stderr = &stubOutput, &stubOutput
	if err := stub.Start(); err != nil {
		t.Fatalf("start IRS stub: %v", err)
	}
	t.Cleanup(func() {
		if stub.Process != nil {
			_ = stub.Process.Kill()
		}
		_ = stub.Wait()
	})
	waitForHealth(t, "http://127.0.0.1:18081/healthz", &stubOutput)

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = defaultDatabaseURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	database, err := postgresstore.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	firmID := fmt.Sprintf("CRASH%d", time.Now().UnixNano())
	rows := make([]lifecycle.PaymentRow, 101)
	for index := range rows {
		rows[index] = lifecycle.PaymentRow{
			SourceRowNumber: int64(index + 1), ClientID: "C-CRASH",
			VendorName: fmt.Sprintf("Crash Vendor %03d", index), VendorTIN: fmt.Sprintf("%09d", 800000000+index),
			PaymentDate: "2025-06-15", Amount: "600.00", PaymentMethod: "check", BackupWithholding: "0.00",
		}
	}
	dataset, err := database.ImportDataset(ctx, lifecycle.ImportDatasetCommand{FirmID: firmID, TaxYear: 2025}, &sliceSource{rows: rows, digest: sha256.Sum256([]byte(firmID))})
	if err != nil {
		t.Fatal(err)
	}
	determination, err := database.DetermineDataset(ctx, lifecycle.DetermineDatasetCommand{FirmID: firmID, DatasetID: dataset.DatasetID, RulesetVersion: lifecycle.RulesetNEC2025V1})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := database.PlanBatches(ctx, lifecycle.PlanBatchesCommand{FirmID: firmID, DeterminationID: determination.DeterminationID})
	if err != nil || plan.CreatedBatchCount != 2 {
		t.Fatalf("plan two batches: %+v, %v", plan, err)
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestCrashWorkerHelper$")
	helper.Env = append(os.Environ(),
		"READINESS_CRASH_HELPER=1",
		"DATABASE_URL="+databaseURL,
		"IRS_BASE_URL=http://127.0.0.1:18081",
		"TEST_FIRM_ID="+firmID,
	)
	helperOutput, err := helper.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 99 {
		t.Fatalf("worker did not exit at crash hook: %v: %s", err, helperOutput)
	}

	client, err := irsclient.New(irsclient.Config{
		BaseURL: "http://127.0.0.1:18081", BearerToken: "local-irs-token",
		ConnectTimeout: time.Second, ResponseHeaderTimeout: 3 * time.Second, TotalTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filing.NewWorker(database, client, "recovery-worker", 90*time.Second, 50*time.Millisecond,
		filing.Backoff{Initial: 50 * time.Millisecond, Maximum: 250 * time.Millisecond, MaxAttempts: 100})
	if err != nil {
		t.Fatal(err)
	}
	for {
		worked, err := worker.ProcessOne(ctx, firmID)
		if err != nil {
			t.Fatalf("recovery worker: %v\nstub output:\n%s", err, stubOutput.String())
		}
		status, err := database.ClientStatus(ctx, lifecycle.ClientStatusQuery{FirmID: firmID, ClientID: "C-CRASH"})
		if err != nil {
			t.Fatal(err)
		}
		if status.Headline == lifecycle.HeadlineFullyFiled && status.Counts.Accepted == 101 {
			return
		}
		if !worked {
			select {
			case <-ctx.Done():
				t.Fatalf("recovery timed out: %v\nstub output:\n%s", ctx.Err(), stubOutput.String())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

func crashRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate crash test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func waitForHealth(t *testing.T, url string, output *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("IRS stub did not become healthy: %s", output.String())
}
