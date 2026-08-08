package repository

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"spark/model"
)

// opRowColumns 是 opCols 扫描顺序对应的列名（含 COALESCE 归一化的
// error_message 与可空的 user_id/operator_type/operator_id），供 mock 行
// 构造使用。
var opRowColumns = []string{"id", "node_id", "pve_vmid", "action", "result", "error_message", "user_id", "operator_type", "operator_id", "created_at"}

// createOperationSQL 是 CreateOperation 运行的确切的 INSERT ... RETURNING
// 语句（含 COALESCE 列序，reviewer-二.2；operator_type/operator_id 为
// 操作者列，设计 D8）。
const createOperationSQL = "INSERT INTO vm_operations (node_id, pve_vmid, action, result, error_message, user_id, operator_type, operator_id) " +
	"VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING " + opCols

// listOperationsCountSQL 是 ListOperations 运行的确切总数统计语句。
const listOperationsCountSQL = "SELECT count(*) FROM vm_operations WHERE node_id=$1 AND pve_vmid=$2"

// listOperationsSQL 是 ListOperations 运行的确切的倒序分页查询语句。
const listOperationsSQL = "SELECT " + opCols + " FROM vm_operations WHERE node_id=$1 AND pve_vmid=$2 " +
	"ORDER BY created_at DESC, id DESC LIMIT $3 OFFSET $4"

// TestCreateOperation 验证审计记录插入：返回带 id/created_at 的行，
// COALESCE 列序正确（error_message 读作 string，user_id/operator_id 读作
// 可空指针，operator_type 读作 string）。
func TestCreateOperation(t *testing.T) {
	mock := newMockPool(t)
	// 期望参数里的 user_id/operator_id nil 必须带 *int64 类型（与
	// op.UserID/op.OperatorID 一致），否则与实际的类型化 nil 指针不相等。
	var nilUserID *int64
	var nilOperatorID *int64
	mock.ExpectQuery(createOperationSQL).
		WithArgs(int64(3), int64(101), "start", "failed", "vm 101 not found on node \"pve1\"", nilUserID, "admin", nilOperatorID).
		WillReturnRows(pgxmock.NewRows(opRowColumns).
			AddRow(int64(9), int64(3), int64(101), "start", "failed", "vm 101 not found on node \"pve1\"", nil, "admin", nil, testTime))

	repo := NewVMOperationRepository(mock)
	op, err := repo.CreateOperation(context.Background(), model.VMOperation{
		NodeID: 3, PVEVmid: 101, Action: model.VMOpActionStart, Result: model.VMOpResultFailed,
		ErrorMessage: "vm 101 not found on node \"pve1\"", OperatorType: "admin",
	})
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	if op.ID != 9 || op.NodeID != 3 || op.PVEVmid != 101 || op.Action != "start" ||
		op.Result != "failed" || op.ErrorMessage != "vm 101 not found on node \"pve1\"" {
		t.Fatalf("op = %+v, want id 9 / node 3 / vmid 101 / start failed", op)
	}
	if op.UserID != nil {
		t.Fatalf("user_id = %v, want nil (reserved column)", op.UserID)
	}
	if op.OperatorType != "admin" || op.OperatorID != nil {
		t.Fatalf("operator = %q / %v, want admin / nil", op.OperatorType, op.OperatorID)
	}
	if !op.CreatedAt.Equal(testTime) {
		t.Fatalf("created_at = %v, want %v", op.CreatedAt, testTime)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestListOperations 验证按 (node_id, pve_vmid) 过滤、created_at DESC,
// id DESC 倒序分页查询，以及 limit 之前的总数统计。
func TestListOperations(t *testing.T) {
	mock := newMockPool(t)
	// 非 nil 的 operator_id 行值必须以 *int64 形式提供（pgxmock 按 Kind
	// 匹配：**int64 目标需要 *int64 值，否则报 destination kind 'ptr'）。
	operatorID := int64(2)
	operatorIDPtr := &operatorID
	mock.ExpectQuery(listOperationsCountSQL).
		WithArgs(int64(3), int64(101)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(listOperationsSQL).
		WithArgs(int64(3), int64(101), 10, 20).
		WillReturnRows(pgxmock.NewRows(opRowColumns).
			AddRow(int64(9), int64(3), int64(101), "destroy", "accepted", "", nil, "user", operatorIDPtr, testTime).
			AddRow(int64(8), int64(3), int64(101), "start", "accepted", "", nil, nil, nil, testTime.Add(1)))

	repo := NewVMOperationRepository(mock)
	ops, total, err := repo.ListOperations(context.Background(), 3, 101, 10, 20)
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	if total != 2 || len(ops) != 2 {
		t.Fatalf("total = %d, ops = %d, want 2/2", total, len(ops))
	}
	if ops[0].ID != 9 || ops[0].Action != "destroy" || ops[1].ID != 8 || ops[1].Action != "start" {
		t.Fatalf("ops = %+v, want destroy then start (descending created_at)", ops)
	}
	for _, op := range ops {
		if op.NodeID != 3 || op.PVEVmid != 101 {
			t.Fatalf("op = %+v, want node 3 / vmid 101", op)
		}
	}
	// 操作者字段按行透传：最新记录为 user/2（操作者），旧记录为 nil（无
	// 操作者信息）。
	if ops[0].OperatorType != "user" || ops[0].OperatorID == nil || *ops[0].OperatorID != 2 {
		t.Fatalf("op[0] operator = %q / %v, want user / 2", ops[0].OperatorType, ops[0].OperatorID)
	}
	if ops[1].OperatorType != "" || ops[1].OperatorID != nil {
		t.Fatalf("op[1] operator = %q / %v, want empty / nil (legacy record)", ops[1].OperatorType, ops[1].OperatorID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
