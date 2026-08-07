package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"spark/model"
)

// imageOpRowColumns 是 imgOpCols 扫描顺序对应的列名（含 COALESCE 归一化的
// error_message / upid），供 mock 行构造使用。
var imageOpRowColumns = []string{"id", "image_id", "node_id", "action", "result", "error_message", "upid", "user_id", "created_at", "updated_at"}

// createImageOperationSQL 是 CreateOperation 运行的确切的 INSERT ...
// RETURNING 语句（含 COALESCE 列序）。
const createImageOperationSQL = "INSERT INTO image_operations (image_id, node_id, action, result, error_message, upid, user_id) " +
	"VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING " + imgOpCols

// listImageOperationsCountSQL 是 ListOperationsByImage 运行的确切总数
// 统计语句。
const listImageOperationsCountSQL = "SELECT count(*) FROM image_operations WHERE image_id=$1"

// listImageOperationsSQL 是 ListOperationsByImage 运行的确切的倒序分页
// 查询语句（created_at 倒序，同刻按 id 倒序，保证翻页稳定）。
const listImageOperationsSQL = "SELECT " + imgOpCols + " FROM image_operations WHERE image_id=$1 " +
	"ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3"

// updateImageOperationResultSQL 是 UpdateOperationResult 运行的确切的
// UPDATE 语句（result / error_message / upid 一并回写）。
const updateImageOperationResultSQL = "UPDATE image_operations SET result=$2, error_message=$3, upid=$4, updated_at=now() WHERE id=$1"

// hasRunningImageOperationSQL 是 HasRunningOperation 运行的确切的
// EXISTS 查询语句（幂等检查：镜像在某节点上是否有未终态下载）。
const hasRunningImageOperationSQL = "SELECT EXISTS(SELECT 1 FROM image_operations WHERE image_id=$1 AND node_id=$2 AND result='running')"

