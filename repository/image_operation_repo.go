package repository

import (
	"context"

	"spark/model"
)

// ImageOperationRepository 负责持久化 image_operations 行：镜像下载操作
// （下载到某节点，action 恒为 "download"）的审计记录。操作记录不随 images
// 行删除而删除（无外键 ON DELETE），仅按 image_id 关联。
type ImageOperationRepository struct {
	pool pgxQuerier
}

// NewImageOperationRepository 创建由 pool 支撑的 ImageOperationRepository。
func NewImageOperationRepository(pool pgxQuerier) *ImageOperationRepository {
	return &ImageOperationRepository{pool: pool}
}

// imgOpCols 是 image_operations 的读取列清单。error_message 列为
// NOT NULL DEFAULT ''（成功时为空串，COALESCE 仅为防御）；upid 可空
// （受理失败或尚未受理），经 COALESCE 归一化为空字符串；user_id 为
// 预留列（用户体系启用前恒为 NULL，不经 COALESCE）。
const imgOpCols = "id, image_id, node_id, action, result, COALESCE(error_message, '') AS error_message, COALESCE(upid, '') AS upid, user_id, created_at, updated_at"

// CreateOperation 插入一条操作记录并返回带 id/created_at/updated_at 的行。
// result 由调用方（service 层）传入 "running"（受理后先记运行中，由后台
// 任务后续更新为 success/failed）。user_id 预留（用户体系启用前始终为
// NULL）。upid 初始为空字符串：下载流程为先写 running 记录 → goroutine 内
// DownloadURL 受理 → WaitTask 轮询 → 随结果更新一并由 UpdateOperationResult
// 回填；未受理（预置失败）时 upid 保持空串。
func (r *ImageOperationRepository) CreateOperation(ctx context.Context, op model.ImageOperation) (*model.ImageOperation, error) {
	var created model.ImageOperation
	err := r.pool.QueryRow(ctx,
		"INSERT INTO image_operations (image_id, node_id, action, result, error_message, upid, user_id) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING "+imgOpCols,
		op.ImageID, op.NodeID, op.Action, op.Result, op.ErrorMessage, op.UPID, op.UserID,
	).Scan(&created.ID, &created.ImageID, &created.NodeID, &created.Action, &created.Result,
		&created.ErrorMessage, &created.UPID, &created.UserID, &created.CreatedAt, &created.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &created, nil
}

// UpdateOperationResult 幂等地将指定操作记录的结果更新为 result，并写入
// 失败原因 errorMessage（成功时为 ""）与 PVE 任务标识 upid；id 不存在时
// 不影响任何行，也不报错（调用方无需感知行是否存在）。
func (r *ImageOperationRepository) UpdateOperationResult(ctx context.Context, id int64, result, errorMessage, upid string) error {
	_, err := r.pool.Exec(ctx,
		"UPDATE image_operations SET result=$2, error_message=$3, upid=$4, updated_at=now() WHERE id=$1",
		id, result, errorMessage, upid,
	)
	if err != nil {
		return classifyDBError(err)
	}
	return nil
}

// ListOperationsByImage 按 created_at 倒序返回指定镜像的一页下载操作记录
// （新操作在前，created_at 相同的行按 id 倒序追加保证翻页稳定），以及
// 匹配该镜像的记录总数（limit/offset 之前），供分页与审计。
func (r *ImageOperationRepository) ListOperationsByImage(ctx context.Context, imageID int64, limit, offset int) ([]model.ImageOperation, int, error) {
	var total int
	if err := r.pool.QueryRow(ctx,
		"SELECT count(*) FROM image_operations WHERE image_id=$1", imageID,
	).Scan(&total); err != nil {
		return nil, 0, classifyDBError(err)
	}
	rows, err := r.pool.Query(ctx,
		"SELECT "+imgOpCols+" FROM image_operations WHERE image_id=$1 "+
			"ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3", imageID, limit, offset)
	if err != nil {
		return nil, 0, classifyDBError(err)
	}
	defer rows.Close()

	out := make([]model.ImageOperation, 0)
	for rows.Next() {
		var op model.ImageOperation
		if err := rows.Scan(&op.ID, &op.ImageID, &op.NodeID, &op.Action, &op.Result,
			&op.ErrorMessage, &op.UPID, &op.UserID, &op.CreatedAt, &op.UpdatedAt); err != nil {
			return nil, 0, classifyDBError(err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, classifyDBError(err)
	}
	return out, total, nil
}

// HasRunningOperation 报告指定镜像在指定节点上是否已有 result='running'
// 的未终态下载操作记录，供 service 层在受理下载前做幂等检查（同一镜像
// 同一节点不允许并发下载）。
func (r *ImageOperationRepository) HasRunningOperation(ctx context.Context, imageID, nodeID int64) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM image_operations WHERE image_id=$1 AND node_id=$2 AND result='running')",
		imageID, nodeID,
	).Scan(&exists)
	if err != nil {
		return false, classifyDBError(err)
	}
	return exists, nil
}
