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

// MigrationFS 暴露内嵌的 SQL 迁移，供测试使用。
// embed 根目录是本文件所在目录，因此 SQL 文件位于 migration/ 子目录下。
var MigrationFS fs.FS = migrationFS

// Migrate 按文件名升序应用内嵌在 fsys 中所有待执行的 SQL 迁移。
// 已应用的版本记录在 schema_migrations 中。事务级 advisory lock、
// schema_migrations 记账和迁移语句都在同一个事务内执行：并发实例在锁上
// 串行化，锁会在事务结束（提交或回滚）时自动释放，因此永远不会泄漏。
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

// migrationFiles 返回 fsys 的 migration/ 目录下所有 *.sql 文件名，
// 按文件名（数字）升序排序。
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

// pendingMigrations 返回 fsys 中不在已应用集合内的迁移文件名，
// 按升序执行顺序排列。它是纯逻辑（不访问数据库），因此无需
// PostgreSQL 连接即可进行单元测试。
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
