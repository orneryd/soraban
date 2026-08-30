package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"readiness/postgres/internal/app"
)

type Store struct {
	pool *pgxpool.Pool
}

var _ app.DataLifecycle = (*Store)(nil)

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	config.ConnConfig.RuntimeParams["statement_timeout"] = "30s"
	config.ConnConfig.RuntimeParams["lock_timeout"] = "10s"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("open database")
	}
	store := &Store{pool: pool}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("connect database")
	}
	var version int64
	err = pool.QueryRow(ctx, "SELECT COALESCE(max(version), 0) FROM schema_migrations").Scan(&version)
	if err != nil || version != app.RequiredSchemaVersion {
		pool.Close()
		return nil, app.ErrSchemaVersion
	}
	return store, nil
}

func (store *Store) Close() {
	if store != nil && store.pool != nil {
		store.pool.Close()
	}
}

func validateFirmID(firmID string) error {
	if strings.TrimSpace(firmID) == "" || len(firmID) > 128 || strings.ContainsAny(firmID, "\x00\r\n") {
		return errors.New("invalid firm identity")
	}
	return nil
}

func (store *Store) beginFirm(ctx context.Context, firmID string) (pgx.Tx, error) {
	if err := validateFirmID(firmID); err != nil {
		return nil, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, errors.New("begin database transaction")
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.firm_id', $1, true)", firmID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, errors.New("set firm context")
	}
	return tx, nil
}

func commit(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) {
		switch pgError.Code {
		case "23505":
			return app.ErrConflict
		case "23503":
			return app.ErrNotFound
		case "23514", "22000", "22001", "22003", "22007", "22P02", "55000":
			return app.ErrInvariant

		}
	}
	return fmt.Errorf("database operation: %w", err)
}
