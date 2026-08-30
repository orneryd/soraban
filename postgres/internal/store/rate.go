package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"readiness/postgres/internal/app"
)

const (
	minimumCallSpacing = 3100 * time.Millisecond
	crashFenceDuration = 60 * time.Second
)

type callPermit struct {
	mu       sync.Mutex
	conn     *pgxpool.Conn
	firmID   string
	logID    int64
	finished bool
	released bool
}

func beginFirmOnConn(ctx context.Context, conn *pgxpool.Conn, firmID string) (pgx.Tx, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, errors.New("begin rate transaction")
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.firm_id', $1, true)", firmID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, errors.New("set rate firm context")
	}
	return tx, nil
}

func (store *Store) AcquireCallPermit(ctx context.Context, command app.AcquireCallCommand) (app.CallPermit, error) {
	if err := validateFirmID(command.FirmID); err != nil || command.BatchID <= 0 || (command.Operation != app.OperationSubmit && command.Operation != app.OperationStatus) {
		return nil, errors.New("invalid call permit command")
	}
	conn, err := store.pool.Acquire(ctx)
	if err != nil {
		return nil, errors.New("acquire rate connection")
	}
	locked := false
	defer func() {
		if !locked {
			conn.Release()
		}
	}()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 1))", command.FirmID); err != nil {
		return nil, mapError(err)
	}
	locked = true

	tx, err := beginFirmOnConn(ctx, conn, command.FirmID)
	if err != nil {
		releaseAdvisoryLock(conn, command.FirmID)
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO rate_gates (firm_id) VALUES ($1) ON CONFLICT DO NOTHING`, command.FirmID); err != nil {
		_ = tx.Rollback(ctx)
		releaseAdvisoryLock(conn, command.FirmID)
		return nil, mapError(err)
	}
	var allowedAt, databaseNow time.Time
	if err := tx.QueryRow(ctx, `
		SELECT GREATEST(next_call_at, crash_fence_until, '1970-01-01 00:00:00+00'::timestamptz), clock_timestamp()
		FROM rate_gates WHERE firm_id=$1 FOR UPDATE`, command.FirmID).Scan(&allowedAt, &databaseNow); err != nil {
		_ = tx.Rollback(ctx)
		releaseAdvisoryLock(conn, command.FirmID)
		return nil, mapError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		releaseAdvisoryLock(conn, command.FirmID)
		return nil, mapError(err)
	}
	if wait := allowedAt.Sub(databaseNow); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			releaseAdvisoryLock(conn, command.FirmID)
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	tx, err = beginFirmOnConn(ctx, conn, command.FirmID)
	if err != nil {
		releaseAdvisoryLock(conn, command.FirmID)
		return nil, err
	}
	var logID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO api_call_log (firm_id, batch_id, operation)
		SELECT $1, b.id, $3 FROM submission_batches b WHERE b.id=$2 AND b.firm_id=$1
		RETURNING id`, command.FirmID, command.BatchID, command.Operation).Scan(&logID); err != nil {
		_ = tx.Rollback(ctx)
		releaseAdvisoryLock(conn, command.FirmID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, app.ErrNotFound
		}
		return nil, mapError(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE rate_gates SET crash_fence_until=clock_timestamp()+interval '60 seconds', updated_at=clock_timestamp()
		WHERE firm_id=$1`, command.FirmID); err != nil {
		_ = tx.Rollback(ctx)
		releaseAdvisoryLock(conn, command.FirmID)
		return nil, mapError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		releaseAdvisoryLock(conn, command.FirmID)
		return nil, mapError(err)
	}
	return &callPermit{conn: conn, firmID: command.FirmID, logID: logID}, nil
}

func (permit *callPermit) Finish(ctx context.Context, result app.CallResult) error {
	permit.mu.Lock()
	defer permit.mu.Unlock()
	if permit.finished || permit.released {
		return app.ErrInvalidTransition
	}
	if result.Outcome != app.CallCompleted && result.Outcome != app.CallAmbiguous && result.Outcome != app.CallRetryableError && result.Outcome != app.CallTerminalError {
		return errors.New("invalid call outcome")
	}
	if result.HTTPStatus < 0 || result.HTTPStatus > 599 || !validFailureCode(result.ErrorCode) {
		return errors.New("invalid call result")
	}
	tx, err := beginFirmOnConn(ctx, permit.conn, permit.firmID)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE api_call_log SET completed_at=clock_timestamp(), outcome=$1,
		    http_status=NULLIF($2,0), error_code=NULLIF($3,'')
		WHERE id=$4 AND firm_id=$5 AND completed_at IS NULL`, result.Outcome, result.HTTPStatus, result.ErrorCode, permit.logID, permit.firmID)
	if err != nil {
		_ = tx.Rollback(ctx)
		return mapError(err)
	}
	if tag.RowsAffected() != 1 {
		_ = tx.Rollback(ctx)
		return app.ErrInvalidTransition
	}
	ambiguous := result.Outcome == app.CallAmbiguous
	if _, err := tx.Exec(ctx, `
		UPDATE rate_gates
		SET next_call_at=clock_timestamp()+interval '3.1 seconds',
		    crash_fence_until=CASE WHEN $2 THEN clock_timestamp()+interval '60 seconds' ELSE '-infinity'::timestamptz END,
		    updated_at=clock_timestamp()
		WHERE firm_id=$1`, permit.firmID, ambiguous); err != nil {
		_ = tx.Rollback(ctx)
		return mapError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mapError(err)
	}
	permit.finished = true
	permit.releaseLocked()
	return nil
}

func (permit *callPermit) Close() error {
	permit.mu.Lock()
	defer permit.mu.Unlock()
	permit.releaseLocked()
	return nil
}

func (permit *callPermit) releaseLocked() {
	if permit.released {
		return
	}
	releaseAdvisoryLock(permit.conn, permit.firmID)
	permit.released = true
}

func releaseAdvisoryLock(conn *pgxpool.Conn, firmID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 1))", firmID)
	conn.Release()
}
