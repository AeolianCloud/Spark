package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark/model"
)

// StorageTypeRepository 负责持久化 model.StorageType 行。存储类型由扫描
// （StorageTypeService.SyncZone）权威同步：pve_storage/type/content/nodes
// 由 PVE 填充（nodes 为挂载节点快照，设计 D8），name/enabled 由管理员
// 维护，因此不再提供手动 Create。
type StorageTypeRepository struct {
	pool *pgxpool.Pool
}

// NewStorageTypeRepository 创建由 pool 支撑的 StorageTypeRepository。
func NewStorageTypeRepository(pool *pgxpool.Pool) *StorageTypeRepository {
	return &StorageTypeRepository{pool: pool}
}

// storageTypeCols 是行投影的完整列清单；name/type/content 可空（未扫描的
// 存量行或扫描新建时 name 为 NULL），以 *string 承载 NULL。nodes 为非空
// 逗号分隔串（PVE 原文，空串 = 不限制节点），scanStorageType 拆分后写入
// model.StorageType.Nodes。
const storageTypeCols = "id, zone_id, name, pve_storage, enabled, type, content, nodes, created_at"

// scanStorageType 按 storageTypeCols 的顺序扫描一行 StorageType；nodes
// 列（TEXT）按逗号拆分为 []string，空串归一为空切片（非 nil，保证对外
// JSON 输出 [] 而非 null）。
func scanStorageType(row pgx.Row) (*model.StorageType, error) {
	var st model.StorageType
	var nodes string
	if err := row.Scan(&st.ID, &st.ZoneID, &st.Name, &st.PVEStorage, &st.Enabled, &st.Type, &st.Content, &nodes, &st.CreatedAt); err != nil {
		return nil, err
	}
	st.Nodes = splitStorageNodes(nodes)
	return &st, nil
}

// splitStorageNodes 把数据库中的逗号分隔节点串解析为节点名切片（容忍
// 空白，剔除空段）；空串（'' = 不限制节点）返回空切片（非 nil，与
// model.StorageType.Nodes 的"空切片 = 不限制"语义一致）。
func splitStorageNodes(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// UpsertByZonePveStorage 以 (zone_id, pve_storage) 为匹配键插入或更新一行：
// 新建时 name 为 NULL、enabled 默认开启；已存在时仅覆盖 type/content/nodes
// 快照，绝不触碰管理员的 name/enabled（扫描同步语义，设计 D3/D8）。nodes
// 是该存储挂载的节点名列表（空切片 = 不限制节点），join 为逗号串落库。
// 第二个返回值报告是否为新建（xmax=0 是 PostgreSQL 官方推荐的"是否插入"
// 判定：插入行为 0，更新行为非 0）。
func (r *StorageTypeRepository) UpsertByZonePveStorage(ctx context.Context, zoneID int64, pveStorage, stype, content string, nodes []string) (*model.StorageType, bool, error) {
	var st model.StorageType
	var inserted bool
	var nodesRaw string
	err := r.pool.QueryRow(ctx,
		"INSERT INTO storage_types (zone_id, name, pve_storage, enabled, type, content, nodes) "+
			"VALUES ($1, NULL, $2, true, $3, $4, $5) "+
			"ON CONFLICT (zone_id, pve_storage) DO UPDATE SET type=EXCLUDED.type, content=EXCLUDED.content, nodes=EXCLUDED.nodes "+
			"RETURNING "+storageTypeCols+", (xmax = 0) AS inserted",
		zoneID, pveStorage, stype, content, strings.Join(nodes, ","),
	).Scan(&st.ID, &st.ZoneID, &st.Name, &st.PVEStorage, &st.Enabled, &st.Type, &st.Content, &nodesRaw, &st.CreatedAt, &inserted)
	if err != nil {
		return nil, false, classifyDBError(err)
	}
	st.Nodes = splitStorageNodes(nodesRaw)
	return &st, inserted, nil
}

// UpdateMeta 仅更新指定 id 行的管理员元数据（name/enabled，设计 D3）：
// 指针为 nil 的字段保持原值；name 非 nil 时应用该值（空串写入 NULL，
// 表示业务名置空）；enabled 非 nil 时切换开关。pve_storage 是扫描权威
// 字段，不可在此修改。id 不存在时返回 pgx.ErrNoRows。
func (r *StorageTypeRepository) UpdateMeta(ctx context.Context, id int64, name *string, enabled *bool) (*model.StorageType, error) {
	var sets []string
	args := []any{id}
	if name != nil {
		args = append(args, name)
		// NULLIF：空串按"置空"写入 NULL；非空串原样写入。
		sets = append(sets, fmt.Sprintf("name=NULLIF($%d, '')", len(args)))
	}
	if enabled != nil {
		args = append(args, enabled)
		sets = append(sets, fmt.Sprintf("enabled=$%d", len(args)))
	}
	if len(sets) == 0 {
		return nil, fmt.Errorf("storage types: update meta %d: no fields to update", id)
	}
	row := r.pool.QueryRow(ctx,
		"UPDATE storage_types SET "+strings.Join(sets, ", ")+" WHERE id=$1 RETURNING "+storageTypeCols,
		args...)
	st, err := scanStorageType(row)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return st, nil
}

// ListPage 返回按 id 排序的一页存储类型。zoneID 非 nil 时仅返回该 zone
// 的行（GET /storage-types?zone_id= 与扫描删除对齐共用）；limit <= 0 表示
// 不限制行数（扫描全量列举用）。
func (r *StorageTypeRepository) ListPage(ctx context.Context, zoneID *int64, limit, offset int) ([]model.StorageType, error) {
	query := "SELECT " + storageTypeCols + " FROM storage_types"
	args := make([]any, 0, 3)
	if zoneID != nil {
		args = append(args, *zoneID)
		query += " WHERE zone_id=$1"
	}
	query += " ORDER BY id"
	if limit > 0 {
		args = append(args, limit, offset)
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	}
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage types: list page: %w", err)
	}
	defer rows.Close()

	types := make([]model.StorageType, 0)
	for rows.Next() {
		st, err := scanStorageType(rows)
		if err != nil {
			return nil, fmt.Errorf("storage types: scan: %w", err)
		}
		types = append(types, *st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage types: iterate: %w", err)
	}
	return types, nil
}

// Count 返回存储类型总数（zoneID 非 nil 时仅统计该 zone），支撑
// GET /storage-types 的 X-Total-Count 响应头。
func (r *StorageTypeRepository) Count(ctx context.Context, zoneID *int64) (int, error) {
	query := "SELECT count(*) FROM storage_types"
	var args []any
	if zoneID != nil {
		args = append(args, *zoneID)
		query += " WHERE zone_id=$1"
	}
	var n int
	if err := r.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage types: count: %w", err)
	}
	return n, nil
}

// Get 返回指定 id 的存储类型；不存在时返回 pgx.ErrNoRows。
func (r *StorageTypeRepository) Get(ctx context.Context, id int64) (*model.StorageType, error) {
	st, err := scanStorageType(r.pool.QueryRow(ctx, "SELECT "+storageTypeCols+" FROM storage_types WHERE id=$1", id))
	if err != nil {
		return nil, classifyDBError(err)
	}
	return st, nil
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
