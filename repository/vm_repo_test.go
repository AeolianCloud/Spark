package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// updateSpecSQL is the exact optimistic-lock statement UpdateSpec runs.
const updateSpecSQL = "UPDATE vms SET cpu=$1, mem_mb=$2, disk_gb=$3, updated_at=now() WHERE id=$4 AND cpu=$5 AND mem_mb=$6 AND disk_gb=$7"

func TestUpdateSpecOptimisticLockSuccess(t *testing.T) {
	mock := newMockPool(t)
	// The WHERE clause carries the old values read by the caller, so a
	// concurrent resize cannot silently overwrite it (asserted by the exact
	// SQL match below).
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

// TestUpdateSpecConcurrentModificationConflict pins down the 0-rows case: a
// spec change committed in between (or the row was deleted) -> ErrSpecConflict.
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
