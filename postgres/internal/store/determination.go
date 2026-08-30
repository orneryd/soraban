package store

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"readiness.local/postgres/internal/app"
)

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

	err = tx.QueryRow(ctx, `
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
		), candidates AS (
			SELECT a.client_id, a.vendor_identity, n.vendor_name,
			       CASE WHEN a.vendor_identity LIKE 'tin:%' THEN substring(a.vendor_identity FROM 5) ELSE NULL END AS recipient_tin,
			       a.reportable_cents, a.withholding_cents
			FROM aggregates a JOIN names n USING (client_id, vendor_identity)
			WHERE n.rank = 1
		), prepared AS (
			SELECT c.*,
			       sha256(
			           int4send(octet_length(convert_to('filing-key-v1','UTF8'))) || convert_to('filing-key-v1','UTF8') ||
			           int4send(octet_length(convert_to($2,'UTF8'))) || convert_to($2,'UTF8') ||
			           int4send(octet_length(convert_to(c.client_id,'UTF8'))) || convert_to(c.client_id,'UTF8') ||
			           int4send(octet_length(convert_to($3::text,'UTF8'))) || convert_to($3::text,'UTF8') ||
			           int4send(octet_length(convert_to('1099-NEC','UTF8'))) || convert_to('1099-NEC','UTF8') ||
			           int4send(octet_length(convert_to(c.vendor_identity,'UTF8'))) || convert_to(c.vendor_identity,'UTF8') ||
			           int4send(octet_length(convert_to('0','UTF8'))) || convert_to('0','UTF8')
			       ) AS filing_key,
			       CASE
			           WHEN c.vendor_identity LIKE 'missing-tin:%' THEN 'blocked_preflight'
			           WHEN c.vendor_identity LIKE 'malformed-tin:%' THEN 'blocked_preflight'
			           WHEN c.recipient_tin LIKE '000%' THEN 'blocked_preflight'
			           WHEN c.reportable_cents <= 0 THEN 'blocked_preflight'
			           ELSE 'ready'
			       END AS state,
			       CASE
			           WHEN c.vendor_identity LIKE 'missing-tin:%' THEN 'TIN_MISSING'
			           WHEN c.vendor_identity LIKE 'malformed-tin:%' THEN 'TIN_MALFORMED'
			           WHEN c.recipient_tin LIKE '000%' THEN 'TIN_INVALID'
			           WHEN c.reportable_cents <= 0 THEN 'AMOUNT_INVALID'
			           ELSE NULL
			       END AS preflight_reason
			FROM candidates c
		), inserted AS (
			INSERT INTO filings (
				firm_id, client_id, determination_id, tax_year, vendor_identity,
				vendor_display_name, recipient_tin, filing_key, reportable_cents,
				withholding_cents, state, preflight_reason
			)
			SELECT $2, client_id, $4, $3::integer, vendor_identity, vendor_name, recipient_tin,
			       filing_key, reportable_cents, withholding_cents, state, preflight_reason
			FROM prepared
			RETURNING state
		)
		SELECT count(*) FILTER (WHERE state='ready'),
		       count(*) FILTER (WHERE state='blocked_preflight')
		FROM inserted`, command.DatasetID, command.FirmID, strconv.Itoa(taxYear), result.DeterminationID).
		Scan(&result.ReadyCount, &result.BlockedCount)
	if err != nil {
		return app.DeterminationResult{}, mapError(err)
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
