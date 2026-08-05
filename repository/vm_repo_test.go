package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// updateSpecSQL 是 UpdateSpec 运行的确切的乐观锁语句。
const updateSpecSQL = "UPDATE vms SET cpu=$1, mem_mb=$2, disk_gb=$3, updated_at=now() WHERE id=$4 AND cpu=$5 AND mem_mb=$6 AND disk_gb=$7"

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
