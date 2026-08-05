//go:build pg

// Integration test for the repository-level pagination contract against a
// real PostgreSQL instance (review batch B, minor fix): the LIMIT/OFFSET
// page slicing and the Count backing the X-Total-Count header. Excluded
// from the default build; run with -tags=pg:
//
//	SPARK_TEST_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' \
//	  go test -tags=pg ./repository/ -count=1 -run TestPGStorageTypeListPage
//
// The suite connects to the DSN from SPARK_TEST_DSN (shared helper
// pgTestDSN, same default as the IP concurrency tests), applies the
// migrations, then inserts five storage types and asserts two pages plus
// the total. Data hygiene: storage_types is TRUNCATEd (CASCADE, so a
// leftover vms row referencing a storage type cannot block the cleanup)
// before and after the test.
package repository

import (
	"context"
	"fmt"
	"testing"

	"spark/database"
	"spark/model"
)

// TestPGStorageTypeListPage verifies StorageTypeRepository.ListPage/Count
// against real PostgreSQL: after inserting five rows (ids ascending with
// creation order) a limit/offset pair must slice exactly that window of the
// id-ordered table and Count must report the full five rows, independent of
// the page. A page past the end yields an empty, non-nil slice.
func TestPGStorageTypeListPage(t *testing.T) {
	ctx := context.Background()

	pool, err := database.New(ctx, pgTestDSN())
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool, database.MigrationFS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Clean slate (also on failure via defer): CASCADE follows the vms FK
	// so a leftover reference cannot block the truncate.
	clean := func() {
		if _, err := pool.Exec(ctx, "TRUNCATE storage_types CASCADE"); err != nil {
			t.Fatalf("truncate storage_types: %v", err)
		}
	}
	clean()
	defer clean()

	repo := NewStorageTypeRepository(pool)
	ids := make([]int64, 0, 5)
	for i := 1; i <= 5; i++ {
		st, err := repo.Create(ctx, fmt.Sprintf("pg-page-%d", i), fmt.Sprintf("Type %d", i), fmt.Sprintf("pve-%d", i))
		if err != nil {
			t.Fatalf("create storage type %d: %v", i, err)
		}
		// 序列不因 TRUNCATE 重置，只断言 id 随插入严格递增。
		if len(ids) > 0 && st.ID <= ids[len(ids)-1] {
			t.Fatalf("storage type %d: id %d not after previous %d", i, st.ID, ids[len(ids)-1])
		}
		ids = append(ids, st.ID)
	}

	assertPage := func(t *testing.T, limit, offset int, wantNames []string) {
		t.Helper()
		page, err := repo.ListPage(ctx, limit, offset)
		if err != nil {
			t.Fatalf("ListPage(%d, %d): %v", limit, offset, err)
		}
		if len(page) != len(wantNames) {
			t.Fatalf("ListPage(%d, %d): got %d rows, want %d", limit, offset, len(page), len(wantNames))
		}
		for i, st := range page {
			if st.Name != wantNames[i] {
				t.Fatalf("ListPage(%d, %d) row %d: name %q, want %q", limit, offset, i, st.Name, wantNames[i])
			}
		}
	}

	assertPage(t, 2, 0, []string{"pg-page-1", "pg-page-2"})
	assertPage(t, 2, 2, []string{"pg-page-3", "pg-page-4"})
	assertPage(t, 2, 4, []string{"pg-page-5"})
	assertPage(t, 10, 0, []string{"pg-page-1", "pg-page-2", "pg-page-3", "pg-page-4", "pg-page-5"})
	assertPage(t, 2, 10, nil) // offset past the end -> empty page

	total, err := repo.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 5 {
		t.Fatalf("Count = %d, want 5 (independent of the page)", total)
	}

	// The storage type rows must look like real entities, not placeholders.
	firstID := ids[0]
	st, err := repo.Get(ctx, firstID)
	if err != nil {
		t.Fatalf("Get(%d): %v", firstID, err)
	}
	want := model.StorageType{ID: firstID, Name: "pg-page-1", DisplayName: "Type 1", PVEStorage: "pve-1"}
	if st.ID != want.ID || st.Name != want.Name || st.DisplayName != want.DisplayName || st.PVEStorage != want.PVEStorage {
		t.Fatalf("Get(%d) = %+v, want %+v", firstID, st, want)
	}
}
