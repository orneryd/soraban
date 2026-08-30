package store

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"readiness/postgres/internal/app"
)

type filingCandidate struct {
	clientID          string
	vendorIdentity    string
	vendorDisplayName string
	recipientTIN      string
	reportableCents   int64
	withholdingCents  int64
}

func (store *Store) DetermineDataset(ctx context.Context, command app.DetermineDatasetCommand) (app.DeterminationResult, error) {
	if command.DatasetID <= 0 || command.RulesetVersion != app.RulesetNEC2025V1 {
		return app.DeterminationResult{}, errors.New("invalid determination command")
	}
	tx, err := store.beginFirm(ctx, command.FirmID)
	if err != nil {
		return app.DeterminationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", command.DatasetID); err != nil {
		return app.DeterminationResult{}, mapError(err)
	}

	var result app.DeterminationResult
	err = tx.QueryRow(ctx, `
		SELECT d.id,
		       count(*) FILTER (WHERE f.state = 'ready'),
		       count(*) FILTER (WHERE f.state = 'blocked_preflight')
		FROM determinations d LEFT JOIN filings f ON f.determination_id = d.id AND f.firm_id = d.firm_id
		WHERE d.firm_id = $1 AND d.dataset_id = $2 AND d.ruleset_version = $3
		GROUP BY d.id`, command.FirmID, command.DatasetID, command.RulesetVersion).
		Scan(&result.DeterminationID, &result.ReadyCount, &result.BlockedCount)
	if err == nil {
		result.Existing = true
		if err := commit(ctx, tx); err != nil {
			return app.DeterminationResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return app.DeterminationResult{}, mapError(err)
	}

	var taxYear int
	if err := tx.QueryRow(ctx, `SELECT tax_year FROM datasets WHERE id = $1 AND firm_id = $2`, command.DatasetID, command.FirmID).Scan(&taxYear); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return app.DeterminationResult{}, app.ErrNotFound
		}
		return app.DeterminationResult{}, mapError(err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO determinations (dataset_id, firm_id, ruleset_version)
		VALUES ($1, $2, $3) RETURNING id`, command.DatasetID, command.FirmID, command.RulesetVersion).Scan(&result.DeterminationID); err != nil {
		return app.DeterminationResult{}, mapError(err)
	}

	rows, err := tx.Query(ctx, `
		WITH classified AS (
			SELECT * FROM payment_classification_nec_2025_v1 WHERE dataset_id = $1 AND firm_id = $2
		), aggregates AS (
			SELECT client_id, vendor_identity,
			       COALESCE(sum(amount_cents) FILTER (WHERE counted), 0)::bigint AS reportable_cents,
			       sum(backup_withholding_cents)::bigint AS withholding_cents
			FROM classified GROUP BY client_id, vendor_identity
			HAVING COALESCE(sum(amount_cents) FILTER (WHERE counted), 0) >= 60000
			    OR sum(backup_withholding_cents) > 0
		), names AS (
			SELECT client_id, vendor_identity, vendor_name,
			       row_number() OVER (PARTITION BY client_id, vendor_identity ORDER BY count(*) DESC, vendor_name) AS rank
			FROM classified GROUP BY client_id, vendor_identity, vendor_name
		)
		SELECT a.client_id, a.vendor_identity, n.vendor_name,
		       CASE WHEN a.vendor_identity LIKE 'tin:%' THEN substring(a.vendor_identity FROM 5) ELSE '' END,
		       a.reportable_cents, a.withholding_cents
		FROM aggregates a JOIN names n USING (client_id, vendor_identity)
		WHERE n.rank = 1 ORDER BY a.client_id, a.vendor_identity`, command.DatasetID, command.FirmID)
	if err != nil {
		return app.DeterminationResult{}, mapError(err)
	}
	var candidates []filingCandidate
	for rows.Next() {
		var candidate filingCandidate
		if err := rows.Scan(&candidate.clientID, &candidate.vendorIdentity, &candidate.vendorDisplayName, &candidate.recipientTIN, &candidate.reportableCents, &candidate.withholdingCents); err != nil {
			rows.Close()
			return app.DeterminationResult{}, mapError(err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return app.DeterminationResult{}, mapError(err)
	}
	rows.Close()

	for _, candidate := range candidates {
		key := filingKey(command.FirmID, candidate.clientID, taxYear, candidate.vendorIdentity)
		state := "ready"
		var reason any
		switch {
		case strings.HasPrefix(candidate.vendorIdentity, "missing-tin:"):
			state, reason = "blocked_preflight", string(app.ReasonTINMissing)
		case strings.HasPrefix(candidate.vendorIdentity, "malformed-tin:"):
			state, reason = "blocked_preflight", string(app.ReasonTINMalformed)
		case strings.HasPrefix(candidate.recipientTIN, "000"):
			state, reason = "blocked_preflight", string(app.ReasonTINInvalid)
		case candidate.reportableCents <= 0:
			state, reason = "blocked_preflight", string(app.ReasonAmountInvalid)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO filings (
				firm_id, client_id, determination_id, tax_year, vendor_identity,
				vendor_display_name, recipient_tin, filing_key, reportable_cents,
				withholding_cents, state, preflight_reason
			) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11,$12)`,
			command.FirmID, candidate.clientID, result.DeterminationID, taxYear,
			candidate.vendorIdentity, candidate.vendorDisplayName, candidate.recipientTIN,
			key[:], candidate.reportableCents, candidate.withholdingCents, state, reason); err != nil {
			return app.DeterminationResult{}, mapError(err)
		}
		if state == "ready" {
			result.ReadyCount++
		} else {
			result.BlockedCount++
		}
	}
	if err := commit(ctx, tx); err != nil {
		return app.DeterminationResult{}, err
	}
	return result, nil
}

