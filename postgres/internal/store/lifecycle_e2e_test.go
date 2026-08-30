package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"readiness/postgres/internal/app"
	"readiness/postgres/internal/store"
)

const defaultDatabaseURL = "postgres://readiness_app:readiness-app-local-only@127.0.0.1:55432/readiness?sslmode=disable"

var fixtureSequence atomic.Uint64

func requirePostgresE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("RUN_POSTGRES_E2E") != "1" {
		t.Skip("set RUN_POSTGRES_E2E=1 to run PostgreSQL E2E tests")
	}
}

type slicePaymentSource struct {
	rows      []app.PaymentRow
	index     int
	digest    [32]byte
	exhausted bool
}

func newPaymentSource(rows []app.PaymentRow) *slicePaymentSource {
	payload, err := json.Marshal(rows)
	if err != nil {
		panic(err)
	}
	return &slicePaymentSource{rows: rows, digest: sha256.Sum256(payload)}
}

func (source *slicePaymentSource) Next(context.Context) (app.PaymentRow, bool, error) {
	if source.index == len(source.rows) {
		source.exhausted = true
		return app.PaymentRow{}, false, nil
	}
	row := source.rows[source.index]
	source.index++
	return row, true, nil
}

func (source *slicePaymentSource) SHA256() ([32]byte, error) {
	if !source.exhausted {
		return [32]byte{}, errors.New("digest requested before EOF")
	}
	return source.digest, nil
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	databaseURL := testDatabaseURL()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open store: %v (run `make migrate` first)", err)
	}
	t.Cleanup(result.Close)
	return result
}

func testDatabaseURL() string {
	if value := os.Getenv("DATABASE_URL"); value != "" {
		return value
	}
	return defaultDatabaseURL
}

func uniqueFirmID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), fixtureSequence.Add(1))
}

func payment(row int64, clientID, name, tin, amount, method, withholding string) app.PaymentRow {
	return app.PaymentRow{
		SourceRowNumber:   row,
		ClientID:          clientID,
		VendorName:        name,
		VendorTIN:         tin,
		PaymentDate:       "2025-06-15",
		Amount:            amount,
		PaymentMethod:     method,
		BackupWithholding: withholding,
		Memo:              "",
	}
}

func lifecycleRows() []app.PaymentRow {
	return []app.PaymentRow{
		payment(1, "C001", "Alpha LLC", "111-11-1111", "200.00", "check", "0.00"),
		payment(2, "C001", "ALPHA LLC", "111111111", "200.00", "ach", "0.00"),
		payment(3, "C001", "Alpha, LLC", "111 11 1111", "200.00", "wire", "0.00"),
		payment(4, "C001", "Reversed Vendor", "222222222", "800.00", "check", "0.00"),
		payment(5, "C001", "Reversed Vendor", "222222222", "-550.00", "ach", "0.00"),
		payment(6, "C001", "Exact Vendor", "333333333", "600.00", "cash", "0.00"),
		payment(7, "C001", "Missing Vendor", "", "700.00", "check", "0.00"),
		payment(8, "C001", "Card Vendor", "444444444", "500.00", "check", "0.00"),
		payment(9, "C001", "Card Vendor", "444444444", "1900.00", "credit_card", "0.00"),
		payment(10, "C001", "Withholding Vendor", "555555555", "400.00", "ach", "10.00"),
		payment(11, "C001", "PayPal Vendor", "666666666", "1000.00", "paypal", "0.00"),
		payment(12, "C001", "Invalid TIN Vendor", "000123456", "700.00", "check", "0.00"),
		payment(13, "C001", "Malformed TIN Vendor", "12-34", "700.00", "check", "0.00"),
		payment(14, "C001", "Invalid Amount Vendor", "777777777", "-100.00", "check", "10.00"),
	}
}

type transmissionFixture struct {
	Details []struct {
		RecordID string `xml:"RecordId"`
	} `xml:"IRSubmission1Grp>Form1099NECDetail"`
}

