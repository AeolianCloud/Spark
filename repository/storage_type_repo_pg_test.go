//go:build pg

// 针对真实 PostgreSQL 实例的存储类型仓库契约集成测试（提案
// auto-scan-pve-storage 任务 3.2）：扫描同步的 upsert 语义（新建/更新、
// 只覆盖 type/content、保留 name/enabled）、UpdateMeta（仅 name/enabled）、
// ListPage/Count 的 zone 过滤、Get/Delete 既有语义。排除在默认构建之外；
// 通过 -tags=pg 运行：
//
//	SPARK_TEST_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' \
//	  go test -tags=pg ./repository/ -count=1 -run TestPGStorageType
//
// 测试套件从 SPARK_TEST_DSN 连接数据库（共享助手 pgTestDSN，默认值与
// IP 并发测试相同）。注意：migration 0011 要求 zones 非空（单 zone 自动
// 归置路径），测试库 zones 为空时先插入一个测试 zone 再应用迁移。
// 数据卫生：storage_types 在测试前后都执行 TRUNCATE（CASCADE，因此引用
// 存储类型的遗留 vms 行不会阻塞清理）。
package repository

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark/database"
)

// pgTestEnsureZone 保证测试库至少存在一个 zone：migration 0011 的存量
// 归置逻辑要求 zones 非空（单 zone 时全量归入），全新测试库从零应用
// 迁移前必须先有 zone 行。
func pgTestEnsureZone(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var zoneCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM zones").Scan(&zoneCount); err != nil {
		t.Fatalf("count zones: %v", err)
	}
	if zoneCount > 0 {
		var id int64
		if err := pool.QueryRow(ctx, "SELECT id FROM zones ORDER BY id LIMIT 1").Scan(&id); err != nil {
			t.Fatalf("pick zone: %v", err)
		}
		return id
	}
	var id int64
	if err := pool.QueryRow(ctx, "INSERT INTO zones (name) VALUES ('pg-storage-test') RETURNING id").Scan(&id); err != nil {
		t.Fatalf("insert test zone: %v", err)
	}
	return id
}

// pgTestInsertZone 插入一个命名测试 zone 并返回其 id。
func pgTestInsertZone(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, "INSERT INTO zones (name) VALUES ($1) RETURNING id", name).Scan(&id); err != nil {
		t.Fatalf("insert zone %s: %v", name, err)
	}
	return id
}

func pgBoolPtr(b bool) *bool { return &b }

