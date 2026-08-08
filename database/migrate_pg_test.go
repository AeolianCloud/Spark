//go:build pg

// 针对真实 PostgreSQL 实例的迁移真实执行契约测试（提案
// auto-scan-pve-storage 任务 1.2/10.1 的补充）：0011_storage_zone_and_scan.sql
// 的 PL/pgSQL 归置块有三条路径，0012_storage_nodes.sql 的 nodes 列随全量
// 迁移应用，内容回归测试（migrate_test.go）无法验证
// 实际可执行性，必须真库执行。排除在默认构建之外，通过 -tags=pg 运行：
//
//	SPARK_TEST_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' \
//	  go test -tags=pg ./database/ -count=1 -run TestPGMigrationStorageZone
//
// 测试套件从 SPARK_TEST_DSN 连接数据库（默认值
// postgres://spark:spark@127.0.0.1:5432/spark_test，与 repository 包
// 的 pg 测试相同，共享同一测试库）。隔离策略：每条测试路径创建独立
// schema，连接池 search_path 指向该 schema，迁移与断言全部在该 schema
// 内进行，互不污染；测试结束 DROP SCHEMA CASCADE 清理。
//
// 三条路径：
//   - 空库路径：全新 schema 直接跑 0001-0012 全部迁移，验证空 zones +
//     空 storage_types 安全继续，最终表结构符合契约（zone_id NOT NULL、
//     无 display_name、(zone_id, pve_storage) 唯一、name 可空、nodes
//     非空默认 ''）。
//   - 单 zone 归置路径：先跑 0012 之前的迁移，预置一个 zone 与两条
//     存量 storage_types，再跑 0011-0012，验证两行归入该 zone、
//     display_name 已删、其余列默认值正确（含 nodes 默认 ''）。
//   - 多 zone 中止路径：先跑 0012 之前的迁移，预置两个 zone 与一条
//     存量 storage_types，再跑 0011（0012 之前的版本），验证
//     RAISE EXCEPTION 中止且整个迁移事务回滚（schema_migrations 无
//     记录、ADD COLUMN 与唯一索引均未落库、存量数据完好无损）。
package database

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgMigrateDSN 返回测试数据库 DSN；当 SPARK_TEST_DSN 未设置时默认
// 使用本地 spark 测试数据库（与 repository 包 pg 测试一致）。
func pgMigrateDSN() string {
	if dsn := os.Getenv("SPARK_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://spark:spark@127.0.0.1:5432/spark_test"
}

// excludeFS 包装 fs.FS：ReadDir/ReadFile 时剔除指定路径。用于分阶段
// 应用迁移——先跑 0011 之前的全部迁移并预置存量数据，再单独应用 0011，
// 使归置路径可以在真实数据库上演练。
type excludeFS struct {
	inner   fs.FS
	exclude map[string]bool
}

func (e excludeFS) Open(name string) (fs.File, error) { return e.inner.Open(name) }

func (e excludeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(e.inner, name)
	if err != nil {
		return nil, err
	}
	kept := make([]fs.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if !e.exclude[name+"/"+entry.Name()] {
			kept = append(kept, entry)
		}
	}
	return kept, nil
}

func (e excludeFS) ReadFile(name string) ([]byte, error) {
	if e.exclude[name] {
		return nil, os.ErrNotExist
	}
	return fs.ReadFile(e.inner, name)
}

// withoutMigration 返回剔除指定迁移文件（如 0011_storage_zone_and_scan）
// 的 MigrationFS 包装，供分阶段应用迁移使用。
func withoutMigration(version string) fs.FS {
	return excludeFS{
		inner:   MigrationFS,
		exclude: map[string]bool{"migration/" + version + ".sql": true},
	}
}

// pgMigrateSchema 创建独立 schema 并返回 search_path 指向该 schema 的
// 连接池；测试结束自动关闭连接池并 DROP SCHEMA CASCADE。返回 schema 名
// 与连接池。
func pgMigrateSchema(t *testing.T, ctx context.Context) (string, *pgxpool.Pool) {
	t.Helper()

	admin, err := New(ctx, pgMigrateDSN())
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	schema := fmt.Sprintf("mig_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(pgMigrateDSN())
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create schema pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return schema, pool
}

// pgStorageColumn 查询 storage_types 指定列的 information_schema 元数据：
// 返回列是否存在、is_nullable（YES/NO）与 column_default（可空）。
func pgStorageColumn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, column string) (exists bool, nullable string, colDefault *string) {
	t.Helper()
	err := pool.QueryRow(ctx, `
		SELECT is_nullable, column_default FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'storage_types' AND column_name = $2`,
		schema, column).Scan(&nullable, &colDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		t.Fatalf("query column %s: %v", column, err)
	}
	return true, nullable, colDefault
}

// pgUniqueIndexExists 判断指定 schema 下是否存在给定名字的索引。
func pgUniqueIndexExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, indexName string) bool {
	t.Helper()
	var indexDef string
	err := pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = $1 AND indexname = $2`,
		schema, indexName).Scan(&indexDef)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("query index %s: %v", indexName, err)
	}
	return true
}

// TestPGMigrationStorageZoneEmptyDB 空库路径：全新 schema（无 zones、
// 无 storage_types 数据）直接应用 0001-0012 全部迁移，空表可安全加
// NOT NULL 约束；最终表结构符合契约。
func TestPGMigrationStorageZoneEmptyDB(t *testing.T) {
	ctx := context.Background()
	schema, pool := pgMigrateSchema(t, ctx)

	if err := Migrate(ctx, pool, MigrationFS); err != nil {
		t.Fatalf("migrate all: %v", err)
	}

	// schema_migrations 记账到 0012（0011 与 0012 均已在全量迁移中应用）。
	for _, version := range []string{"0011_storage_zone_and_scan", "0012_storage_nodes"} {
		var recorded int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM schema_migrations WHERE version = $1",
			version,
		).Scan(&recorded); err != nil || recorded != 1 {
			t.Fatalf("%s recorded = %d, err = %v; want 1", version, recorded, err)
		}
	}

	// zone_id NOT NULL（空表加约束安全）。
	exists, nullable, _ := pgStorageColumn(t, ctx, pool, schema, "zone_id")
	if !exists || nullable != "NO" {
		t.Fatalf("zone_id: exists=%v nullable=%q, want NOT NULL", exists, nullable)
	}

	// enabled 默认 true 且非空。
	exists, nullable, def := pgStorageColumn(t, ctx, pool, schema, "enabled")
	if !exists || nullable != "NO" || def == nil || *def != "true" {
		t.Fatalf("enabled: exists=%v nullable=%q default=%v, want NOT NULL default true", exists, nullable, def)
	}

	// name 可空；type/content 可空存在。
	for _, col := range []string{"name", "type", "content"} {
		exists, nullable, _ := pgStorageColumn(t, ctx, pool, schema, col)
		if !exists || nullable != "YES" {
			t.Fatalf("%s: exists=%v nullable=%q, want nullable", col, exists, nullable)
		}
	}

	// nodes 列存在且非空，默认 ''（空串 = 不限制节点，设计 D8）。
	exists, nullable, def = pgStorageColumn(t, ctx, pool, schema, "nodes")
	if !exists || nullable != "NO" || def == nil || *def != "''::text" {
		t.Fatalf("nodes: exists=%v nullable=%q default=%v, want NOT NULL default ''", exists, nullable, def)
	}

	// display_name 已移除。
	if exists, _, _ := pgStorageColumn(t, ctx, pool, schema, "display_name"); exists {
		t.Fatal("display_name 列仍存在")
	}

	// (zone_id, pve_storage) 唯一索引存在，name 全局唯一索引已移除。
	if !pgUniqueIndexExists(t, ctx, pool, schema, "storage_types_zone_pve_storage_key") {
		t.Fatal("缺少 (zone_id, pve_storage) 唯一索引")
	}
	if pgUniqueIndexExists(t, ctx, pool, schema, "storage_types_name_key") {
		t.Fatal("storage_types_name_key 仍存在")
	}

	// 空库无存量行。
	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM storage_types").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("storage_types rows = %d, err = %v; want 0", rows, err)
	}
}

// TestPGMigrationStorageZoneSingleZone 单 zone 归置路径：预置一个 zone
// 与两条存量 storage_types，应用 0011 后两行归入该 zone，display_name
// 删除，enabled 默认 true、type/content 为 NULL，唯一索引真实生效。
func TestPGMigrationStorageZoneSingleZone(t *testing.T) {
	ctx := context.Background()
	schema, pool := pgMigrateSchema(t, ctx)

	// 先应用 0011 之前的迁移，再预置存量数据。
	if err := Migrate(ctx, pool, withoutMigration("0011_storage_zone_and_scan")); err != nil {
		t.Fatalf("migrate to 0010: %v", err)
	}
	var zoneID int64
	if err := pool.QueryRow(ctx, "INSERT INTO zones (name) VALUES ('mig-zone-a') RETURNING id").Scan(&zoneID); err != nil {
		t.Fatalf("insert zone: %v", err)
	}
	wantName := map[string]string{"local-1": "业务一", "local-2": "业务二"}
	for _, st := range []struct{ name, display, pve string }{
		{"业务一", "展示一", "local-1"},
		{"业务二", "展示二", "local-2"},
	} {
		if _, err := pool.Exec(ctx,
			"INSERT INTO storage_types (name, display_name, pve_storage) VALUES ($1, $2, $3)",
			st.name, st.display, st.pve); err != nil {
			t.Fatalf("insert storage_type %s: %v", st.pve, err)
		}
	}

	// 应用 0011：单 zone 应全量归置并成功。
	if err := Migrate(ctx, pool, MigrationFS); err != nil {
		t.Fatalf("migrate 0011: %v", err)
	}

	// 两行归入该 zone；name 保留；enabled 默认 true；type/content 为 NULL
	// （尚未扫描）。
	for pve, wantName := range wantName {
		var gotZone int64
		var name string
		var enabled bool
		var typ, content *string
		if err := pool.QueryRow(ctx,
			"SELECT zone_id, name, enabled, type, content FROM storage_types WHERE pve_storage = $1", pve,
		).Scan(&gotZone, &name, &enabled, &typ, &content); err != nil {
			t.Fatalf("scan %s: %v", pve, err)
		}
		if gotZone != zoneID {
			t.Fatalf("%s zone_id = %d, want %d", pve, gotZone, zoneID)
		}
		if name != wantName {
			t.Fatalf("%s name = %q, want %q", pve, name, wantName)
		}
		if !enabled {
			t.Fatalf("%s enabled = false, want true", pve)
		}
		if typ != nil || content != nil {
			t.Fatalf("%s type/content 应为 NULL（未扫描），got type=%v content=%v", pve, typ, content)
		}
	}

	// display_name 已删除；zone_id NOT NULL；(zone_id, pve_storage) 唯一索引存在。
	if exists, _, _ := pgStorageColumn(t, ctx, pool, schema, "display_name"); exists {
		t.Fatal("display_name 列仍存在")
	}
	if exists, nullable, _ := pgStorageColumn(t, ctx, pool, schema, "zone_id"); !exists || nullable != "NO" {
		t.Fatalf("zone_id: exists=%v nullable=%q, want NOT NULL", exists, nullable)
	}
	if !pgUniqueIndexExists(t, ctx, pool, schema, "storage_types_zone_pve_storage_key") {
		t.Fatal("缺少 (zone_id, pve_storage) 唯一索引")
	}

	// 0012 随全量迁移应用：nodes 列存在、非空、默认 ''（存量行不限制节点）。
	if exists, nullable, def := pgStorageColumn(t, ctx, pool, schema, "nodes"); !exists || nullable != "NO" || def == nil || *def != "''::text" {
		t.Fatalf("nodes: exists=%v nullable=%q default=%v, want NOT NULL default ''", exists, nullable, def)
	}
	// 存量行未扫描：nodes 保持默认 ''（空 = 不限制节点）。
	var nodes string
	if err := pool.QueryRow(ctx, "SELECT nodes FROM storage_types WHERE pve_storage='local-1'").Scan(&nodes); err != nil {
		t.Fatalf("scan nodes: %v", err)
	}
	if nodes != "" {
		t.Fatalf("legacy row nodes = %q, want '' (unlimited)", nodes)
	}

	// 唯一约束真实生效：同 zone 同 pve_storage 插入应违反唯一键。
	_, err := pool.Exec(ctx,
		"INSERT INTO storage_types (zone_id, name, pve_storage) VALUES ($1, 'dup', 'local-1')", zoneID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate insert err = %v, want SQLSTATE 23505", err)
	}
}

// TestPGMigrationStorageZoneMultiZoneAbort 多 zone 中止路径：预置两个
// zone 与一条存量 storage_types，应用 0011 应 RAISE EXCEPTION 中止；
// 整个迁移事务回滚——schema_migrations 无记录、zone_id 列与唯一索引
// 均未落库、display_name 与存量数据完好。
func TestPGMigrationStorageZoneMultiZoneAbort(t *testing.T) {
	ctx := context.Background()
	schema, pool := pgMigrateSchema(t, ctx)

	// 先应用 0011 之前的迁移，再预置存量数据。
	if err := Migrate(ctx, pool, withoutMigration("0011_storage_zone_and_scan")); err != nil {
		t.Fatalf("migrate to 0010: %v", err)
	}
	for _, z := range []string{"mig-zone-1", "mig-zone-2"} {
		if _, err := pool.Exec(ctx, "INSERT INTO zones (name) VALUES ($1)", z); err != nil {
			t.Fatalf("insert zone %s: %v", z, err)
		}
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO storage_types (name, display_name, pve_storage) VALUES ('业务一', '展示一', 'local-1')"); err != nil {
		t.Fatalf("insert storage_type: %v", err)
	}

	// 0011 应中止并给出可读提示。
	err := Migrate(ctx, pool, MigrationFS)
	if err == nil {
		t.Fatal("migrate 0011: want error, got nil")
	}
	if !strings.Contains(err.Error(), "无法自动归置") {
		t.Fatalf("migrate error = %v, want 多 zone 中止提示", err)
	}

	// 回滚语义：迁移整体在 Migrate 的单个事务内，失败即全量回滚。
	var recorded int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM schema_migrations WHERE version = '0011_storage_zone_and_scan'",
	).Scan(&recorded); err != nil || recorded != 0 {
		t.Fatalf("0011 recorded = %d, err = %v; want 0（未提交）", recorded, err)
	}
	// ADD COLUMN zone_id 已回滚；display_name 列仍在。
	if exists, _, _ := pgStorageColumn(t, ctx, pool, schema, "zone_id"); exists {
		t.Fatal("zone_id 列仍存在，迁移未回滚")
	}
	if exists, _, _ := pgStorageColumn(t, ctx, pool, schema, "display_name"); !exists {
		t.Fatal("display_name 列已被删除，迁移未回滚")
	}
	if pgUniqueIndexExists(t, ctx, pool, schema, "storage_types_zone_pve_storage_key") {
		t.Fatal("(zone_id, pve_storage) 唯一索引仍存在，迁移未回滚")
	}

	// 存量数据完好无损。
	var cnt int
	var name, display, pve string
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM storage_types").Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("storage_types rows = %d, err = %v; want 1", cnt, err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT name, display_name, pve_storage FROM storage_types",
	).Scan(&name, &display, &pve); err != nil {
		t.Fatalf("scan storage_type: %v", err)
	}
	if name != "业务一" || display != "展示一" || pve != "local-1" {
		t.Fatalf("存量数据被破坏: name=%q display=%q pve=%q", name, display, pve)
	}
}