func recordIDs(t *testing.T, payload []byte) []string {
	t.Helper()
	var transmission transmissionFixture
	if err := xml.Unmarshal(payload, &transmission); err != nil {
		t.Fatalf("parse canonical XML: %v", err)
	}
	result := make([]string, 0, len(transmission.Details))
	for _, detail := range transmission.Details {
		result = append(result, detail.RecordID)
	}
	return result
}

func prepareBatches(t *testing.T, database *store.Store, firmID string, rows []app.PaymentRow) (app.DeterminationResult, app.BatchPlanResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dataset, err := database.ImportDataset(ctx, app.ImportDatasetCommand{FirmID: firmID, TaxYear: 2025}, newPaymentSource(rows))
	if err != nil {
		t.Fatalf("import fixture: %v", err)
	}
	determination, err := database.DetermineDataset(ctx, app.DetermineDatasetCommand{
		FirmID: firmID, DatasetID: dataset.DatasetID, RulesetVersion: app.RulesetNEC2025V1,
	})
	if err != nil {
		t.Fatalf("determine fixture: %v", err)
	}
	plan, err := database.PlanBatches(ctx, app.PlanBatchesCommand{FirmID: firmID, DeterminationID: determination.DeterminationID})
	if err != nil {
		t.Fatalf("plan fixture: %v", err)
	}
	return determination, plan
}

