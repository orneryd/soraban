package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"readiness.local/postgres/internal/app"
)

type plannedFiling struct {
	id                int64
	clientID          string
	taxYear           int
	key               [32]byte
	vendorDisplayName string
	recipientTIN      string
	reportableCents   int64
	withholdingCents  int64
}

type plannedBatch struct {
	sequence     int
	clientID     string
	taxYear      int
	utid         string
	canonicalXML []byte
	payloadHash  [32]byte
}

type plannedLink struct {
	utid     string
	filingID int64
	clientID string
	slot     int
}

func (store *Store) PlanBatches(ctx context.Context, command app.PlanBatchesCommand) (app.BatchPlanResult, error) {
	if command.DeterminationID <= 0 {
		return app.BatchPlanResult{}, errors.New("invalid batch plan command")
	}
	tx, err := store.beginFirm(ctx, command.FirmID)
	if err != nil {
		return app.BatchPlanResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", -command.DeterminationID); err != nil {
		return app.BatchPlanResult{}, mapError(err)
	}
	var result app.BatchPlanResult
	if err := tx.QueryRow(ctx, `
		SELECT count(DISTINCT bf.batch_id)
		FROM batch_filings bf JOIN filings f ON f.id = bf.filing_id
		WHERE f.firm_id = $1 AND f.determination_id = $2`, command.FirmID, command.DeterminationID).Scan(&result.ExistingBatchCount); err != nil {
		return app.BatchPlanResult{}, mapError(err)
	}
	rows, err := tx.Query(ctx, `
		SELECT f.id, f.client_id, f.tax_year, f.filing_key, f.vendor_display_name,
		       COALESCE(f.recipient_tin, ''), f.reportable_cents, f.withholding_cents
		FROM filings f LEFT JOIN batch_filings bf ON bf.filing_id = f.id
		WHERE f.firm_id = $1 AND f.determination_id = $2 AND f.state = 'ready' AND bf.filing_id IS NULL
		ORDER BY f.client_id, f.filing_key`, command.FirmID, command.DeterminationID)
	if err != nil {
		return app.BatchPlanResult{}, mapError(err)
	}
	var filings []plannedFiling
	for rows.Next() {
		var filing plannedFiling
		var key []byte
		if err := rows.Scan(&filing.id, &filing.clientID, &filing.taxYear, &key, &filing.vendorDisplayName, &filing.recipientTIN, &filing.reportableCents, &filing.withholdingCents); err != nil {
			rows.Close()
			return app.BatchPlanResult{}, mapError(err)
		}
		if len(key) != len(filing.key) {
			rows.Close()
			return app.BatchPlanResult{}, app.ErrInvariant
		}
		copy(filing.key[:], key)
		filings = append(filings, filing)
	}
	if err := rows.Err(); err != nil {
		return app.BatchPlanResult{}, mapError(err)
	}
	rows.Close()

	batches := make([]plannedBatch, 0, (len(filings)+99)/100)
	links := make([]plannedLink, 0, len(filings))
	for start := 0; start < len(filings); {
		end := start
		for end < len(filings) && end-start < 100 && filings[end].clientID == filings[start].clientID {
			end++
		}
		group := filings[start:end]
		keys := make([][32]byte, len(group))
		details := make([]xmlDetail, len(group))
		for index, filing := range group {
			keys[index] = filing.key
			details[index] = xmlDetail{
				RecordID: filingKeyString(filing.key[:]), RecipientName: filing.vendorDisplayName,
				RecipientTIN: filing.recipientTIN, Compensation: formatCents(filing.reportableCents),
				Withholding: formatCents(filing.withholdingCents),
			}
		}
		batchDigest, utid := batchIdentity(command.FirmID, group[0].clientID, group[0].taxYear, keys)
		xmlPayload, err := canonicalXML(command.FirmID, group[0].clientID, group[0].taxYear, utid, details, batchDigest)
		if err != nil {
			return app.BatchPlanResult{}, app.ErrInvariant
		}
		payloadHash := sha256.Sum256(xmlPayload)
		batches = append(batches, plannedBatch{
			sequence: len(batches) + 1, clientID: group[0].clientID, taxYear: group[0].taxYear,
			utid: utid, canonicalXML: xmlPayload, payloadHash: payloadHash,
		})
		for index, filing := range group {
			links = append(links, plannedLink{utid: utid, filingID: filing.id, clientID: filing.clientID, slot: index + 1})
		}
		start = end
	}
	if len(batches) > 0 {
		if _, err := tx.Exec(ctx, `
			CREATE TEMPORARY TABLE batch_stage (
				sequence integer, client_id text, tax_year integer, utid text,
				canonical_xml bytea, payload_sha256 bytea
			) ON COMMIT DROP;
			CREATE TEMPORARY TABLE batch_link_stage (
				utid text, filing_id bigint, client_id text, slot integer
			) ON COMMIT DROP`); err != nil {
			return app.BatchPlanResult{}, mapError(err)
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"batch_stage"},
			[]string{"sequence", "client_id", "tax_year", "utid", "canonical_xml", "payload_sha256"},
			pgx.CopyFromSlice(len(batches), func(index int) ([]any, error) {
				batch := batches[index]
				return []any{batch.sequence, batch.clientID, batch.taxYear, batch.utid, batch.canonicalXML, batch.payloadHash[:]}, nil
			})); err != nil {
			return app.BatchPlanResult{}, mapError(err)
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"batch_link_stage"},
			[]string{"utid", "filing_id", "client_id", "slot"},
			pgx.CopyFromSlice(len(links), func(index int) ([]any, error) {
				link := links[index]
				return []any{link.utid, link.filingID, link.clientID, link.slot}, nil
			})); err != nil {
			return app.BatchPlanResult{}, mapError(err)
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO submission_batches (firm_id, client_id, tax_year, utid, canonical_xml, payload_sha256, state)
			SELECT $1, client_id, tax_year, utid, canonical_xml, payload_sha256, 'planned'
			FROM batch_stage ORDER BY sequence`, command.FirmID)
		if err != nil {
			return app.BatchPlanResult{}, mapError(err)
		}
		result.CreatedBatchCount = tag.RowsAffected()
		if result.CreatedBatchCount != int64(len(batches)) {
			return app.BatchPlanResult{}, app.ErrInvariant
		}
		tag, err = tx.Exec(ctx, `
			INSERT INTO batch_filings (batch_id, filing_id, firm_id, client_id, slot)
			SELECT b.id, l.filing_id, $1, l.client_id, l.slot
			FROM batch_link_stage l
			JOIN submission_batches b ON b.firm_id=$1 AND b.utid=l.utid`, command.FirmID)
		if err != nil {
			return app.BatchPlanResult{}, mapError(err)
		}
		if tag.RowsAffected() != int64(len(links)) {
			return app.BatchPlanResult{}, app.ErrInvariant
		}
		tag, err = tx.Exec(ctx, `
			UPDATE filings f SET state='batched'
			FROM batch_link_stage l
			WHERE f.id=l.filing_id AND f.firm_id=$1 AND f.state='ready'`, command.FirmID)
		if err != nil {
			return app.BatchPlanResult{}, mapError(err)
		}
		if tag.RowsAffected() != int64(len(links)) {
			return app.BatchPlanResult{}, app.ErrInvariant
		}
	}
	if err := commit(ctx, tx); err != nil {
		return app.BatchPlanResult{}, err
	}
	return result, nil
}

func newClaimToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (store *Store) ClaimNextBatch(ctx context.Context, command app.ClaimBatchCommand) (app.BatchWork, bool, error) {
	if command.WorkerID == "" || command.LeaseDuration <= 0 {
		return app.BatchWork{}, false, errors.New("invalid claim command")
	}
	token, err := newClaimToken()
	if err != nil {
		return app.BatchWork{}, false, errors.New("create claim token")
	}
	tx, err := store.beginFirm(ctx, command.FirmID)
	if err != nil {
		return app.BatchWork{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var work app.BatchWork
	var state string
	var payloadHash []byte
	leaseMicros := command.LeaseDuration.Microseconds()
	err = tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id, state FROM submission_batches
			WHERE firm_id = $1 AND next_action_at <= clock_timestamp()
			  AND state NOT IN ('acknowledged', 'invariant_failed')
			  AND (lease_expires_at IS NULL OR lease_expires_at <= clock_timestamp())
			ORDER BY next_action_at, id FOR UPDATE SKIP LOCKED LIMIT 1
		)
		UPDATE submission_batches b
		SET state = CASE candidate.state
				WHEN 'planned' THEN 'submitting'
				WHEN 'submitting' THEN 'submit_unknown'
				WHEN 'submitted' THEN 'awaiting_ack'
				ELSE candidate.state END,
			lease_owner = $2, lease_expires_at = clock_timestamp() + $3 * interval '1 microsecond',
			claim_token = $4, attempt_count = attempt_count + 1
		FROM candidate WHERE b.id = candidate.id
		RETURNING b.id, b.firm_id, b.client_id, b.tax_year, b.state, b.utid,
		          b.canonical_xml, b.payload_sha256, COALESCE(b.receipt_id, ''), b.attempt_count`,
		command.FirmID, command.WorkerID, leaseMicros, token).
		Scan(&work.BatchID, &work.FirmID, &work.ClientID, &work.TaxYear, &state, &work.UTID, &work.CanonicalXML, &payloadHash, &work.ReceiptID, &work.AttemptCount)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := commit(ctx, tx); err != nil {
			return app.BatchWork{}, false, err
		}
		return app.BatchWork{}, false, nil
	}
	if err != nil {
		return app.BatchWork{}, false, mapError(err)
	}
	if len(payloadHash) != 32 {
		return app.BatchWork{}, false, app.ErrInvariant
	}
	copy(work.PayloadSHA256[:], payloadHash)
	actualHash := sha256.Sum256(work.CanonicalXML)
	if actualHash != work.PayloadSHA256 {
		if _, err := tx.Exec(ctx, `UPDATE submission_batches SET state='invariant_failed', last_error_code='PAYLOAD_HASH_MISMATCH', lease_owner=NULL, lease_expires_at=NULL, claim_token=NULL WHERE id=$1 AND firm_id=$2`, work.BatchID, command.FirmID); err != nil {
			return app.BatchWork{}, false, mapError(err)
		}
		if err := commit(ctx, tx); err != nil {
			return app.BatchWork{}, false, err
		}
		return app.BatchWork{}, false, app.ErrInvariant
	}
	switch state {
	case "submitting":
		work.NextAction = app.ActionSubmit
	case "submit_unknown":
		work.NextAction = app.ActionLookupByUTID
	case "submitted", "awaiting_ack":
		work.NextAction = app.ActionPollStatus
	default:
		return app.BatchWork{}, false, app.ErrInvariant
	}
	work.Claim = app.NewStoreClaim(work.BatchID, work.FirmID, token)
	if err := commit(ctx, tx); err != nil {
		return app.BatchWork{}, false, err
	}
	return work, true, nil
}

