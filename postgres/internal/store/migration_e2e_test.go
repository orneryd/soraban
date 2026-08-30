package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const defaultOwnerDatabaseURL = "postgres://readiness:readiness-local-only@127.0.0.1:55432/readiness?sslmode=disable"

func ownerDatabaseURL() string {
	if value := os.Getenv("MIGRATION_DATABASE_URL"); value != "" {
		return value
	}
	return defaultOwnerDatabaseURL
}

func migrationSQL(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate migration test")
	}
	payload, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), "..", "..", "db", "migrations", "000001_initial.sql"))
	if err != nil {
		t.Fatalf("read initial migration: %v", err)
	}
	return string(payload)
}

func createMigrationDatabase(t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	config, err := pgx.ParseConfig(ownerDatabaseURL())
	if err != nil {
		t.Fatalf("parse owner database URL: %v", err)
	}
	config.Database = "postgres"
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect database administrator: %v", err)
	}
	databaseName := fmt.Sprintf("readiness_migration_%d_%d", time.Now().UnixNano(), fixtureSequence.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+databaseName+" OWNER readiness"); err != nil {
		admin.Close(ctx)
		t.Fatalf("create migration database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = admin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+databaseName+" WITH (FORCE)")
		_ = admin.Close(cleanupCtx)
	})

	config.Database = databaseName
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	database, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect migration database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	return database, databaseName
}

func TestInitialMigrationFreshE2E(t *testing.T) {
	requirePostgresE2E(t)
	database, _ := createMigrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := database.Exec(ctx, migrationSQL(t)); err != nil {
		t.Fatalf("apply initial migration: %v", err)
	}

	var version int64
	if err := database.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 1 {
		t.Fatalf("schema version = %d, want 1", version)
	}
	var tableCount int
	if err := database.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema='public' AND table_name IN (
			'firms','clients','datasets','payments','determinations','filings',
			'submission_batches','batch_filings','rate_gates','api_call_log'
		)`).Scan(&tableCount); err != nil {
		t.Fatalf("count application tables: %v", err)
	}
	if tableCount != 10 {
		t.Fatalf("application table count = %d, want 10", tableCount)
	}
	var securedTables int
	if err := database.QueryRow(ctx, `
		SELECT count(*) FROM pg_class
		WHERE relnamespace='public'::regnamespace AND relname IN (
			'firms','clients','datasets','payments','determinations','filings',
			'submission_batches','batch_filings','rate_gates','api_call_log'
		) AND relrowsecurity AND relforcerowsecurity`).Scan(&securedTables); err != nil {
		t.Fatalf("count secured tables: %v", err)
	}
	if securedTables != 10 {
		t.Fatalf("forced-RLS table count = %d, want 10", securedTables)
	}
}

func TestInitialMigrationRollbackE2E(t *testing.T) {
	requirePostgresE2E(t)
	database, _ := createMigrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	broken := strings.Replace(migrationSQL(t), "\nCOMMIT;", "\nSELECT 1 / 0;\nCOMMIT;", 1)
	if _, err := database.Exec(ctx, broken); err == nil {
		t.Fatal("broken migration unexpectedly succeeded")
	}
	if _, err := database.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatalf("rollback failed migration: %v", err)
	}
	var schemaTableExists bool
	if err := database.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&schemaTableExists); err != nil {
		t.Fatalf("inspect rolled-back migration: %v", err)
	}
	if schemaTableExists {
		t.Fatal("failed migration left schema_migrations behind")
	}
}