func TestLifecycleE2E(t *testing.T) {
	requirePostgresE2E(t)
	database := openStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	firmID := uniqueFirmID("LIFE")
	rows := lifecycleRows()

	dataset, err := database.ImportDataset(ctx, app.ImportDatasetCommand{FirmID: firmID, TaxYear: 2025}, newPaymentSource(rows))
	if err != nil {
		t.Fatalf("import dataset: %v", err)
	}
	if dataset.Existing || dataset.RowCount != int64(len(rows)) {
		t.Fatalf("unexpected first import: %+v", dataset)
	}

	replayed, err := database.ImportDataset(ctx, app.ImportDatasetCommand{FirmID: firmID, TaxYear: 2025}, newPaymentSource(rows))
	if err != nil {
		t.Fatalf("replay dataset: %v", err)
	}
	if !replayed.Existing || replayed.DatasetID != dataset.DatasetID || replayed.ContentSHA256 != dataset.ContentSHA256 {
		t.Fatalf("replay did not return existing dataset: %+v", replayed)
	}

	changed := append([]app.PaymentRow(nil), rows...)
	changed[0].Amount = "201.00"
	if _, err := database.ImportDataset(ctx, app.ImportDatasetCommand{FirmID: firmID, TaxYear: 2025}, newPaymentSource(changed)); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("changed replay error = %v, want ErrConflict", err)
	}

	determination, err := database.DetermineDataset(ctx, app.DetermineDatasetCommand{
		FirmID: firmID, DatasetID: dataset.DatasetID, RulesetVersion: app.RulesetNEC2025V1,
	})
	if err != nil {
		t.Fatalf("determine dataset: %v", err)
	}
	if determination.ReadyCount != 3 || determination.BlockedCount != 4 {
		t.Fatalf("determination counts = ready %d blocked %d, want 3 and 4", determination.ReadyCount, determination.BlockedCount)
	}

	explanation, err := database.PaymentExplanation(ctx, app.PaymentExplanationQuery{
		FirmID: firmID, DeterminationID: determination.DeterminationID, ClientID: "C001", VendorIdentity: "tin:444444444",
	})
	if err != nil {
		t.Fatalf("payment explanation: %v", err)
	}
	if explanation.ReportableCents != 50000 || explanation.WithholdingCents != 0 || len(explanation.Payments) != 2 {
		t.Fatalf("unexpected card explanation: %+v", explanation)
	}
	if !explanation.Payments[0].Counted || explanation.Payments[1].ExclusionReason != app.ExcludedPaymentProcessor {
		t.Fatalf("incorrect payment classification: %+v", explanation.Payments)
	}

	plan, err := database.PlanBatches(ctx, app.PlanBatchesCommand{FirmID: firmID, DeterminationID: determination.DeterminationID})
	if err != nil {
		t.Fatalf("plan batches: %v", err)
	}
	if plan.CreatedBatchCount != 1 || plan.ExistingBatchCount != 0 {
		t.Fatalf("unexpected batch plan: %+v", plan)
	}

	work, found, err := database.ClaimNextBatch(ctx, app.ClaimBatchCommand{FirmID: firmID, WorkerID: "worker-1", LeaseDuration: time.Minute})
	if err != nil || !found {
		t.Fatalf("claim batch: found=%v err=%v", found, err)
	}
	if work.NextAction != app.ActionSubmit || work.UTID == "" || len(work.CanonicalXML) == 0 {
		t.Fatalf("unexpected claimed work: %+v", work)
	}
	keys := recordIDs(t, work.CanonicalXML)
	if len(keys) != 3 {
		t.Fatalf("record count = %d, want 3", len(keys))
	}
	if err := database.RecordSubmitAccepted(ctx, work.Claim, app.SubmitAccepted{ReceiptID: "receipt-" + firmID, PollDelay: 0}); err != nil {
		t.Fatalf("record submit accepted: %v", err)
	}

	work, found, err = database.ClaimNextBatch(ctx, app.ClaimBatchCommand{FirmID: firmID, WorkerID: "worker-2", LeaseDuration: time.Minute})
	if err != nil || !found || work.NextAction != app.ActionPollStatus {
		t.Fatalf("claim acknowledgment work: work=%+v found=%v err=%v", work, found, err)
	}
	if err := database.CompleteAcknowledgment(ctx, work.Claim, []app.FilingOutcome{{FilingKey: keys[0], IRSRecordID: "partial"}}); !errors.Is(err, app.ErrInvariant) {
		t.Fatalf("partial acknowledgment error = %v, want ErrInvariant", err)
	}
	outcomes := make([]app.FilingOutcome, 0, len(keys))
	for index, key := range keys {
		outcome := app.FilingOutcome{FilingKey: key, IRSRecordID: fmt.Sprintf("irs-%d-%s", index, firmID)}
		if index == len(keys)-1 {
			outcome.IRSRecordID = ""
			outcome.RejectionReason = app.ReasonTINInvalid
		}
		outcomes = append(outcomes, outcome)
	}
	if err := database.CompleteAcknowledgment(ctx, work.Claim, outcomes); err != nil {
		t.Fatalf("complete acknowledgment: %v", err)
	}

	status, err := database.ClientStatus(ctx, app.ClientStatusQuery{FirmID: firmID, ClientID: "C001"})
	if err != nil {
		t.Fatalf("client status: %v", err)
	}
	wantCounts := app.StatusCounts{Required: 7, Blocked: 4, Accepted: 2, Rejected: 1}
	if status.Counts != wantCounts || status.Headline != app.HeadlineNeedsAttention {
		t.Fatalf("client status = %+v, want counts %+v and needs attention", status, wantCounts)
	}

	exceptions, err := database.Exceptions(ctx, app.ExceptionsQuery{FirmID: firmID, ClientID: "C001"})
	if err != nil {
		t.Fatalf("exceptions: %v", err)
	}
	gotTypes := make([]string, 0, len(exceptions))
	for _, group := range exceptions {
		gotTypes = append(gotTypes, fmt.Sprintf("%s:%d", group.Type, group.Count))
	}
	sort.Strings(gotTypes)
	wantTypes := []string{"FILING_REJECTED_TIN_INVALID:1", "INVALID_AMOUNT:1", "INVALID_TIN:2", "MISSING_TIN:1"}
	if fmt.Sprint(gotTypes) != fmt.Sprint(wantTypes) {
		t.Fatalf("exception groups = %v, want %v", gotTypes, wantTypes)
	}

	permit, err := database.AcquireCallPermit(ctx, app.AcquireCallCommand{FirmID: firmID, BatchID: work.BatchID, Operation: app.OperationStatus})
	if err != nil {
		t.Fatalf("acquire call permit: %v", err)
	}
	defer permit.Close()
	if err := permit.Finish(ctx, app.CallResult{Outcome: app.CallCompleted, HTTPStatus: 200}); err != nil {
		t.Fatalf("finish call permit: %v", err)
	}
	if err := permit.Finish(ctx, app.CallResult{Outcome: app.CallCompleted, HTTPStatus: 200}); !errors.Is(err, app.ErrInvalidTransition) {
		t.Fatalf("second permit finish error = %v, want ErrInvalidTransition", err)
	}

	if _, err := database.ClientStatus(ctx, app.ClientStatusQuery{FirmID: uniqueFirmID("OTHER"), ClientID: "C001"}); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("cross-firm status error = %v, want ErrNotFound", err)
	}
}

