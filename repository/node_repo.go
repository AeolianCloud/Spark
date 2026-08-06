package repository

import (
	"context"
	"fmt"

	"spark/model"
)

// NodeRepository 负责持久化 model.PVENode 行。
type NodeRepository struct {
	pool pgxQuerier
}

// NewNodeRepository 创建由 pool 支撑的 NodeRepository。
func NewNodeRepository(pool pgxQuerier) *NodeRepository {
	return &NodeRepository{pool: pool}
}

const nodeCols = "id, zone_id, name, pve_name, host, port, api_user, api_token_secret, enabled, created_at"

// CreateNode 插入一个节点。节点包含业务名、PVE 集群节点名、主机地址、
// API 凭据与可选端口（默认 8006）。
// 凭据按设计以明文存储（vms 上的 password_encrypted 是唯一的加密字段）。
func (r *NodeRepository) CreateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error) {
	created := node
	err := r.pool.QueryRow(ctx,
		"INSERT INTO pve_nodes (zone_id, name, pve_name, host, port, api_user, api_token_secret, enabled) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id, created_at",
		node.ZoneID, node.Name, node.PveName, node.Host, node.Port, node.APIUser, node.APITokenSecret, node.Enabled,
	).Scan(&created.ID, &created.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &created, nil
}

// GetNode 返回指定 id 的节点；不存在时返回 pgx.ErrNoRows。
func (r *NodeRepository) GetNode(ctx context.Context, id int64) (*model.PVENode, error) {
	var n model.PVENode
	err := r.pool.QueryRow(ctx, "SELECT "+nodeCols+" FROM pve_nodes WHERE id=$1", id).
		Scan(&n.ID, &n.ZoneID, &n.Name, &n.PveName, &n.Host, &n.Port, &n.APIUser, &n.APITokenSecret, &n.Enabled, &n.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &n, nil
}

// ListNodesByZone 返回指定区域的节点，按 id 排序。
func (r *NodeRepository) ListNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	return r.listNodes(ctx, "SELECT "+nodeCols+" FROM pve_nodes WHERE zone_id=$1 ORDER BY id", zoneID)
}

// ListEnabledNodesByZone 返回指定区域内已启用的节点，按 id 排序。
func (r *NodeRepository) ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	return r.listNodes(ctx, "SELECT "+nodeCols+" FROM pve_nodes WHERE zone_id=$1 AND enabled ORDER BY id", zoneID)
}

func (r *NodeRepository) listNodes(ctx context.Context, sql string, args ...any) ([]model.PVENode, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("nodes: list: %w", err)
	}
	defer rows.Close()

	nodes := make([]model.PVENode, 0)
	for rows.Next() {
		var n model.PVENode
		if err := rows.Scan(&n.ID, &n.ZoneID, &n.Name, &n.PveName, &n.Host, &n.Port, &n.APIUser, &n.APITokenSecret, &n.Enabled, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("nodes: scan: %w", err)
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("nodes: iterate: %w", err)
	}
	return nodes, nil
}

// UpdateNode 替换指定 id 节点的全部字段并返回更新后的行。
// 节点不存在时返回 pgx.ErrNoRows。
func (r *NodeRepository) UpdateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error) {
	var n model.PVENode
	err := r.pool.QueryRow(ctx,
		"UPDATE pve_nodes SET zone_id=$1, name=$2, pve_name=$3, host=$4, port=$5, api_user=$6, api_token_secret=$7, enabled=$8 WHERE id=$9 RETURNING "+nodeCols,
		node.ZoneID, node.Name, node.PveName, node.Host, node.Port, node.APIUser, node.APITokenSecret, node.Enabled, node.ID,
	).Scan(&n.ID, &n.ZoneID, &n.Name, &n.PveName, &n.Host, &n.Port, &n.APIUser, &n.APITokenSecret, &n.Enabled, &n.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &n, nil
}
