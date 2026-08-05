package repository

import (
	"context"
	"fmt"

	"spark/model"
)

// NodeRepository persists model.PVENode rows.
type NodeRepository struct {
	pool pgxQuerier
}

// NewNodeRepository creates a NodeRepository backed by pool.
func NewNodeRepository(pool pgxQuerier) *NodeRepository {
	return &NodeRepository{pool: pool}
}

const nodeCols = "id, zone_id, name, host, api_user, api_token_secret, enabled, created_at"

// CreateNode inserts a node. Credentials are stored in plain text by design
// (password_encrypted on vms is the only encrypted field).
func (r *NodeRepository) CreateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error) {
	created := node
	err := r.pool.QueryRow(ctx,
		"INSERT INTO pve_nodes (zone_id, name, host, api_user, api_token_secret, enabled) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at",
		node.ZoneID, node.Name, node.Host, node.APIUser, node.APITokenSecret, node.Enabled,
	).Scan(&created.ID, &created.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &created, nil
}

// GetNode returns the node with the given id, or pgx.ErrNoRows when absent.
func (r *NodeRepository) GetNode(ctx context.Context, id int64) (*model.PVENode, error) {
	var n model.PVENode
	err := r.pool.QueryRow(ctx, "SELECT "+nodeCols+" FROM pve_nodes WHERE id=$1", id).
		Scan(&n.ID, &n.ZoneID, &n.Name, &n.Host, &n.APIUser, &n.APITokenSecret, &n.Enabled, &n.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &n, nil
}

// ListNodesByZone returns the nodes of a zone ordered by id.
func (r *NodeRepository) ListNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	return r.listNodes(ctx, "SELECT "+nodeCols+" FROM pve_nodes WHERE zone_id=$1 ORDER BY id", zoneID)
}

// ListEnabledNodesByZone returns the enabled nodes of a zone ordered by id.
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
		if err := rows.Scan(&n.ID, &n.ZoneID, &n.Name, &n.Host, &n.APIUser, &n.APITokenSecret, &n.Enabled, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("nodes: scan: %w", err)
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("nodes: iterate: %w", err)
	}
	return nodes, nil
}

// UpdateNode replaces all fields of the node with the given id and returns
// the updated row. It returns pgx.ErrNoRows when absent.
func (r *NodeRepository) UpdateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error) {
	var n model.PVENode
	err := r.pool.QueryRow(ctx,
		"UPDATE pve_nodes SET zone_id=$1, name=$2, host=$3, api_user=$4, api_token_secret=$5, enabled=$6 WHERE id=$7 RETURNING "+nodeCols,
		node.ZoneID, node.Name, node.Host, node.APIUser, node.APITokenSecret, node.Enabled, node.ID,
	).Scan(&n.ID, &n.ZoneID, &n.Name, &n.Host, &n.APIUser, &n.APITokenSecret, &n.Enabled, &n.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &n, nil
}