func TestImportRollbackAndTenantIsolationE2E(t *testing.T) {
	requirePostgresE2E(t)
	database := openStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	firmOne := uniqueFirmID("RLS1")
	firmTwo := uniqueFirmID("RLS2")

	invalidRows := []app.PaymentRow{
		payment(1, "C001", "Valid Vendor", "123456789", "600.00", "check", "0.00"),
		payment(2, "C001", "Invalid Vendor", "987654321", "6.000", "check", "0.00"),
	}
	if _, err := database.ImportDataset(ctx, app.ImportDatasetCommand{FirmID: firmOne, TaxYear: 2025}, newPaymentSource(invalidRows)); err == nil {
		t.Fatal("invalid import unexpectedly succeeded")
	}
	validRows := invalidRows[:1]
	if _, err := database.ImportDataset(ctx, app.ImportDatasetCommand{FirmID: firmOne, TaxYear: 2025}, newPaymentSource(validRows)); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if _, err := database.ImportDataset(ctx, app.ImportDatasetCommand{FirmID: firmTwo, TaxYear: 2025}, newPaymentSource(validRows)); err != nil {
		t.Fatalf("import second firm: %v", err)
	}

	pool, err := pgxpool.New(ctx, testDatabaseURL())
	if err != nil {
		t.Fatalf("open direct runtime connection: %v", err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin RLS check: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.firm_id', $1, true)", firmOne); err != nil {
		t.Fatalf("set firm context: %v", err)
	}
	var visible int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM datasets WHERE firm_id=$1`, firmTwo).Scan(&visible); err != nil {
		t.Fatalf("cross-firm read: %v", err)
	}
	if visible != 0 {
		t.Fatalf("cross-firm rows visible = %d, want 0", visible)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO clients (firm_id,id) VALUES ($1,'CROSS')`, firmTwo); err == nil {
		t.Fatal("cross-firm insert unexpectedly succeeded")
	}
}

