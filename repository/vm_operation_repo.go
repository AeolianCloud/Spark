package repository

import (
	"context"

	"spark/model"
)

// VMOperationRepository 负责持久化 vm_operations 行：虚拟机生命周期操作
// （start/stop/reboot/destroy）的审计记录（设计 D5）。操作记录不随 vms
// 行删除而删除（无外键 ON DELETE），仅按 (node_id, pve_vmid) 关联。
type VMOperationRepository struct {
	pool pgxQuerier
}

// NewVMOperationRepository 创建由 pool 支撑的 VMOperationRepository。
func NewVMOperationRepository(pool pgxQuerier) *VMOperationRepository {
	return &VMOperationRepository{pool: pool}
}

// opCols 是 vm_operations 的读取列清单。error_message 有默认值，
// user_id/operator_type/operator_id 可为 NULL（预留与旧记录），都经
// COALESCE/可空指针归一化（operator_type/operator_id 同时为 NULL 表示
// 用户体系落地前的旧记录，无操作者信息）。
const opCols = "id, node_id, pve_vmid, action, result, COALESCE(error_message, '') AS error_message, user_id, operator_type, operator_id, created_at"

// CreateOperation 插入一条操作记录并返回带 id/created_at 的行。user_id
// 是 0008 迁移预留的可空列（保留不写，语义以 operator_type/operator_id
// 为准，设计 D8）；operator_type/operator_id 记录实际操作者（设计 D5），
// 由调用方写入，NULL 表示旧记录。
func (r *VMOperationRepository) CreateOperation(ctx context.Context, op model.VMOperation) (*model.VMOperation, error) {
	var created model.VMOperation
	err := r.pool.QueryRow(ctx,
		"INSERT INTO vm_operations (node_id, pve_vmid, action, result, error_message, user_id, operator_type, operator_id) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING "+opCols,
		op.NodeID, op.PVEVmid, op.Action, op.Result, op.ErrorMessage, op.UserID,
		op.OperatorType, op.OperatorID,
	).Scan(&created.ID, &created.NodeID, &created.PVEVmid, &created.Action, &created.Result,
		&created.ErrorMessage, &created.UserID, &created.OperatorType, &created.OperatorID,
		&created.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &created, nil
}

// ListOperations 按时间倒序返回指定 (node_id, pve_vmid) 的一页操作记录，
// 以及匹配该 VM 的记录总数（limit/offset 之前）。created_at 相同的行按
// id 倒序追加，保证翻页稳定。
func (r *VMOperationRepository) ListOperations(ctx context.Context, nodeID, vmid int64, limit, offset int) ([]model.VMOperation, int, error) {
	var total int
	if err := r.pool.QueryRow(ctx,
		"SELECT count(*) FROM vm_operations WHERE node_id=$1 AND pve_vmid=$2", nodeID, vmid,
	).Scan(&total); err != nil {
		return nil, 0, classifyDBError(err)
	}
	rows, err := r.pool.Query(ctx,
		"SELECT "+opCols+" FROM vm_operations WHERE node_id=$1 AND pve_vmid=$2 "+
			"ORDER BY created_at DESC, id DESC LIMIT $3 OFFSET $4", nodeID, vmid, limit, offset)
	if err != nil {
		return nil, 0, classifyDBError(err)
	}
	defer rows.Close()

	out := make([]model.VMOperation, 0)
	for rows.Next() {
		var op model.VMOperation
		if err := rows.Scan(&op.ID, &op.NodeID, &op.PVEVmid, &op.Action, &op.Result,
			&op.ErrorMessage, &op.UserID, &op.OperatorType, &op.OperatorID, &op.CreatedAt); err != nil {
			return nil, 0, classifyDBError(err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, classifyDBError(err)
	}
	return out, total, nil
}
