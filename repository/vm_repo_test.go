package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"spark/model"
)

// updateSpecSQL 是 UpdateSpec 运行的确切的乐观锁语句。
const updateSpecSQL = "UPDATE vms SET cpu=$1, mem_mb=$2, disk_gb=$3, updated_at=now() WHERE id=$4 AND cpu=$5 AND mem_mb=$6 AND disk_gb=$7"

// importVMSQL 是 ImportVMTx 运行的确切的 INSERT ... RETURNING 语句；
// 可空列（ip_id 与 password_encrypted）显式写入 NULL，source 显式写入
// 调用方传入的值（认领语义下为 'claimed'），user_id 写入调用方的可选归属
// （nil 编码为 NULL=无主）。
const importVMSQL = "INSERT INTO vms (uuid, name, zone_id, node_id, pve_vmid, image_id, storage_type_id, cpu, mem_mb, disk_gb, ip_id, password_encrypted, source, user_id) " +
	"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULL, NULL, $11, $12) RETURNING " + vmCols

func TestUpdateSpecOptimisticLockSuccess(t *testing.T) {
	mock := newMockPool(t)
	// WHERE 子句携带调用方读到的旧值，因此并发扩容无法静默覆盖
	// （由下面的精确 SQL 匹配断言）。
	mock.ExpectExec(updateSpecSQL).
		WithArgs(4, int64(4096), int64(20), int64(1), 2, int64(2048), int64(10)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	repo := NewVMRepository(mock)
	if err := repo.UpdateSpec(context.Background(), 1, 4, 4096, 20, 2, 2048, 10); err != nil {
		t.Fatalf("UpdateSpec: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUpdateSpecConcurrentModificationConflict 钉死 0 行命中的情况：
// 期间有规格变更提交（或行被删除）-> ErrSpecConflict。
func TestUpdateSpecConcurrentModificationConflict(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectExec(updateSpecSQL).
		WithArgs(4, int64(4096), int64(20), int64(1), 2, int64(2048), int64(10)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	repo := NewVMRepository(mock)
	err := repo.UpdateSpec(context.Background(), 1, 4, 4096, 20, 2, 2048, 10)
	if !errors.Is(err, ErrSpecConflict) {
		t.Fatalf("err = %v, want ErrSpecConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// vmRowColumns 是 vmCols 扫描顺序对应的列名（18 列，含 source 与 user_id），
// 供 mock 行构造使用。
var vmRowColumns = []string{"id", "uuid", "name", "zone_id", "node_id", "pve_vmid", "image_id", "storage_type_id", "cpu", "mem_mb", "disk_gb", "ip_id", "password_encrypted", "provision_error", "source", "user_id", "created_at", "updated_at"}

// TestGetVMByNodeVMID 验证导入幂等检查查询：按 (node_id, pve_vmid) 精确
// 匹配；导入的 VM 行 image_id/storage_type_id/ip_id 为 NULL，密码与
// 预配置错误经 COALESCE 读作空字符串。
func TestGetVMByNodeVMID(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT "+vmCols+" FROM vms WHERE node_id=$1 AND pve_vmid=$2").
		WithArgs(int64(3), int64(101)).
		WillReturnRows(pgxmock.NewRows(vmRowColumns).
			AddRow(int64(9), "uuid-imp", "imported", int64(1), int64(3), int64(101), nil, nil, 2, int64(4096), int64(20), nil, "", "", "claimed", nil, testTime, testTime))

	repo := NewVMRepository(mock)
	vm, err := repo.GetVMByNodeVMID(context.Background(), 3, 101)
	if err != nil {
		t.Fatalf("GetVMByNodeVMID: %v", err)
	}
	if vm.ID != 9 || vm.NodeID != 3 || vm.PVEVmid != 101 {
		t.Fatalf("vm = %+v, want id 9, node 3, pve_vmid 101", vm)
	}
	if vm.ImageID != nil || vm.StorageTypeID != nil || vm.IPID != nil {
		t.Fatalf("expected nil image/storage/ip fields, got %+v", vm)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetVMByNodeVMIDNoRows 钉死未纳管的 (node_id, pve_vmid) 返回
// pgx.ErrNoRows，供服务层区分"首次导入"与"重复导入"。
func TestGetVMByNodeVMIDNoRows(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT "+vmCols+" FROM vms WHERE node_id=$1 AND pve_vmid=$2").
		WithArgs(int64(3), int64(999)).WillReturnError(pgx.ErrNoRows)

	repo := NewVMRepository(mock)
	_, err := repo.GetVMByNodeVMID(context.Background(), 3, 999)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestImportVMTx 验证导入插入：非零 pve_vmid，image_id/storage_type_id
// 传 nil 指针（编码为 NULL），ip_id 与 password_encrypted 显式 NULL。
func TestImportVMTx(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	// 期望参数里的两个 nil 必须带 *int64 类型（与 vm.ImageID 一致），
	// 否则与实际的类型化 nil 指针不相等。
	var nilImageID *int64
	var nilUserID *int64
	mock.ExpectQuery(importVMSQL).
		WithArgs("uuid-imp", "imported", int64(1), int64(3), int64(101), nilImageID, nilImageID, 2, int64(4096), int64(20), "claimed", nilUserID).
		WillReturnRows(pgxmock.NewRows(vmRowColumns).
			AddRow(int64(9), "uuid-imp", "imported", int64(1), int64(3), int64(101), nil, nil, 2, int64(4096), int64(20), nil, "", "", "claimed", nil, testTime, testTime))
	mock.ExpectCommit()

	repo := NewVMRepository(mock)
	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	vm, err := repo.ImportVMTx(context.Background(), tx, model.VM{
		UUID: "uuid-imp", Name: "imported", ZoneID: 1, NodeID: 3, PVEVmid: 101,
		CPU: 2, MemMB: 4096, DiskGB: 20, Source: model.VMSourceClaimed,
	})
	if err != nil {
		t.Fatalf("ImportVMTx: %v", err)
	}
	if vm.ID != 9 || vm.PVEVmid != 101 || vm.Name != "imported" {
		t.Fatalf("vm = %+v, want id 9, pve_vmid 101", vm)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestImportVMTxDuplicateConflict 钉死并发重复导入：vms_node_vmid_key
// 部分唯一索引冲突（SQLSTATE 23505）映射为 ErrConflict，服务层据此
// 返回 409 与幂等检查路径一致。
func TestImportVMTxDuplicateConflict(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	var nilImageID *int64
	var nilUserID *int64
	mock.ExpectQuery(importVMSQL).
		WithArgs("uuid-imp", "imported", int64(1), int64(3), int64(101), nilImageID, nilImageID, 2, int64(4096), int64(20), "claimed", nilUserID).
		WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint \"vms_node_vmid_key\""})

	repo := NewVMRepository(mock)
	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = repo.ImportVMTx(context.Background(), tx, model.VM{
		UUID: "uuid-imp", Name: "imported", ZoneID: 1, NodeID: 3, PVEVmid: 101,
		CPU: 2, MemMB: 4096, DiskGB: 20, Source: model.VMSourceClaimed,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