func TestBatchLimitAndConcurrentClaimsE2E(t *testing.T) {
	requirePostgresE2E(t)
	database := openStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	firmID := uniqueFirmID("BATCH")
	rows := make([]app.PaymentRow, 101)
	for index := range rows {
		rows[index] = payment(int64(index+1), "C100", fmt.Sprintf("Vendor %03d", index), fmt.Sprintf("%09d", 100000000+index), "600.00", "check", "0.00")
	}
	determination, plan := prepareBatches(t, database, firmID, rows)
	if determination.ReadyCount != 101 || plan.CreatedBatchCount != 2 {
		t.Fatalf("ready=%d batches=%d, want 101 and 2", determination.ReadyCount, plan.CreatedBatchCount)
	}

	type claimResult struct {
		work  app.BatchWork
		found bool
		err   error
	}
	results := make(chan claimResult, 2)
	for index := 0; index < 2; index++ {
		go func(worker int) {
			work, found, err := database.ClaimNextBatch(ctx, app.ClaimBatchCommand{
				FirmID: firmID, WorkerID: fmt.Sprintf("worker-%d", worker), LeaseDuration: time.Minute,
			})
			results <- claimResult{work: work, found: found, err: err}
		}(index)
	}
	claimed := make([]app.BatchWork, 0, 2)
	for range 2 {
		result := <-results
		if result.err != nil || !result.found {
			t.Fatalf("concurrent claim: found=%v err=%v", result.found, result.err)
		}
		claimed = append(claimed, result.work)
	}
	if claimed[0].BatchID == claimed[1].BatchID {
		t.Fatalf("workers claimed the same batch %d", claimed[0].BatchID)
	}
	sizes := []int{len(recordIDs(t, claimed[0].CanonicalXML)), len(recordIDs(t, claimed[1].CanonicalXML))}
	sort.Ints(sizes)
	if sizes[0] != 1 || sizes[1] != 100 {
		t.Fatalf("batch sizes = %v, want [1 100]", sizes)
	}
	for _, work := range claimed {
		if err := database.FailBatchInvariant(ctx, work.Claim, "TEST_COMPLETE"); err != nil {
			t.Fatalf("finish claimed batch: %v", err)
		}
	}
	replayed, err := database.PlanBatches(ctx, app.PlanBatchesCommand{FirmID: firmID, DeterminationID: determination.DeterminationID})
	if err != nil {
		t.Fatalf("replay batch plan: %v", err)
	}
	if replayed.ExistingBatchCount != 2 || replayed.CreatedBatchCount != 0 {
		t.Fatalf("replayed plan = %+v, want 2 existing", replayed)
	}
}

func TestAmbiguousSubmissionReconciliationE2E(t *testing.T) {
	requirePostgresE2E(t)
	database := openStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	firmID := uniqueFirmID("UNKNOWN")
	_, plan := prepareBatches(t, database, firmID, []app.PaymentRow{
		payment(1, "C200", "Stable Vendor", "888888888", "600.00", "check", "0.00"),
	})
	if plan.CreatedBatchCount != 1 {
		t.Fatalf("created batches = %d, want 1", plan.CreatedBatchCount)
	}

	first, found, err := database.ClaimNextBatch(ctx, app.ClaimBatchCommand{FirmID: firmID, WorkerID: "worker-1", LeaseDuration: time.Minute})
	if err != nil || !found || first.NextAction != app.ActionSubmit {
		t.Fatalf("first claim: work=%+v found=%v err=%v", first, found, err)
	}
	if err := database.RecordSubmitUnknown(ctx, first.Claim, app.RetrySchedule{FailureCode: "HTTP_503"}); err != nil {
		t.Fatalf("record unknown submit: %v", err)
	}
	lookup, found, err := database.ClaimNextBatch(ctx, app.ClaimBatchCommand{FirmID: firmID, WorkerID: "worker-2", LeaseDuration: time.Minute})
	if err != nil || !found || lookup.NextAction != app.ActionLookupByUTID {
		t.Fatalf("lookup claim: work=%+v found=%v err=%v", lookup, found, err)
	}
	if lookup.UTID != first.UTID || string(lookup.CanonicalXML) != string(first.CanonicalXML) || lookup.PayloadSHA256 != first.PayloadSHA256 {
		t.Fatal("ambiguous reconciliation changed immutable submission identity")
	}
	if err := database.RecordStatusUnavailable(ctx, lookup.Claim, app.RetrySchedule{FailureCode: "LOOKUP_UNAVAILABLE"}); err != nil {
		t.Fatalf("record unavailable lookup: %v", err)
	}
	lookup, found, err = database.ClaimNextBatch(ctx, app.ClaimBatchCommand{FirmID: firmID, WorkerID: "worker-lookup-retry", LeaseDuration: time.Minute})
	if err != nil || !found || lookup.NextAction != app.ActionLookupByUTID {
		t.Fatalf("lookup retry claim: work=%+v found=%v err=%v", lookup, found, err)
	}
	if err := database.RecordReferenceNotFound(ctx, lookup.Claim, app.RetrySchedule{FailureCode: "NOT_FOUND"}); err != nil {
		t.Fatalf("record reference not found: %v", err)
	}
	retry, found, err := database.ClaimNextBatch(ctx, app.ClaimBatchCommand{FirmID: firmID, WorkerID: "worker-3", LeaseDuration: time.Minute})
	if err != nil || !found || retry.NextAction != app.ActionSubmit {
		t.Fatalf("retry claim: work=%+v found=%v err=%v", retry, found, err)
	}
	if retry.UTID != first.UTID || string(retry.CanonicalXML) != string(first.CanonicalXML) || retry.PayloadSHA256 != first.PayloadSHA256 {
		t.Fatal("submission retry changed immutable identity")
	}
	if err := database.FailBatchInvariant(ctx, retry.Claim, "TEST_COMPLETE"); err != nil {
		t.Fatalf("finish retry fixture: %v", err)
	}
}