func (store *Store) PaymentExplanation(ctx context.Context, query app.PaymentExplanationQuery) (app.PaymentExplanation, error) {
	if query.DeterminationID <= 0 || query.ClientID == "" || query.VendorIdentity == "" {
		return app.PaymentExplanation{}, errors.New("invalid explanation query")
	}
	tx, err := store.beginFirm(ctx, query.FirmID)
	if err != nil {
		return app.PaymentExplanation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT p.source_row_number, p.vendor_name, p.payment_date, p.amount_cents,
		       p.payment_method, p.backup_withholding_cents, p.counted, COALESCE(p.exclusion_reason, '')
		FROM determinations d
		JOIN payment_classification_nec_2025_v1 p ON p.dataset_id = d.dataset_id AND p.firm_id = d.firm_id
		WHERE d.id = $1 AND d.firm_id = $2 AND p.client_id = $3 AND p.vendor_identity = $4
		ORDER BY p.source_row_number`, query.DeterminationID, query.FirmID, query.ClientID, query.VendorIdentity)
	if err != nil {
		return app.PaymentExplanation{}, mapError(err)
	}
	var result app.PaymentExplanation
	for rows.Next() {
		var payment app.ExplainedPayment
		if err := rows.Scan(&payment.SourceRowNumber, &payment.VendorName, &payment.PaymentDate, &payment.AmountCents, &payment.PaymentMethod, &payment.WithholdingCents, &payment.Counted, &payment.ExclusionReason); err != nil {
			rows.Close()
			return app.PaymentExplanation{}, mapError(err)
		}
		if payment.Counted {
			result.ReportableCents += payment.AmountCents
		}
		result.WithholdingCents += payment.WithholdingCents
		result.Payments = append(result.Payments, payment)
	}
	if err := rows.Err(); err != nil {
		return app.PaymentExplanation{}, mapError(err)
	}
	if len(result.Payments) == 0 {
		return app.PaymentExplanation{}, app.ErrNotFound
	}
	if err := commit(ctx, tx); err != nil {
		return app.PaymentExplanation{}, err
	}
	return result, nil
}

func filingKeyString(key []byte) string { return hex.EncodeToString(key) }
