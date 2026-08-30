package app_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	serviceapp "readiness/service/internal/app"

	"readiness.local/postgres/lifecycle"
	postgresstore "readiness.local/postgres/store"
)

const defaultDatabaseURL = "postgres://readiness_app:readiness-app-local-only@127.0.0.1:55432/readiness?sslmode=disable"

type fixture struct {
	firmID           string
	fileName         string
	compressedSHA256 string
	rowCount         int64
}

var fixtures = []fixture{
	{firmID: "F001", fileName: "firm_F001_export.csv.gz", compressedSHA256: "229ac7223dc4e316e6fed52644b793899c0f0c34b33084453e364e68ec1ea29c", rowCount: 500000},
	{firmID: "F002", fileName: "firm_F002_export.csv.gz", compressedSHA256: "09668327e446544c1e0fe1430dbe859838bfbb64ef0381c57f17ffd2d06f7e71", rowCount: 500000},
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate E2E test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
}

func verifyFixture(filePath, expected string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open fixture %s: %w", filePath, err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return fmt.Errorf("hash fixture %s: %w", filePath, err)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != expected {
		return fmt.Errorf("fixture %s checksum = %s, want %s", filePath, got, expected)
	}
	return nil
}

func TestRealDataImportDetermineAndReplayE2E(t *testing.T) {
	if os.Getenv("RUN_SERVICE_E2E") != "1" {
		t.Skip("set RUN_SERVICE_E2E=1 to consume checked-in exports")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = defaultDatabaseURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	database, err := postgresstore.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL lifecycle: %v", err)
	}
	defer database.Close()
	application := serviceapp.New(database)
	root := repositoryRoot(t)

	type result struct {
		fixture       fixture
		dataset       lifecycle.DatasetResult
		determination lifecycle.DeterminationResult
		plan          lifecycle.BatchPlanResult
		importTime    time.Duration
		determineTime time.Duration
		planTime      time.Duration
		err           error
	}
	results := make(chan result, len(fixtures))
	var waitGroup sync.WaitGroup
	for _, testFixture := range fixtures {
		testFixture := testFixture
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			filePath := filepath.Join(root, "data", testFixture.fileName)
			if verifyErr := verifyFixture(filePath, testFixture.compressedSHA256); verifyErr != nil {
				results <- result{fixture: testFixture, err: verifyErr}
				return
			}
			startedAt := time.Now()
			dataset, runErr := application.Import(ctx, testFixture.firmID, 2025, filePath)
			importTime := time.Since(startedAt)
			if runErr != nil {
				results <- result{fixture: testFixture, err: runErr}
				return
			}
			startedAt = time.Now()
			determination, runErr := application.Determine(ctx, testFixture.firmID, dataset.DatasetID)
			determineTime := time.Since(startedAt)
			if runErr != nil {
				results <- result{fixture: testFixture, dataset: dataset, err: runErr}
				return
			}
			startedAt = time.Now()
			plan, runErr := application.Plan(ctx, testFixture.firmID, determination.DeterminationID)
			results <- result{fixture: testFixture, dataset: dataset, determination: determination, plan: plan, importTime: importTime, determineTime: determineTime, planTime: time.Since(startedAt), err: runErr}
		}()
	}
	waitGroup.Wait()
	close(results)
	for got := range results {
		if got.err != nil {
			if errors.Is(got.err, lifecycle.ErrConflict) {
				t.Fatalf("fixture %s conflicts with existing database; use a clean E2E database: %v", got.fixture.firmID, got.err)
			}
			t.Fatalf("fixture %s lifecycle: %v", got.fixture.firmID, got.err)
		}
		if got.dataset.RowCount != got.fixture.rowCount {
			t.Fatalf("fixture %s row count = %d, want %d", got.fixture.firmID, got.dataset.RowCount, got.fixture.rowCount)
		}
		if got.importTime > 120*time.Second {
			t.Fatalf("fixture %s import took %v, want under 120s", got.fixture.firmID, got.importTime)
		}
		if got.determineTime > 60*time.Second {
			t.Fatalf("fixture %s determination took %v, want under 60s", got.fixture.firmID, got.determineTime)
		}
		filePath := filepath.Join(root, "data", got.fixture.fileName)
		replayed, err := application.Import(ctx, got.fixture.firmID, 2025, filePath)
		if err != nil {
			t.Fatalf("replay fixture %s: %v", got.fixture.firmID, err)
		}
		if !replayed.Existing || replayed.DatasetID != got.dataset.DatasetID || replayed.RowCount != got.dataset.RowCount || replayed.ContentSHA256 != got.dataset.ContentSHA256 {
			t.Fatalf("fixture %s replay changed dataset: first=%+v replay=%+v", got.fixture.firmID, got.dataset, replayed)
		}
		t.Logf("%s rows=%d ready=%d blocked=%d batches=%d import=%v determine=%v plan=%v", got.fixture.firmID, got.dataset.RowCount, got.determination.ReadyCount, got.determination.BlockedCount, got.plan.CreatedBatchCount+got.plan.ExistingBatchCount, got.importTime, got.determineTime, got.planTime)
	}
}