func validDelay(delay time.Duration) bool { return delay >= 0 && delay <= 30*24*time.Hour }

func validFailureCode(code app.FailureCode) bool {
	if len(code) > 64 {
		return false
	}
	for _, character := range code {
		if !((character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("_.-", character)) {
			return false
		}
	}
	return true
}

func claimMiss(ctx context.Context, tx pgx.Tx, batchID int64, firmID, token string) error {
	var currentToken string
	var state string
	var live bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(claim_token,''), state, COALESCE(lease_expires_at > clock_timestamp(), false) FROM submission_batches WHERE id=$1 AND firm_id=$2`, batchID, firmID).Scan(&currentToken, &state, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ErrNotFound
	}
	if err != nil {
		return mapError(err)
	}
	if currentToken != token || !live {
		return app.ErrStaleClaim
	}
	return app.ErrInvalidTransition
}

func (store *Store) eventTransition(ctx context.Context, claim app.Claim, states []string, newState string, delay time.Duration, receiptID string, failureCode app.FailureCode, setSubmitted bool) error {
	batchID, firmID, token := app.StoreClaimParts(claim)
	if batchID <= 0 || !validDelay(delay) || !validFailureCode(failureCode) || len(receiptID) > 256 {
		return errors.New("invalid transition event")
	}
	tx, err := store.beginFirm(ctx, firmID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE submission_batches
		SET state=$1, receipt_id=CASE WHEN $2='' THEN receipt_id ELSE $2 END,
		    next_action_at=clock_timestamp()+$3*interval '1 microsecond',
		    submitted_at=CASE WHEN $4 THEN COALESCE(submitted_at, clock_timestamp()) ELSE submitted_at END,
		    last_error_code=NULLIF($5,''), lease_owner=NULL, lease_expires_at=NULL, claim_token=NULL
		WHERE id=$6 AND firm_id=$7 AND claim_token=$8 AND lease_expires_at > clock_timestamp() AND state=ANY($9)`,
		newState, receiptID, delay.Microseconds(), setSubmitted, string(failureCode), batchID, firmID, token, states)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() != 1 {
		return claimMiss(ctx, tx, batchID, firmID, token)
	}
	return commit(ctx, tx)
}