// TestCreateImageOperation 验证下载操作记录插入：result 由调用方传入
// （'running'），返回带 id/created_at/updated_at 的行；error_message /
// upid 经 COALESCE 读作字符串，user_id 读作可空指针（预留列恒 NULL）。
func TestCreateImageOperation(t *testing.T) {
	mock := newMockPool(t)
	// 期望参数里的 user_id nil 必须带 *int64 类型（与 op.UserID 一致），
	// 否则与实际的类型化 nil 指针不相等。
	var nilUserID *int64
	mock.ExpectQuery(createImageOperationSQL).
		WithArgs(int64(7), int64(3), model.ImageOpActionDownload, model.ImageOpResultRunning, "", "", nilUserID).
		WillReturnRows(pgxmock.NewRows(imageOpRowColumns).
			AddRow(int64(21), int64(7), int64(3), "download", "running", "", "", nil, testTime, testTime))

	repo := NewImageOperationRepository(mock)
	op, err := repo.CreateOperation(context.Background(), model.ImageOperation{
		ImageID: 7, NodeID: 3, Action: model.ImageOpActionDownload, Result: model.ImageOpResultRunning,
	})
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	if op.ID != 21 || op.ImageID != 7 || op.NodeID != 3 || op.Action != "download" || op.Result != "running" {
		t.Fatalf("op = %+v, want id 21 / image 7 / node 3 / download running", op)
	}
	if op.UserID != nil {
		t.Fatalf("user_id = %v, want nil (reserved column)", op.UserID)
	}
	if !op.CreatedAt.Equal(testTime) || !op.UpdatedAt.Equal(testTime) {
		t.Fatalf("created_at/updated_at = %v/%v, want %v", op.CreatedAt, op.UpdatedAt, testTime)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUpdateImageOperationResult 验证结果更新：result、error_message 与
// upid 一并落库（upid 为受理后回填的任务标识），updated_at 由数据库
// now() 刷新。
func TestUpdateImageOperationResult(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectExec(updateImageOperationResultSQL).
		WithArgs(int64(21), model.ImageOpResultFailed, "download timed out", "UPID:pve1:0001:0002").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	repo := NewImageOperationRepository(mock)
	if err := repo.UpdateOperationResult(context.Background(), 21, model.ImageOpResultFailed, "download timed out", "UPID:pve1:0001:0002"); err != nil {
		t.Fatalf("UpdateOperationResult: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUpdateImageOperationResultError 验证更新失败时原样返回数据库错误
// （非哨兵错误，直接透传）。
func TestUpdateImageOperationResultError(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectExec(updateImageOperationResultSQL).
		WithArgs(int64(21), model.ImageOpResultFailed, "download timed out", "UPID:pve1:0001:0002").
		WillReturnError(errors.New("connection refused"))

	repo := NewImageOperationRepository(mock)
	if err := repo.UpdateOperationResult(context.Background(), 21, model.ImageOpResultFailed, "download timed out", "UPID:pve1:0001:0002"); err == nil || err.Error() != "connection refused" {
		t.Fatalf("UpdateOperationResult err = %v, want connection refused", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestCreateImageOperationForeignKeyConflict 验证镜像/节点被删时插入操作
// 记录触发外键冲突（SQLSTATE 23503），映射为 ErrInUse。
func TestCreateImageOperationForeignKeyConflict(t *testing.T) {
	mock := newMockPool(t)
	var nilUserID *int64
	mock.ExpectQuery(createImageOperationSQL).
		WithArgs(int64(999), int64(3), model.ImageOpActionDownload, model.ImageOpResultRunning, "", "", nilUserID).
		WillReturnError(&pgconn.PgError{Code: "23503", Message: "insert or update on table \"image_operations\" violates foreign key constraint \"image_operations_image_id_fkey\""})

	repo := NewImageOperationRepository(mock)
	op, err := repo.CreateOperation(context.Background(), model.ImageOperation{
		ImageID: 999, NodeID: 3, Action: model.ImageOpActionDownload, Result: model.ImageOpResultRunning,
	})
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("CreateOperation err = %v, want ErrInUse", err)
	}
	if op != nil {
		t.Fatalf("op = %+v, want nil", op)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestHasRunningImageOperation 验证幂等检查查询：镜像在某节点上有
// running 记录时返回 true，没有时返回 false（SQL 参数与断言语句精确匹配）。
func TestHasRunningImageOperation(t *testing.T) {
	t.Run("returns true when a running operation exists", func(t *testing.T) {
		mock := newMockPool(t)
		mock.ExpectQuery(hasRunningImageOperationSQL).
			WithArgs(int64(7), int64(3)).
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))

		repo := NewImageOperationRepository(mock)
		exists, err := repo.HasRunningOperation(context.Background(), 7, 3)
		if err != nil {
			t.Fatalf("HasRunningOperation: %v", err)
		}
		if !exists {
			t.Fatalf("exists = false, want true")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns false when no running operation exists", func(t *testing.T) {
		mock := newMockPool(t)
		mock.ExpectQuery(hasRunningImageOperationSQL).
			WithArgs(int64(7), int64(3)).
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))

		repo := NewImageOperationRepository(mock)
		exists, err := repo.HasRunningOperation(context.Background(), 7, 3)
		if err != nil {
			t.Fatalf("HasRunningOperation: %v", err)
		}
		if exists {
			t.Fatalf("exists = true, want false")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

// TestHasRunningImageOperationError 验证查询失败时返回 (false, err)。
func TestHasRunningImageOperationError(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery(hasRunningImageOperationSQL).
		WithArgs(int64(7), int64(3)).
		WillReturnError(errors.New("connection refused"))

	repo := NewImageOperationRepository(mock)
	exists, err := repo.HasRunningOperation(context.Background(), 7, 3)
	if err == nil || err.Error() != "connection refused" {
		t.Fatalf("HasRunningOperation err = %v, want connection refused", err)
	}
	if exists {
		t.Fatalf("exists = true, want false on error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestListImageOperationsByImage 验证按 image_id 过滤、created_at 倒序
// 分页查询（新操作在前，created_at 相同的行按 id 倒序追加），以及 limit
// 之前的总数统计；error_message/upid 的 NULL 经 COALESCE 读作空字符串。
func TestListImageOperationsByImage(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery(listImageOperationsCountSQL).
		WithArgs(int64(7)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(listImageOperationsSQL).
		WithArgs(int64(7), 10, 20).
		WillReturnRows(pgxmock.NewRows(imageOpRowColumns).
			AddRow(int64(22), int64(7), int64(3), "download", "success", "", "UPID:pve1:0001:0002", nil, testTime, testTime).
			AddRow(int64(21), int64(7), int64(3), "download", "failed", "download timed out", "", nil, testTime, testTime))

	repo := NewImageOperationRepository(mock)
	ops, total, err := repo.ListOperationsByImage(context.Background(), 7, 10, 20)
	if err != nil {
		t.Fatalf("ListOperationsByImage: %v", err)
	}
	if total != 2 || len(ops) != 2 {
		t.Fatalf("total = %d, ops = %d, want 2/2", total, len(ops))
	}
	// created_at 相同的两行按 id 倒序：22 在前、21 在后。
	if ops[0].ID != 22 || ops[0].Result != "success" || ops[0].UPID != "UPID:pve1:0001:0002" {
		t.Fatalf("ops[0] = %+v, want id 22 success with upid", ops[0])
	}
	if ops[1].ID != 21 || ops[1].Result != "failed" || ops[1].ErrorMessage != "download timed out" {
		t.Fatalf("ops[1] = %+v, want id 21 failed with error", ops[1])
	}
	for _, op := range ops {
		if op.ImageID != 7 || op.NodeID != 3 {
			t.Fatalf("op = %+v, want image 7 / node 3", op)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestListImageOperationsByImageCountError 验证总数统计查询失败时返回
// (nil, 0, err)。
func TestListImageOperationsByImageCountError(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery(listImageOperationsCountSQL).
		WithArgs(int64(7)).
		WillReturnError(errors.New("connection refused"))

	repo := NewImageOperationRepository(mock)
	ops, total, err := repo.ListOperationsByImage(context.Background(), 7, 10, 20)
	if err == nil || err.Error() != "connection refused" {
		t.Fatalf("ListOperationsByImage err = %v, want connection refused", err)
	}
	if ops != nil || total != 0 {
		t.Fatalf("ops = %v, total = %d, want nil/0", ops, total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestListImageOperationsByImageRowsErr 验证行迭代结束时的错误（rows.Err）
// 原样返回。
func TestListImageOperationsByImageRowsErr(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery(listImageOperationsCountSQL).
		WithArgs(int64(7)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(listImageOperationsSQL).
		WithArgs(int64(7), 10, 0).
		WillReturnRows(pgxmock.NewRows(imageOpRowColumns).
			AddRow(int64(21), int64(7), int64(3), "download", "running", "", "", nil, testTime, testTime).
			CloseError(errors.New("scan aborted")))

	repo := NewImageOperationRepository(mock)
	ops, total, err := repo.ListOperationsByImage(context.Background(), 7, 10, 0)
	if err == nil || err.Error() != "scan aborted" {
		t.Fatalf("ListOperationsByImage err = %v, want scan aborted", err)
	}
	if ops != nil || total != 0 {
		t.Fatalf("ops = %v, total = %d, want nil/0", ops, total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
