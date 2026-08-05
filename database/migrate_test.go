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

// TestMigrationFilesOrder ensures migrations are discovered under the
// migration/ directory and run in ascending numeric filename order, while
// non-SQL files and subdirectories are ignored.
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

// TestMigrationFilesDirFS exercises the same discovery against a real
// directory tree (t.TempDir + os.DirFS) instead of an in-memory FS.
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

// TestEmbeddedMigrationsDiscovered is a regression test for the go:embed
// layout: the embedded FS root is database/, so migrations must be read from
// the migration/ subdirectory (not the FS root), exactly as Migrate does.
func TestEmbeddedMigrationsDiscovered(t *testing.T) {
	names, err := migrationFiles(MigrationFS)
	if err != nil {
		t.Fatalf("migrationFiles(MigrationFS): %v", err)
	}
	want := []string{"0001_init.sql", "0002_create_tables.sql", "0003_indexes_and_unique.sql", "0004_add_provision_error.sql"}
	if !slices.Equal(names, want) {
		t.Fatalf("embedded migrations = %v, want %v", names, want)
	}
	for _, name := range names {
		if _, err := fs.ReadFile(MigrationFS, "migration/"+name); err != nil {
			t.Fatalf("read migration/%s: %v", name, err)
		}
	}
}

// TestPendingMigrationsIdempotent simulates a full migrate cycle without a
// database: the first pass reports every migration as pending, and once they
// are marked as applied a second pass reports nothing (no-op re-run).
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