func (store *Store) RecordSubmitAccepted(ctx context.Context, claim app.Claim, event app.SubmitAccepted) error {
	if event.ReceiptID == "" {
		return errors.New("missing receipt identity")
	}
	return store.eventTransition(ctx, claim, []string{"submitting"}, "submitted", event.PollDelay, event.ReceiptID, "", true)
}

func (store *Store) RecordSubmitUnknown(ctx context.Context, claim app.Claim, schedule app.RetrySchedule) error {
	return store.eventTransition(ctx, claim, []string{"submitting"}, "submit_unknown", schedule.Delay, "", schedule.FailureCode, false)
}

func (store *Store) RecordReferenceFound(ctx context.Context, claim app.Claim, event app.ReferenceFound) error {
	if event.ReceiptID == "" {
		return errors.New("missing receipt identity")
	}
	return store.eventTransition(ctx, claim, []string{"submit_unknown"}, "submitted", event.PollDelay, event.ReceiptID, "", true)
}

func (store *Store) RecordReferenceNotFound(ctx context.Context, claim app.Claim, schedule app.RetrySchedule) error {
	return store.eventTransition(ctx, claim, []string{"submit_unknown"}, "planned", schedule.Delay, "", schedule.FailureCode, false)
}

func (store *Store) RecordAcknowledgmentPending(ctx context.Context, claim app.Claim, schedule app.RetrySchedule) error {
	return store.eventTransition(ctx, claim, []string{"submitted", "awaiting_ack"}, "awaiting_ack", schedule.Delay, "", schedule.FailureCode, false)
}

