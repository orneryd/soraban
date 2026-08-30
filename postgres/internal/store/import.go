package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"readiness.local/postgres/internal/app"
)

type paymentCopySource struct {
	ctx     context.Context
	source  app.PaymentRowSource
	current app.PaymentRow
	err     error
}

func (source *paymentCopySource) Next() bool {
	if source.err != nil {
		return false
	}
	row, found, err := source.source.Next(source.ctx)
	if err != nil {
		source.err = err
		return false
	}
	if !found {
		return false
	}
	source.current = row
	return true
}

func (source *paymentCopySource) Values() ([]any, error) {
	row := source.current
	return []any{row.SourceRowNumber, row.ClientID, row.VendorName, row.VendorTIN, row.PaymentDate, row.Amount, row.PaymentMethod, row.BackupWithholding, row.Memo}, nil
}

func (source *paymentCopySource) Err() error { return source.err }

func (store *Store) ImportDataset(ctx context.Context, command app.ImportDatasetCommand, rows app.PaymentRowSource) (app.DatasetResult, error) {
	if rows == nil || command.TaxYear != 2025 {
		return app.DatasetResult{}, errors.New("invalid import command")
	}
	tx, err := store.beginFirm(ctx, command.FirmID)
	if err != nil {
		return app.DatasetResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", fmt.Sprintf("dataset:%s:%d", command.FirmID, command.TaxYear)); err != nil {
		return app.DatasetResult{}, mapError(err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TEMPORARY TABLE payment_stage (
			source_row_number bigint, client_id text, vendor_name text, vendor_tin text,
			payment_date text, amount text, payment_method text,
			backup_withholding text, memo text
		) ON COMMIT DROP`); err != nil {
		return app.DatasetResult{}, mapError(err)
	}
	copySource := &paymentCopySource{ctx: ctx, source: rows}
	copied, err := tx.CopyFrom(ctx, pgx.Identifier{"payment_stage"}, []string{
		"source_row_number", "client_id", "vendor_name", "vendor_tin", "payment_date",
		"amount", "payment_method", "backup_withholding", "memo",
	}, copySource)
	if err != nil {
		if copySource.err != nil {
			return app.DatasetResult{}, copySource.err
		}
		return app.DatasetResult{}, mapError(err)
	}
	contentHash, err := rows.SHA256()
	if err != nil {
		return app.DatasetResult{}, err
	}
	if copied == 0 {
		return app.DatasetResult{}, errors.New("empty dataset")
	}

	var invalidRow int64
	var invalidField string
	err = tx.QueryRow(ctx, `
		WITH invalid AS (
			SELECT source_row_number,
				CASE
					WHEN source_row_number IS NULL OR source_row_number <= 0 THEN 'source_row_number'
					WHEN client_id IS NULL OR btrim(client_id) = '' THEN 'client_id'
					WHEN vendor_name IS NULL OR btrim(vendor_name) = '' THEN 'vendor_name'
					WHEN payment_date !~ '^2025-[0-9]{2}-[0-9]{2}$' OR NOT pg_input_is_valid(payment_date, 'date') THEN 'payment_date'
					WHEN amount !~ '^-?(0|[1-9][0-9]*)(\.[0-9]{1,2})?$' OR NOT pg_input_is_valid(amount, 'numeric')
						OR abs(amount::numeric * 100) > 9223372036854775807 THEN 'amount'
					WHEN payment_method NOT IN ('check', 'ach', 'wire', 'cash', 'credit_card', 'paypal') THEN 'payment_method'
					WHEN backup_withholding !~ '^(0|[1-9][0-9]*)(\.[0-9]{1,2})?$' OR NOT pg_input_is_valid(backup_withholding, 'numeric')
						OR backup_withholding::numeric * 100 > 9223372036854775807 THEN 'backup_withholding'
					WHEN memo IS NULL THEN 'memo'
				END AS field
			FROM payment_stage
		), duplicate_rows AS (
			SELECT min(source_row_number) AS source_row_number, 'source_row_number'::text AS field
			FROM payment_stage GROUP BY source_row_number HAVING count(*) > 1
		)
		SELECT source_row_number, field FROM (
			SELECT * FROM invalid WHERE field IS NOT NULL UNION ALL SELECT * FROM duplicate_rows
		) failures ORDER BY source_row_number LIMIT 1`).Scan(&invalidRow, &invalidField)
	if err == nil {
		return app.DatasetResult{}, fmt.Errorf("source row %d: invalid %s", invalidRow, invalidField)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return app.DatasetResult{}, mapError(err)
	}

	var result app.DatasetResult
	var storedHash []byte
	err = tx.QueryRow(ctx, `SELECT id, content_sha256, source_row_count FROM datasets WHERE firm_id = $1 AND tax_year = $2`, command.FirmID, command.TaxYear).
		Scan(&result.DatasetID, &storedHash, &result.RowCount)
	if err == nil {
		if len(storedHash) != len(result.ContentSHA256) {
			return app.DatasetResult{}, app.ErrInvariant
		}
		copy(result.ContentSHA256[:], storedHash)
		if result.ContentSHA256 != contentHash || result.RowCount != copied {
			return app.DatasetResult{}, app.ErrConflict
		}
		result.Existing = true
		if err := commit(ctx, tx); err != nil {
			return app.DatasetResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return app.DatasetResult{}, mapError(err)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO firms (id) VALUES ($1) ON CONFLICT DO NOTHING`, command.FirmID); err != nil {
		return app.DatasetResult{}, mapError(err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO datasets (firm_id, tax_year, content_sha256, source_row_count)
		VALUES ($1, $2, $3, $4) RETURNING id`, command.FirmID, command.TaxYear, contentHash[:], copied).Scan(&result.DatasetID); err != nil {
		return app.DatasetResult{}, mapError(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO clients (firm_id, id)
		SELECT DISTINCT $1, client_id FROM payment_stage ON CONFLICT DO NOTHING`, command.FirmID); err != nil {
		return app.DatasetResult{}, mapError(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO payments (
			dataset_id, source_row_number, firm_id, client_id, vendor_name, vendor_tin,
			vendor_identity, payment_date, amount_cents, payment_method,
			backup_withholding_cents, memo
		)
		SELECT $1, source_row_number, $2, client_id, btrim(vendor_name), NULLIF(btrim(vendor_tin), ''),
			CASE
				WHEN btrim(vendor_tin) = '' THEN 'missing-tin:' || translate(regexp_replace(btrim(vendor_name), '[[:space:]]+', ' ', 'g'), 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz')
				WHEN regexp_replace(vendor_tin, '[ -]', '', 'g') ~ '^[0-9]{9}$' THEN 'tin:' || regexp_replace(vendor_tin, '[ -]', '', 'g')
				ELSE 'malformed-tin:' || btrim(vendor_tin)
			END,
			payment_date::date, (amount::numeric * 100)::bigint, payment_method,
			(backup_withholding::numeric * 100)::bigint, memo
		FROM payment_stage`, result.DatasetID, command.FirmID); err != nil {
		return app.DatasetResult{}, mapError(err)
	}
	result.ContentSHA256 = contentHash
	result.RowCount = copied
	if err := commit(ctx, tx); err != nil {
		return app.DatasetResult{}, err
	}
	return result, nil
}
