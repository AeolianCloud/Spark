//go:build pg

// 针对真实 PostgreSQL 实例的仓库层分页契约集成测试（评审批次 B，
// 次要修复）：LIMIT/OFFSET 分页切片以及支撑 X-Total-Count 响应头的
// Count。排除在默认构建之外；通过 -tags=pg 运行：
//
//	SPARK_TEST_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' \
//	  go test -tags=pg ./repository/ -count=1 -run TestPGStorageTypeListPage
//
// 测试套件从 SPARK_TEST_DSN 连接数据库（共享助手 pgTestDSN，默认值与
// IP 并发测试相同），应用 migration，然后插入五个存储类型并断言两页
// 结果与总数。数据卫生：storage_types 在测试前后都执行 TRUNCATE
// （CASCADE，因此引用存储类型的遗留 vms 行不会阻塞清理）。
package repository

import (
	"context"
	"fmt"
	"testing"

	"spark/database"
	"spark/model"
)

// TestPGStorageTypeListPage 在真实 PostgreSQL 上验证
// StorageTypeRepository.ListPage/Count：插入五行后（id 随创建顺序
// 递增），一个 limit/offset 组合必须精确切出按 id 排序表的对应窗口，
// 而 Count 必须报告完整的五行，与分页无关。越过末尾的页返回空且
// 非 nil 的切片。
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

	// 干净起点（失败时也通过 defer 清理）：CASCADE 会跟随 vms 外键，
	// 因此遗留引用不会阻塞 TRUNCATE。
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
	assertPage(t, 2, 10, nil) // offset 越过末尾 -> 空页

	total, err := repo.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 5 {
		t.Fatalf("Count = %d, want 5 (independent of the page)", total)
	}

	// 存储类型行必须像真实实体，而不是占位符。
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