func (store *Store) RecordStatusUnavailable(ctx context.Context, claim app.Claim, schedule app.RetrySchedule) error {
	batchID, firmID, token := app.StoreClaimParts(claim)
	if batchID <= 0 || !validDelay(schedule.Delay) || !validFailureCode(schedule.FailureCode) {
		return errors.New("invalid status retry event")
	}
	tx, err := store.beginFirm(ctx, firmID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE submission_batches
		SET next_action_at=clock_timestamp()+$1*interval '1 microsecond',
		    last_error_code=NULLIF($2,''), lease_owner=NULL, lease_expires_at=NULL, claim_token=NULL
		WHERE id=$3 AND firm_id=$4 AND claim_token=$5 AND lease_expires_at > clock_timestamp()
		  AND state IN ('submit_unknown','awaiting_ack')`,
		schedule.Delay.Microseconds(), string(schedule.FailureCode), batchID, firmID, token)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() != 1 {
		return claimMiss(ctx, tx, batchID, firmID, token)
	}
	return commit(ctx, tx)
}

func (store *Store) CompleteAcknowledgment(ctx context.Context, claim app.Claim, outcomes []app.FilingOutcome) error {
	batchID, firmID, token := app.StoreClaimParts(claim)
	if batchID <= 0 || len(outcomes) == 0 {
		return app.ErrInvariant
	}
	tx, err := store.beginFirm(ctx, firmID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT f.id, encode(f.filing_key, 'hex') FROM batch_filings bf
		JOIN filings f ON f.id=bf.filing_id AND f.firm_id=bf.firm_id
		WHERE bf.batch_id=$1 AND bf.firm_id=$2 ORDER BY bf.slot`, batchID, firmID)
	if err != nil {
		return mapError(err)
	}
	expected := make(map[string]int64)
	for rows.Next() {
		var filingID int64
		var key string
		if err := rows.Scan(&filingID, &key); err != nil {
			rows.Close()
			return mapError(err)
		}
		expected[key] = filingID
	}
	if err := rows.Err(); err != nil {
		return mapError(err)
	}
	rows.Close()
	if len(expected) != len(outcomes) {
		return app.ErrInvariant
	}
	seen := make(map[string]struct{}, len(outcomes))
	for _, outcome := range outcomes {
		filingID, found := expected[outcome.FilingKey]
		if !found {
			return app.ErrInvariant
		}
		if _, duplicate := seen[outcome.FilingKey]; duplicate {
			return app.ErrInvariant
		}
		seen[outcome.FilingKey] = struct{}{}
		accepted := outcome.IRSRecordID != "" && outcome.RejectionReason == ""
		rejected := outcome.IRSRecordID == "" && isRejectionReason(outcome.RejectionReason)
		if !accepted && !rejected {
			return app.ErrInvariant
		}
		state := "accepted"
		var recordID any = outcome.IRSRecordID
		var rejection any
		if rejected {
			state = "rejected"
			recordID = nil
			rejection = string(outcome.RejectionReason)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE filings SET state=$1, irs_record_id=$2, rejection_reason=$3, acknowledged_at=clock_timestamp()
			WHERE id=$4 AND firm_id=$5 AND state='batched'`, state, recordID, rejection, filingID, firmID)
		if err != nil {
			return mapError(err)
		}
		if tag.RowsAffected() != 1 {
			return app.ErrInvariant
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE submission_batches SET state='acknowledged', acknowledged_at=clock_timestamp(),
		    lease_owner=NULL, lease_expires_at=NULL, claim_token=NULL, last_error_code=NULL
		WHERE id=$1 AND firm_id=$2 AND claim_token=$3 AND lease_expires_at > clock_timestamp()
		  AND state IN ('submitted','awaiting_ack')`, batchID, firmID, token)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() != 1 {
		return claimMiss(ctx, tx, batchID, firmID, token)
	}
	return commit(ctx, tx)
}

func isRejectionReason(reason app.PreflightReason) bool {
	return reason == app.ReasonTINMissing || reason == app.ReasonTINMalformed || reason == app.ReasonTINInvalid || reason == app.ReasonAmountInvalid
}

func (store *Store) terminalTransition(ctx context.Context, claim app.Claim, code app.FailureCode, exhausted bool) error {
	batchID, firmID, token := app.StoreClaimParts(claim)
	if batchID <= 0 || code == "" || !validFailureCode(code) {
		return errors.New("invalid terminal event")
	}
	tx, err := store.beginFirm(ctx, firmID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE submission_batches SET state='invariant_failed', last_error_code=$1,
		    retry_exhausted_at=CASE WHEN $2 THEN clock_timestamp() ELSE NULL END,
		    lease_owner=NULL, lease_expires_at=NULL, claim_token=NULL
		WHERE id=$3 AND firm_id=$4 AND claim_token=$5 AND lease_expires_at > clock_timestamp()
		  AND state NOT IN ('acknowledged','invariant_failed')`, code, exhausted, batchID, firmID, token)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() != 1 {
		return claimMiss(ctx, tx, batchID, firmID, token)
	}
	return commit(ctx, tx)
}

func (store *Store) RecordRetryExhausted(ctx context.Context, claim app.Claim, code app.FailureCode) error {
	return store.terminalTransition(ctx, claim, code, true)
}

func (store *Store) FailBatchInvariant(ctx context.Context, claim app.Claim, code app.FailureCode) error {
	return store.terminalTransition(ctx, claim, code, false)
}

func (filing plannedFiling) String() string { return fmt.Sprintf("%d", filing.id) }
