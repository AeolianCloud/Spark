package database

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// TestMigrationFilesOrder 确保迁移在 migration/ 目录下被发现，
// 并按文件名数字升序执行，同时忽略非 SQL 文件和子目录。
func TestMigrationFilesOrder(t *testing.T) {
	fsys := fstest.MapFS{
		"migration/0001_init.sql":   {Data: []byte("-- one")},
		"migration/0002_tables.sql": {Data: []byte("-- two")},
		"migration/0010_more.sql":   {Data: []byte("-- ten")},
		"migration/readme.md":       {Data: []byte("-- ignored")},
		"migration/notes.txt":       {Data: []byte("-- ignored")},
		"migration/nested/x.sql":    {Data: []byte("-- ignored")},
		"other/0001_elsewhere.sql":  {Data: []byte("-- ignored")},
	}

	got, err := migrationFiles(fsys)
	if err != nil {
		t.Fatalf("migrationFiles: %v", err)
	}
	want := []string{"0001_init.sql", "0002_tables.sql", "0010_more.sql"}
	if !slices.Equal(got, want) {
		t.Fatalf("migrationFiles = %v, want %v", got, want)
	}
}

// TestMigrationFilesDirFS 针对真实的目录树（t.TempDir + os.DirFS）
// 而非内存 FS 验证相同的发现逻辑。
func TestMigrationFilesDirFS(t *testing.T) {
	dir := t.TempDir()
	migDir := filepath.Join(dir, "migration")
	if err := os.Mkdir(migDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"0001_init.sql", "0002_tables.sql", "0010_more.sql"} {
		if err := os.WriteFile(filepath.Join(migDir, name), []byte("-- x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got, err := migrationFiles(os.DirFS(dir))
	if err != nil {
		t.Fatalf("migrationFiles: %v", err)
	}
	want := []string{"0001_init.sql", "0002_tables.sql", "0010_more.sql"}
	if !slices.Equal(got, want) {
		t.Fatalf("migrationFiles = %v, want %v", got, want)
	}
}

// TestEmbeddedMigrationsDiscovered 是 go:embed 布局的回归测试：内嵌 FS 的
// 根目录是 database/，因此必须像 Migrate 一样从 migration/ 子目录
// （而非 FS 根目录）读取迁移。
func TestEmbeddedMigrationsDiscovered(t *testing.T) {
	names, err := migrationFiles(MigrationFS)
	if err != nil {
		t.Fatalf("migrationFiles(MigrationFS): %v", err)
	}
	want := []string{"0001_init.sql", "0002_create_tables.sql", "0003_indexes_and_unique.sql", "0004_add_provision_error.sql", "0005_add_node_port.sql", "0006_add_pve_name.sql", "0007_import_vm.sql", "0008_vm_source_and_operations.sql", "0009_image_download.sql", "0010_user_auth.sql"}
	if !slices.Equal(names, want) {
		t.Fatalf("embedded migrations = %v, want %v", names, want)
	}
	for _, name := range names {
		if _, err := fs.ReadFile(MigrationFS, "migration/"+name); err != nil {
			t.Fatalf("read migration/%s: %v", name, err)
		}
	}
}

// TestPendingMigrationsIdempotent 在不使用数据库的情况下模拟完整的迁移周期：
// 第一次执行将所有迁移报告为待执行，一旦它们被标记为已应用，
// 第二次执行将不报告任何内容（空操作的重复运行）。
func TestPendingMigrationsIdempotent(t *testing.T) {
	fsys := fstest.MapFS{
		"migration/0001_init.sql":   {Data: []byte("-- one")},
		"migration/0002_tables.sql": {Data: []byte("-- two")},
	}

	first, err := pendingMigrations(fsys, nil)
	if err != nil {
		t.Fatalf("first pendingMigrations: %v", err)
	}
	wantFirst := []string{"0001_init.sql", "0002_tables.sql"}
	if !slices.Equal(first, wantFirst) {
		t.Fatalf("first run = %v, want %v", first, wantFirst)
	}

	applied := map[string]bool{}
	for _, name := range first {
		applied[strings.TrimSuffix(name, ".sql")] = true
	}

	second, err := pendingMigrations(fsys, applied)
	if err != nil {
		t.Fatalf("second pendingMigrations: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second run should have no pending migrations, got %v", second)
	}

	third, err := pendingMigrations(fsys, applied)
	if err != nil {
		t.Fatalf("third pendingMigrations: %v", err)
	}
	if len(third) != 0 {
		t.Fatalf("repeated runs should stay empty, got %v", third)
	}
}

// TestImageDownloadMigrationContent 对 0009_image_download.sql 的内容做回归
// 校验：迁移按文件名发现、以 schema_migrations 记账，错误的内容不会被
// 发现机制捕获，因此这里断言破坏性变更（DROP node_images）与新增对象
// （download_url 列、image_operations 表及其索引）的关键语句确实存在。
// 注意：迁移只在未被应用时执行一次，下述语句必须在测试环境之外的数据库
// 上验证实际可执行性，本测试仅保证文件内容不被误删改。
func TestImageDownloadMigrationContent(t *testing.T) {
	raw, err := fs.ReadFile(MigrationFS, "migration/0009_image_download.sql")
	if err != nil {
		t.Fatalf("read migration/0009_image_download.sql: %v", err)
	}
	content := string(raw)
	for _, want := range []string{
		"ALTER TABLE images DROP COLUMN node_images;",
		"ALTER TABLE images ADD COLUMN download_url TEXT NOT NULL DEFAULT '';",
		"CREATE TABLE image_operations (",
		"BIGINT NOT NULL REFERENCES images(id),",
		"BIGINT NOT NULL REFERENCES pve_nodes(id),",
		"CREATE INDEX image_operations_image_id_created_idx",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("migration 0009 缺少语句: %q", want)
		}
	}
}
