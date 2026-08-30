package store

import (
	"context"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"

	"readiness/postgres/internal/app"
)

type clientStatusFacts struct {
	status    app.ClientStatus
	attention bool
	awaiting  bool
}

func (store *Store) ClientStatus(ctx context.Context, query app.ClientStatusQuery) (app.ClientStatus, error) {
	if query.ClientID == "" {
		return app.ClientStatus{}, errors.New("invalid client status query")
	}
	tx, err := store.beginFirm(ctx, query.FirmID)
	if err != nil {
		return app.ClientStatus{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	facts, err := readClientStatus(ctx, tx, query.FirmID, query.ClientID)
	if err != nil {
		return app.ClientStatus{}, err
	}
	if err := commit(ctx, tx); err != nil {
		return app.ClientStatus{}, err
	}
	return facts.status, nil
}

func readClientStatus(ctx context.Context, tx pgx.Tx, firmID, clientID string) (clientStatusFacts, error) {
	var facts clientStatusFacts
	facts.status.FirmID = firmID
	facts.status.ClientID = clientID
	err := tx.QueryRow(ctx, `
		SELECT count(f.id),
		       count(f.id) FILTER (WHERE f.state='blocked_preflight'),
		       count(f.id) FILTER (WHERE f.state='ready'),
		       count(f.id) FILTER (WHERE f.state='batched'),
		       count(f.id) FILTER (WHERE f.state='accepted'),
		       count(f.id) FILTER (WHERE f.state='rejected'),
		       (count(f.id) FILTER (WHERE f.state IN ('blocked_preflight','rejected')) > 0
		          OR EXISTS (SELECT 1 FROM submission_batches b WHERE b.firm_id=$1 AND b.client_id=$2 AND b.state='invariant_failed')
		          OR EXISTS (SELECT 1 FROM submission_batches b WHERE b.firm_id=$1 AND b.client_id=$2
		                     AND b.state IN ('submitted','awaiting_ack')
		                     AND COALESCE(b.submitted_at,b.next_action_at) < clock_timestamp()-interval '30 minutes')),
		       EXISTS (SELECT 1 FROM submission_batches b WHERE b.firm_id=$1 AND b.client_id=$2
		               AND b.state IN ('submitting','submit_unknown','submitted','awaiting_ack'))
		FROM clients c LEFT JOIN filings f ON f.firm_id=c.firm_id AND f.client_id=c.id
		WHERE c.firm_id=$1 AND c.id=$2 GROUP BY c.id`, firmID, clientID).Scan(
		&facts.status.Counts.Required, &facts.status.Counts.Blocked, &facts.status.Counts.Ready,
		&facts.status.Counts.Pending, &facts.status.Counts.Accepted, &facts.status.Counts.Rejected,
		&facts.attention, &facts.awaiting,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return facts, app.ErrNotFound
		}
		return facts, mapError(err)
	}
	facts.status.Headline = headline(facts)
	return facts, nil
}

func headline(facts clientStatusFacts) app.HeadlineState {
	if facts.attention {
		return app.HeadlineNeedsAttention
	}
	if facts.awaiting {
		return app.HeadlineAwaitingIRS
	}
	if facts.status.Counts.Ready == 0 && facts.status.Counts.Pending == 0 {
		return app.HeadlineFullyFiled
	}
	return app.HeadlinePartiallyFiled
}

func (store *Store) FirmStatus(ctx context.Context, query app.FirmStatusQuery) (app.FirmStatus, error) {
	tx, err := store.beginFirm(ctx, query.FirmID)
	if err != nil {
		return app.FirmStatus{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT id FROM clients WHERE firm_id=$1 ORDER BY id`, query.FirmID)
	if err != nil {
		return app.FirmStatus{}, mapError(err)
	}
	var clientIDs []string
	for rows.Next() {
		var clientID string
		if err := rows.Scan(&clientID); err != nil {
			rows.Close()
			return app.FirmStatus{}, mapError(err)
		}
		clientIDs = append(clientIDs, clientID)
	}
	if err := rows.Err(); err != nil {
		return app.FirmStatus{}, mapError(err)
	}
	rows.Close()
	if len(clientIDs) == 0 {
		return app.FirmStatus{}, app.ErrNotFound
	}
	result := app.FirmStatus{FirmID: query.FirmID, Headline: app.HeadlineFullyFiled}
	for _, clientID := range clientIDs {
		facts, err := readClientStatus(ctx, tx, query.FirmID, clientID)
		if err != nil {
			return app.FirmStatus{}, err
		}
		result.Clients = append(result.Clients, facts.status)
		result.Counts.Required += facts.status.Counts.Required
		result.Counts.Blocked += facts.status.Counts.Blocked
		result.Counts.Ready += facts.status.Counts.Ready
		result.Counts.Pending += facts.status.Counts.Pending
		result.Counts.Accepted += facts.status.Counts.Accepted
		result.Counts.Rejected += facts.status.Counts.Rejected
		if facts.status.Headline == app.HeadlineNeedsAttention {
			result.Headline = app.HeadlineNeedsAttention
		} else if result.Headline != app.HeadlineNeedsAttention && facts.status.Headline == app.HeadlineAwaitingIRS {
			result.Headline = app.HeadlineAwaitingIRS
		} else if result.Headline == app.HeadlineFullyFiled && facts.status.Headline == app.HeadlinePartiallyFiled {
			result.Headline = app.HeadlinePartiallyFiled
		}
	}
	if err := commit(ctx, tx); err != nil {
		return app.FirmStatus{}, err
	}
	return result, nil
}

func (store *Store) Exceptions(ctx context.Context, query app.ExceptionsQuery) ([]app.ExceptionGroup, error) {
	tx, err := store.beginFirm(ctx, query.FirmID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT exception_type, client_id, filing_id, batch_id, vendor_name, failure_code FROM (
			SELECT CASE f.preflight_reason
			           WHEN 'TIN_MISSING' THEN 'MISSING_TIN'
			           WHEN 'TIN_MALFORMED' THEN 'INVALID_TIN'
			           WHEN 'TIN_INVALID' THEN 'INVALID_TIN'
			           WHEN 'AMOUNT_INVALID' THEN 'INVALID_AMOUNT' END AS exception_type,
			       f.client_id, f.id AS filing_id, 0::bigint AS batch_id,
			       f.vendor_display_name AS vendor_name, f.preflight_reason AS failure_code
			FROM filings f WHERE f.firm_id=$1 AND f.state='blocked_preflight'
			UNION ALL
			SELECT 'FILING_REJECTED_' || f.rejection_reason, f.client_id, f.id, COALESCE(bf.batch_id,0),
			       f.vendor_display_name, f.rejection_reason
			FROM filings f LEFT JOIN batch_filings bf ON bf.filing_id=f.id
			WHERE f.firm_id=$1 AND f.state='rejected'
			UNION ALL
			SELECT CASE WHEN b.retry_exhausted_at IS NOT NULL THEN 'RETRY_EXHAUSTED' ELSE 'INVARIANT_FAILURE' END,
			       b.client_id, 0, b.id, '', COALESCE(b.last_error_code,'INVARIANT')
			FROM submission_batches b WHERE b.firm_id=$1 AND b.state='invariant_failed'
			UNION ALL
			SELECT 'SUBMISSION_UNACKNOWLEDGED', b.client_id, 0, b.id, '', 'ACK_DELAY'
			FROM submission_batches b WHERE b.firm_id=$1 AND b.state IN ('submitted','awaiting_ack')
			  AND COALESCE(b.submitted_at,b.next_action_at) < clock_timestamp()-interval '30 minutes'
		) exceptions
		WHERE ($2='' OR client_id=$2)
		ORDER BY exception_type, client_id, filing_id, batch_id`, query.FirmID, query.ClientID)
	if err != nil {
		return nil, mapError(err)
	}
	grouped := make(map[string]*app.ExceptionGroup)
	var order []string
	for rows.Next() {
		var exceptionType string
		var item app.ExceptionItem
		if err := rows.Scan(&exceptionType, &item.ClientID, &item.FilingID, &item.BatchID, &item.VendorDisplayName, &item.FailureCode); err != nil {
			rows.Close()
			return nil, mapError(err)
		}
		group := grouped[exceptionType]
		if group == nil {
			group = &app.ExceptionGroup{Type: exceptionType}
			grouped[exceptionType] = group
			order = append(order, exceptionType)
		}
		group.Count++
		group.Items = append(group.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	sort.Strings(order)
	result := make([]app.ExceptionGroup, 0, len(order))
	for _, exceptionType := range order {
		result = append(result, *grouped[exceptionType])
	}
	if err := commit(ctx, tx); err != nil {
		return nil, err
	}
	return result, nil
}
