package repository

import (
	"context"
	"fmt"

	"spark/model"
)

// ZoneRepository 负责持久化 model.Zone 行。
type ZoneRepository struct {
	pool pgxQuerier
}

// NewZoneRepository 创建由 pool 支撑的 ZoneRepository。
func NewZoneRepository(pool pgxQuerier) *ZoneRepository {
	return &ZoneRepository{pool: pool}
}

const zoneCols = "id, name, created_at"

// CreateZone 插入一个区域并返回它，且已填充 id 与 created_at。
func (r *ZoneRepository) CreateZone(ctx context.Context, name string) (*model.Zone, error) {
	z := &model.Zone{Name: name}
	err := r.pool.QueryRow(ctx,
		"INSERT INTO zones (name) VALUES ($1) RETURNING id, created_at", name,
	).Scan(&z.ID, &z.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return z, nil
}

// GetZone 返回指定 id 的区域；不存在时返回 pgx.ErrNoRows。
func (r *ZoneRepository) GetZone(ctx context.Context, id int64) (*model.Zone, error) {
	var z model.Zone
	err := r.pool.QueryRow(ctx, "SELECT "+zoneCols+" FROM zones WHERE id=$1", id).
		Scan(&z.ID, &z.Name, &z.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &z, nil
}

// ListZones 返回按 id 排序的全部区域。
func (r *ZoneRepository) ListZones(ctx context.Context) ([]model.Zone, error) {
	rows, err := r.pool.Query(ctx, "SELECT "+zoneCols+" FROM zones ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("zones: list: %w", err)
	}
	defer rows.Close()

	zones := make([]model.Zone, 0)
	for rows.Next() {
		var z model.Zone
		if err := rows.Scan(&z.ID, &z.Name, &z.CreatedAt); err != nil {
			return nil, fmt.Errorf("zones: scan: %w", err)
		}
		zones = append(zones, z)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("zones: iterate: %w", err)
	}
	return zones, nil
}

// ListZonesPage 返回按 id 排序的一页区域。它服务于分页的 GET /zones
// 端点；ListZones 仍可供内部全量扫描使用（创建时的重名校验、VM 列表的区域枚举）。
func (r *ZoneRepository) ListZonesPage(ctx context.Context, limit, offset int) ([]model.Zone, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT "+zoneCols+" FROM zones ORDER BY id LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, fmt.Errorf("zones: list page: %w", err)
	}
	defer rows.Close()

	zones := make([]model.Zone, 0)
	for rows.Next() {
		var z model.Zone
		if err := rows.Scan(&z.ID, &z.Name, &z.CreatedAt); err != nil {
			return nil, fmt.Errorf("zones: scan: %w", err)
		}
		zones = append(zones, z)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("zones: iterate: %w", err)
	}
	return zones, nil
}

// CountZones 返回区域总数，支撑 GET /zones 的 X-Total-Count 响应头。
func (r *ZoneRepository) CountZones(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM zones").Scan(&n); err != nil {
		return 0, fmt.Errorf("zones: count: %w", err)
	}
	return n, nil
}