// TestPGStorageTypeUpsertAndMeta 验证扫描同步的核心语义：同 zone 同名
// PVE 存储 upsert 命中同一行（不重复），更新仅覆盖 type/content 且保留
// 管理员设置的 name/enabled；UpdateMeta 只改非 nil 字段（name 置空写
// NULL、enabled 切换）。
func TestPGStorageTypeUpsertAndMeta(t *testing.T) {
	ctx := context.Background()

	pool, err := database.New(ctx, pgTestDSN())
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer pool.Close()

	zoneID := pgTestEnsureZone(t, ctx, pool)
	if err := database.Migrate(ctx, pool, database.MigrationFS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	clean := func() {
		if _, err := pool.Exec(ctx, "TRUNCATE storage_types CASCADE"); err != nil {
			t.Fatalf("truncate storage_types: %v", err)
		}
	}
	clean()
	defer clean()

	repo := NewStorageTypeRepository(pool)

	// 新建：name 为 NULL、enabled 默认 true、type/content/nodes 快照落库。
	st, created, err := repo.UpsertByZonePveStorage(ctx, zoneID, "local", "dir", "images,iso", []string{"pve1", "pve2"})
	if err != nil {
		t.Fatalf("upsert new: %v", err)
	}
	if !created {
		t.Fatal("upsert new: inserted = false, want true")
	}
	if st.Name != nil || !st.Enabled || st.Type == nil || *st.Type != "dir" ||
		st.Content == nil || *st.Content != "images,iso" {
		t.Fatalf("upsert new row = %+v", st)
	}
	if len(st.Nodes) != 2 || st.Nodes[0] != "pve1" || st.Nodes[1] != "pve2" {
		t.Fatalf("upsert new nodes = %v, want [pve1 pve2]", st.Nodes)
	}

	// 管理员设置业务名并禁用，随后再次 upsert：type/content/nodes 更新，
	// name/enabled 必须保留。
	name := "业务名"
	updated, err := repo.UpdateMeta(ctx, st.ID, &name, pgBoolPtr(false))
	if err != nil {
		t.Fatalf("update meta: %v", err)
	}
	if updated.Name == nil || *updated.Name != "业务名" || updated.Enabled {
		t.Fatalf("updated row = %+v", updated)
	}

	st2, created2, err := repo.UpsertByZonePveStorage(ctx, zoneID, "local", "lvm", "images", []string{"pve1"})
	if err != nil {
		t.Fatalf("upsert existing: %v", err)
	}
	if created2 {
		t.Fatal("upsert existing: inserted = true, want false (same zone+pve_storage)")
	}
	if st2.ID != st.ID {
		t.Fatalf("upsert existing id = %d, want %d (no duplicate row)", st2.ID, st.ID)
	}
	if st2.Name == nil || *st2.Name != "业务名" || st2.Enabled {
		t.Fatalf("upsert must preserve name/enabled: %+v", st2)
	}
	if st2.Type == nil || *st2.Type != "lvm" || st2.Content == nil || *st2.Content != "images" {
		t.Fatalf("upsert must refresh type/content: %+v", st2)
	}
	if len(st2.Nodes) != 1 || st2.Nodes[0] != "pve1" {
		t.Fatalf("upsert must refresh nodes snapshot: %v, want [pve1]", st2.Nodes)
	}

	// 不同 zone 的同名 PVE 存储互不冲突（zone 隔离）。
	otherZoneID := pgTestInsertZone(t, ctx, pool, "pg-storage-zone-2")
	stOther, created3, err := repo.UpsertByZonePveStorage(ctx, otherZoneID, "local", "dir", "iso", nil)
	if err != nil {
		t.Fatalf("upsert other zone: %v", err)
	}
	if !created3 || stOther.ID == st.ID {
		t.Fatalf("upsert other zone: created=%v id=%d, want a distinct new row", created3, stOther.ID)
	}
	// 空切片（nil = 不限制节点）落库为空串，读回空切片（非 nil）。
	if stOther.Nodes == nil || len(stOther.Nodes) != 0 {
		t.Fatalf("upsert empty nodes = %v, want non-nil empty slice (unlimited)", stOther.Nodes)
	}

	// UpdateMeta 只改 enabled（name 保持）。
	enabled := true
	meta2, err := repo.UpdateMeta(ctx, st.ID, nil, &enabled)
	if err != nil {
		t.Fatalf("update meta enabled only: %v", err)
	}
	if !meta2.Enabled || meta2.Name == nil || *meta2.Name != "业务名" {
		t.Fatalf("update meta enabled only = %+v", meta2)
	}

	// name 置空：空串 -> NULL。
	empty := ""
	meta3, err := repo.UpdateMeta(ctx, st.ID, &empty, nil)
	if err != nil {
		t.Fatalf("update meta clear name: %v", err)
	}
	if meta3.Name != nil {
		t.Fatalf("update meta clear name: name = %v, want NULL", meta3.Name)
	}
	if !meta3.Enabled {
		t.Fatalf("update meta clear name: enabled must stay true: %+v", meta3)
	}

	// 不存在的 id -> pgx.ErrNoRows。
	if _, err := repo.UpdateMeta(ctx, 999999, nil, pgBoolPtr(true)); err == nil {
		t.Fatal("update meta missing id: want error")
	}
}

// TestPGStorageTypeZoneFilter 验证 ListPage/Count 的 zone 过滤与分页契约：
// 两 zone 各插入两行，全量与单 zone 查询返回正确的行集与总数。
func TestPGStorageTypeZoneFilter(t *testing.T) {
	ctx := context.Background()

	pool, err := database.New(ctx, pgTestDSN())
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer pool.Close()

	zoneID := pgTestEnsureZone(t, ctx, pool)
	if err := database.Migrate(ctx, pool, database.MigrationFS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	clean := func() {
		if _, err := pool.Exec(ctx, "TRUNCATE storage_types CASCADE"); err != nil {
			t.Fatalf("truncate storage_types: %v", err)
		}
	}
	clean()
	defer clean()

	repo := NewStorageTypeRepository(pool)

	otherZoneID := pgTestInsertZone(t, ctx, pool, "pg-storage-zone-filter")

	for i := 1; i <= 2; i++ {
		for _, z := range []int64{zoneID, otherZoneID} {
			if _, _, err := repo.UpsertByZonePveStorage(ctx, z, fmt.Sprintf("pve-%d", i), "dir", "images", nil); err != nil {
				t.Fatalf("upsert zone %d storage %d: %v", z, i, err)
			}
		}
	}

	// 全量 Count 与分页。
	total, err := repo.Count(ctx, nil)
	if err != nil || total != 4 {
		t.Fatalf("Count(nil) = %d, %v; want 4", total, err)
	}
	all, err := repo.ListPage(ctx, nil, 2, 1)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListPage(nil, 2, 1) = %d rows, %v; want 2", len(all), err)
	}

	// 单 zone 过滤。
	for _, z := range []int64{zoneID, otherZoneID} {
		total, err := repo.Count(ctx, &z)
		if err != nil || total != 2 {
			t.Fatalf("Count(zone %d) = %d, %v; want 2", z, total, err)
		}
		page, err := repo.ListPage(ctx, &z, 10, 0)
		if err != nil || len(page) != 2 {
			t.Fatalf("ListPage(zone %d) = %d rows, %v; want 2", z, len(page), err)
		}
		for _, st := range page {
			if st.ZoneID != z {
				t.Fatalf("ListPage(zone %d) leaked row of zone %d", z, st.ZoneID)
			}
		}
	}

	// 全量列举（limit <= 0）返回全部行，扫描删除对齐用。
	all2, err := repo.ListPage(ctx, &zoneID, -1, 0)
	if err != nil || len(all2) != 2 {
		t.Fatalf("ListPage(zone %d, -1, 0) = %d rows, %v; want all 2", zoneID, len(all2), err)
	}
}

// TestPGStorageTypeDeleteSemantics 验证 Delete 的既有语义在新列下保持：
// 正常删除、未知 id -> ErrNoRows、被 vms 引用 -> ErrInUse（扫描删除的
// skipped 兜底依赖该哨兵错误）。
func TestPGStorageTypeDeleteSemantics(t *testing.T) {
	ctx := context.Background()

	pool, err := database.New(ctx, pgTestDSN())
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer pool.Close()

	zoneID := pgTestEnsureZone(t, ctx, pool)
	if err := database.Migrate(ctx, pool, database.MigrationFS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	clean := func() {
		if _, err := pool.Exec(ctx, "TRUNCATE storage_types CASCADE"); err != nil {
			t.Fatalf("truncate storage_types: %v", err)
		}
	}
	clean()
	defer clean()

	repo := NewStorageTypeRepository(pool)

	st, _, err := repo.UpsertByZonePveStorage(ctx, zoneID, "local", "dir", "images", []string{"pve1"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Get 新列齐备。
	got, err := repo.Get(ctx, st.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ZoneID != zoneID || got.PVEStorage != "local" || !got.Enabled {
		t.Fatalf("Get = %+v", got)
	}
	if len(got.Nodes) != 1 || got.Nodes[0] != "pve1" {
		t.Fatalf("Get nodes = %v, want [pve1]", got.Nodes)
	}

	// 被 VM 引用 -> ErrInUse（vms.node_id 引用 pve_nodes，先插一个节点）。
	var nodeID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO pve_nodes (zone_id, name, host, api_user, api_token_secret) "+
			"VALUES ($1, 'pg-del-node', 'h', 'root@pam', 's') RETURNING id",
		zoneID).Scan(&nodeID); err != nil {
		t.Fatalf("insert referencing node: %v", err)
	}
	var vmID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO vms (uuid, name, zone_id, node_id, pve_vmid, storage_type_id, cpu, mem_mb, disk_gb) "+
			"VALUES ('pg-del-ref', 'pg-del-ref', $1, $2, 1, $3, 1, 512, 1) RETURNING id",
		zoneID, nodeID, st.ID).Scan(&vmID); err != nil {
		t.Fatalf("insert referencing vm: %v", err)
	}
	if err := repo.Delete(ctx, st.ID); err == nil || err != ErrInUse {
		t.Fatalf("Delete referenced = %v, want ErrInUse", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM vms WHERE id=$1", vmID); err != nil {
		t.Fatalf("cleanup vm: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM pve_nodes WHERE id=$1", nodeID); err != nil {
		t.Fatalf("cleanup node: %v", err)
	}

	// 无引用 -> 删除成功；再删 -> ErrNoRows。
	if err := repo.Delete(ctx, st.ID); err != nil {
		t.Fatalf("Delete = %v, want nil", err)
	}
	if err := repo.Delete(ctx, st.ID); err == nil || err != pgx.ErrNoRows {
		t.Fatalf("Delete missing = %v, want pgx.ErrNoRows", err)
	}
}
