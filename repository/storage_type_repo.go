package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark/model"
)

// StorageTypeRepository 负责持久化 model.StorageType 行。
type StorageTypeRepository struct {
	pool *pgxpool.Pool
}

// NewStorageTypeRepository 创建由 pool 支撑的 StorageTypeRepository。
func NewStorageTypeRepository(pool *pgxpool.Pool) *StorageTypeRepository {
	return &StorageTypeRepository{pool: pool}
}

const storageTypeCols = "id, name, display_name, pve_storage, created_at"

// Create 插入一个存储类型并返回它，且已填充 id 与 created_at。
// 名称重复时产生 ErrConflict。
func (r *StorageTypeRepository) Create(ctx context.Context, name, displayName, pveStorage string) (*model.StorageType, error) {
	st := &model.StorageType{Name: name, DisplayName: displayName, PVEStorage: pveStorage}
	err := r.pool.QueryRow(ctx,
		"INSERT INTO storage_types (name, display_name, pve_storage) VALUES ($1, $2, $3) RETURNING id, created_at",
		name, displayName, pveStorage,
	).Scan(&st.ID, &st.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return st, nil
}

// List 返回按 id 排序的全部存储类型。
func (r *StorageTypeRepository) List(ctx context.Context) ([]model.StorageType, error) {
	rows, err := r.pool.Query(ctx, "SELECT "+storageTypeCols+" FROM storage_types ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("storage types: list: %w", err)
	}
	defer rows.Close()

	types := make([]model.StorageType, 0)
	for rows.Next() {
		var st model.StorageType
		if err := rows.Scan(&st.ID, &st.Name, &st.DisplayName, &st.PVEStorage, &st.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage types: scan: %w", err)
		}
		types = append(types, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage types: iterate: %w", err)
	}
	return types, nil
}

// ListPage 返回按 id 排序的一页存储类型。它服务于分页的 GET /storage-types 端点。
func (r *StorageTypeRepository) ListPage(ctx context.Context, limit, offset int) ([]model.StorageType, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT "+storageTypeCols+" FROM storage_types ORDER BY id LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, fmt.Errorf("storage types: list page: %w", err)
	}
	defer rows.Close()

	types := make([]model.StorageType, 0)
	for rows.Next() {
		var st model.StorageType
		if err := rows.Scan(&st.ID, &st.Name, &st.DisplayName, &st.PVEStorage, &st.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage types: scan: %w", err)
		}
		types = append(types, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage types: iterate: %w", err)
	}
	return types, nil
}

// Count 返回存储类型总数，支撑 GET /storage-types 的 X-Total-Count 响应头。
func (r *StorageTypeRepository) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM storage_types").Scan(&n); err != nil {
		return 0, fmt.Errorf("storage types: count: %w", err)
	}
	return n, nil
}

// Get 返回指定 id 的存储类型；不存在时返回 pgx.ErrNoRows。
func (r *StorageTypeRepository) Get(ctx context.Context, id int64) (*model.StorageType, error) {
	var st model.StorageType
	err := r.pool.QueryRow(ctx, "SELECT "+storageTypeCols+" FROM storage_types WHERE id=$1", id).
		Scan(&st.ID, &st.Name, &st.DisplayName, &st.PVEStorage, &st.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &st, nil
}

// Update 替换指定 id 存储类型的元数据并返回更新后的行。存储类型
// 不存在时返回 pgx.ErrNoRows，名称重复时返回 ErrConflict。既有 VM
// 始终按 id 引用该行，因此更新映射不会改写它们（纯元数据）。
func (r *StorageTypeRepository) Update(ctx context.Context, id int64, name, displayName, pveStorage string) (*model.StorageType, error) {
	var st model.StorageType
	err := r.pool.QueryRow(ctx,
		"UPDATE storage_types SET name=$1, display_name=$2, pve_storage=$3 WHERE id=$4 RETURNING "+storageTypeCols,
		name, displayName, pveStorage, id,
	).Scan(&st.ID, &st.Name, &st.DisplayName, &st.PVEStorage, &st.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &st, nil
}

// Delete 删除指定 id 的存储类型。在同一事务中，当仍有 VM 引用该行时
// 拒绝删除（ErrInUse），以保证既有 VM 保留其存储映射。
// id 不存在时返回 pgx.ErrNoRows。
func (r *StorageTypeRepository) Delete(ctx context.Context, id int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage types: begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var inUse bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM vms WHERE storage_type_id=$1)", id).Scan(&inUse); err != nil {
		return fmt.Errorf("storage types: check vms references: %w", err)
	}
	if inUse {
		return ErrInUse
	}

	tag, err := tx.Exec(ctx, "DELETE FROM storage_types WHERE id=$1", id)
	if err != nil {
		return classifyDBError(err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage types: commit delete: %w", err)
	}
	return nil
}

// classifyDBError 将已知的数据库错误映射到仓库的哨兵错误上。
// 其余错误（包括 pgx.ErrNoRows）原样返回，调用方可以直接进行匹配。
// 通过包级助手被每个仓库共享（存储类型、镜像、区域、节点、IP 池）。
func classifyDBError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // 唯一约束冲突
			return ErrConflict
		case "23503": // 外键约束冲突
			return ErrInUse
		}
	}
	return err
}
