package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark/model"
)

// ErrAllocationRetry signals that the conditional IP claim lost a concurrent
// race (the address was taken between the random SELECT and the UPDATE);
// callers should pick another address and retry.
var ErrAllocationRetry = errors.New("repository: concurrent ip claim conflict")

// IPPoolRepository persists model.IPPool rows, the expanded model.IP rows and
// the pool-node whitelist (ip_pool_nodes).
type IPPoolRepository struct {
	pool pgxQuerier
}

// NewIPPoolRepository creates an IPPoolRepository backed by pool.
func NewIPPoolRepository(pool pgxQuerier) *IPPoolRepository {
	return &IPPoolRepository{pool: pool}
}

const poolCols = "id, zone_id, name, network_cidr, gateway, dns, created_at"

// CreatePoolWithIPs inserts the pool and, in the same transaction, all of its
// expanded address rows (batch insert). A globally duplicate address (e.g.
// overlapping CIDR in another pool) yields ErrConflict via the unique
// constraint on ips.ip.
func (r *IPPoolRepository) CreatePoolWithIPs(ctx context.Context, pool model.IPPool, ips []model.IP) (*model.IPPool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("ip pools: begin create tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created := pool
	err = tx.QueryRow(ctx,
		"INSERT INTO ip_pools (zone_id, name, network_cidr, gateway, dns) VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at",
		pool.ZoneID, pool.Name, pool.NetworkCIDR, pool.Gateway, pool.DNS,
	).Scan(&created.ID, &created.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}

	if len(ips) > 0 {
		stmt, args := batchIPInsert(created.ID, ips)
		if _, err := tx.Exec(ctx, stmt, args...); err != nil {
			return nil, classifyDBError(err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("ip pools: commit create: %w", err)
	}
	return &created, nil
}

// batchIPInsert builds a single multi-row INSERT for the pool's addresses.
func batchIPInsert(poolID int64, ips []model.IP) (string, []any) {
	values := make([]string, 0, len(ips))
	args := make([]any, 0, len(ips)+1)
	args = append(args, poolID)
	for i, ip := range ips {
		values = append(values, fmt.Sprintf("($1, $%d)", i+2))
		args = append(args, ip.IP)
	}
	return "INSERT INTO ips (pool_id, ip) VALUES " + strings.Join(values, ", "), args
}

// GetPool returns the pool with the given id, or pgx.ErrNoRows when absent.
func (r *IPPoolRepository) GetPool(ctx context.Context, id int64) (*model.IPPool, error) {
	var p model.IPPool
	err := r.pool.QueryRow(ctx, "SELECT "+poolCols+" FROM ip_pools WHERE id=$1", id).
		Scan(&p.ID, &p.ZoneID, &p.Name, &p.NetworkCIDR, &p.Gateway, &p.DNS, &p.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &p, nil
}

// ListPools returns all pools ordered by id.
func (r *IPPoolRepository) ListPools(ctx context.Context) ([]model.IPPool, error) {
	return r.listPools(ctx, "SELECT "+poolCols+" FROM ip_pools ORDER BY id")
}

// ListPoolsByZone returns the pools of a zone ordered by id.
func (r *IPPoolRepository) ListPoolsByZone(ctx context.Context, zoneID int64) ([]model.IPPool, error) {
	return r.listPools(ctx, "SELECT "+poolCols+" FROM ip_pools WHERE zone_id=$1 ORDER BY id", zoneID)
}

func (r *IPPoolRepository) listPools(ctx context.Context, sql string, args ...any) ([]model.IPPool, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("ip pools: list: %w", err)
	}
	defer rows.Close()

	pools := make([]model.IPPool, 0)
	for rows.Next() {
		var p model.IPPool
		if err := rows.Scan(&p.ID, &p.ZoneID, &p.Name, &p.NetworkCIDR, &p.Gateway, &p.DNS, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("ip pools: scan: %w", err)
		}
		pools = append(pools, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ip pools: iterate: %w", err)
	}
	return pools, nil
}

// GetPoolNodes returns the nodes whitelisted for the pool (ip_pool_nodes)
// ordered by node id.
func (r *IPPoolRepository) GetPoolNodes(ctx context.Context, poolID int64) ([]model.PVENode, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT n."+nodeCols+" FROM pve_nodes n JOIN ip_pool_nodes pn ON pn.node_id = n.id WHERE pn.ip_pool_id=$1 ORDER BY n.id",
		poolID,
	)
	if err != nil {
		return nil, fmt.Errorf("ip pools: list pool nodes: %w", err)
	}
	defer rows.Close()

	nodes := make([]model.PVENode, 0)
	for rows.Next() {
		var n model.PVENode
		if err := rows.Scan(&n.ID, &n.ZoneID, &n.Name, &n.Host, &n.APIUser, &n.APITokenSecret, &n.Enabled, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("ip pools: scan pool node: %w", err)
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ip pools: iterate pool nodes: %w", err)
	}
	return nodes, nil
}

// SetPoolNodes replaces the pool's node whitelist in a single transaction
// (DELETE old rows, INSERT new ones).
func (r *IPPoolRepository) SetPoolNodes(ctx context.Context, poolID int64, nodeIDs []int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ip pools: begin set nodes tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "DELETE FROM ip_pool_nodes WHERE ip_pool_id=$1", poolID); err != nil {
		return fmt.Errorf("ip pools: clear pool nodes: %w", err)
	}
	for _, nodeID := range nodeIDs {
		if _, err := tx.Exec(ctx, "INSERT INTO ip_pool_nodes (ip_pool_id, node_id) VALUES ($1, $2)", poolID, nodeID); err != nil {
			return fmt.Errorf("ip pools: add pool node %d: %w", nodeID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ip pools: commit set nodes: %w", err)
	}
	return nil
}

const (
	// selectFreeIPSQL picks a random candidate address inside the allocation
	// transaction. The candidate is deliberately not locked.
	selectFreeIPSQL = "SELECT id, pool_id, ip, status, vm_id, updated_at FROM ips WHERE pool_id=$1 AND status='free' ORDER BY random() LIMIT 1"
	// claimFreeIPSQL is the atomic claim: its WHERE guard re-checks
	// status='free' against the latest committed row version, so concurrent
	// claims serialize on the row lock and exactly one of them reports
	// RowsAffected=1.
	claimFreeIPSQL = "UPDATE ips SET status='used', vm_id=$2, updated_at=now() WHERE id=$1 AND status='free'"
)

// ClaimFreeIP atomically claims one random free address of the pool inside
// the caller's transaction and returns it. It is the Tx-aware core of
// AllocateFreeIP and must be used by the VM create/destroy flows (batch 7):
// per the migration 0002 header conventions, the claim and the vms row write
// must happen in the same transaction (INSERT vms with ip_id NULL, claim the
// ip, then set vms.ip_id), and the release must run in the destroy
// transaction BEFORE the vms delete, so a freed address never ends up with
// status='used' and no owner.
//
// Concurrency semantics: the random candidate is selected without any lock
// and claimed by a single conditional UPDATE. Under READ COMMITTED Postgres
// re-checks the WHERE clause on the latest committed row version, so when two
// transactions pick the same candidate exactly one claim wins and the loser
// gets RowsAffected=0 and returns ErrAllocationRetry for the caller to try
// another candidate. No SELECT FOR UPDATE long locks are used, and a claim
// never blocks on anything but the row it targets.
//
// It returns pgx.ErrNoRows when the pool has no free address and
// ErrAllocationRetry when a concurrent transaction claimed the candidate
// first. vmID links the claim to an existing vms row when set; the FK cycle
// is resolved inside the caller's transaction (see the migration header).
func (r *IPPoolRepository) ClaimFreeIP(ctx context.Context, tx pgx.Tx, poolID int64, vmID *int64) (model.IP, error) {
	var ip model.IP
	err := tx.QueryRow(ctx, selectFreeIPSQL, poolID).
		Scan(&ip.ID, &ip.PoolID, &ip.IP, &ip.Status, &ip.VMID, &ip.UpdatedAt)
	if err != nil {
		return model.IP{}, err
	}

	tag, err := tx.Exec(ctx, claimFreeIPSQL, ip.ID, vmID)
	if err != nil {
		return model.IP{}, err
	}
	if tag.RowsAffected() != 1 {
		return model.IP{}, ErrAllocationRetry
	}

	ip.Status = model.IPStatusUsed
	if vmID != nil {
		ip.VMID = vmID
	}
	return ip, nil
}

// AllocateFreeIP claims one random free address of the pool in its own
// transaction and returns it. Standalone callers (the IP pool service) use
// this; flows that must reserve an address inside a larger transaction (VM
// creation, batch 7) use ClaimFreeIP with their own tx.
func (r *IPPoolRepository) AllocateFreeIP(ctx context.Context, poolID int64, vmID *int64) (model.IP, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return model.IP{}, fmt.Errorf("ip pools: begin allocate tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ip, err := r.ClaimFreeIP(ctx, tx, poolID, vmID)
	if err != nil {
		return model.IP{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.IP{}, fmt.Errorf("ip pools: commit allocate: %w", err)
	}
	return ip, nil
}

// ReleaseIPByVMTx frees the address claimed by the VM, if any, inside the
// caller's transaction. Idempotent (releasing an unknown vm id is not an
// error). Per the migration 0002 conventions the VM destroy flow (batch 7)
// must run this in the same transaction that deletes the vms row, before the
// delete, so a freed address never ends up with status='used' and no owner.
func (r *IPPoolRepository) ReleaseIPByVMTx(ctx context.Context, tx pgx.Tx, vmID int64) error {
	_, err := tx.Exec(ctx,
		"UPDATE ips SET status='free', vm_id=NULL, updated_at=now() WHERE vm_id=$1", vmID)
	return err
}

// ReleaseIPByVM frees the address claimed by the VM in its own transaction.
// Standalone callers use this; destroy flows that must release inside a
// larger transaction use ReleaseIPByVMTx.
func (r *IPPoolRepository) ReleaseIPByVM(ctx context.Context, vmID int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ip pools: begin release tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := r.ReleaseIPByVMTx(ctx, tx, vmID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