func TestRateGateSerializesFirmE2E(t *testing.T) {
	requirePostgresE2E(t)
	database := openStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	firmID := uniqueFirmID("RATE")
	_, plan := prepareBatches(t, database, firmID, []app.PaymentRow{
		payment(1, "C300", "Rate Vendor", "123123123", "600.00", "check", "0.00"),
	})
	if plan.CreatedBatchCount != 1 {
		t.Fatalf("created batches = %d, want 1", plan.CreatedBatchCount)
	}
	work, found, err := database.ClaimNextBatch(ctx, app.ClaimBatchCommand{FirmID: firmID, WorkerID: "rate-worker", LeaseDuration: time.Minute})
	if err != nil || !found {
		t.Fatalf("claim rate fixture: found=%v err=%v", found, err)
	}

	first, err := database.AcquireCallPermit(ctx, app.AcquireCallCommand{FirmID: firmID, BatchID: work.BatchID, Operation: app.OperationSubmit})
	if err != nil {
		t.Fatalf("acquire first permit: %v", err)
	}
	type permitResult struct {
		permit app.CallPermit
		err    error
		at     time.Time
	}
	secondResult := make(chan permitResult, 1)
	go func() {
		permit, acquireErr := database.AcquireCallPermit(ctx, app.AcquireCallCommand{FirmID: firmID, BatchID: work.BatchID, Operation: app.OperationStatus})
		secondResult <- permitResult{permit: permit, err: acquireErr, at: time.Now()}
	}()
	select {
	case result := <-secondResult:
		if result.permit != nil {
			_ = result.permit.Close()
		}
		t.Fatalf("second permit bypassed live firm lock: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	finishedAt := time.Now()
	if err := first.Finish(ctx, app.CallResult{Outcome: app.CallCompleted, HTTPStatus: 200}); err != nil {
		t.Fatalf("finish first permit: %v", err)
	}
	_ = first.Close()
	second := <-secondResult
	if second.err != nil {
		t.Fatalf("acquire second permit: %v", second.err)
	}
	defer second.permit.Close()
	if spacing := second.at.Sub(finishedAt); spacing < 3*time.Second {
		t.Fatalf("second permit spacing = %v, want at least 3s", spacing)
	}
	if err := second.permit.Finish(ctx, app.CallResult{Outcome: app.CallCompleted, HTTPStatus: 200}); err != nil {
		t.Fatalf("finish second permit: %v", err)
	}
	if err := database.FailBatchInvariant(ctx, work.Claim, "TEST_COMPLETE"); err != nil {
		t.Fatalf("finish rate fixture: %v", err)
	}
}
