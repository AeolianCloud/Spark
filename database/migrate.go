package database

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migration/*.sql
var migrationFS embed.FS

// MigrationFS exposes the embedded SQL migrations for testing.
// The embed root is this file's directory, so the SQL files live under the
// migration/ subdirectory.
var MigrationFS fs.FS = migrationFS

// Migrate applies all pending SQL migrations embedded in fsys, in ascending
// filename order. Applied versions are tracked in schema_migrations.
// The transaction-level advisory lock, schema_migrations bookkeeping and the
// migration statements all run inside a single transaction: concurrent
// instances serialize on the lock, and it is released automatically when the
// transaction ends (commit or rollback), so it can never leak.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire conn: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	slog.Info("migrate: waiting for migration advisory lock")
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('spark_migrations'))"); err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := tx.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("migrate: query applied versions: %w", err)
	}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return fmt.Errorf("migrate: scan version: %w", err)
		}
		applied[version] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrate: read applied versions: %w", err)
	}

	pending, err := pendingMigrations(fsys, applied)
	if err != nil {
		return err
	}

	for _, name := range pending {
		version := strings.TrimSuffix(name, ".sql")
		sqlContent, err := fs.ReadFile(fsys, "migration/"+name)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sqlContent)); err != nil {
			return fmt.Errorf("migrate: apply %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
			return fmt.Errorf("migrate: record %s: %w", version, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: commit: %w", err)
	}
	return nil
}

// migrationFiles returns the names of the *.sql files under the migration/
// directory of fsys, sorted in ascending filename (numeric) order.
func migrationFiles(fsys fs.FS) ([]string, error) {
	files, err := fs.ReadDir(fsys, "migration")
	if err != nil {
		return nil, fmt.Errorf("migrate: read dir: %w", err)
	}
	var names []string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".sql") {
			names = append(names, f.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// pendingMigrations returns the migration file names from fsys that are not
// present in the applied set, in ascending execution order. It is pure logic
// (no DB access), so it can be unit tested without a PostgreSQL connection.
func pendingMigrations(fsys fs.FS, applied map[string]bool) ([]string, error) {
	names, err := migrationFiles(fsys)
	if err != nil {
		return nil, err
	}
	var pending []string
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		if !applied[version] {
			pending = append(pending, name)
		}
	}
	return pending, nil
}
