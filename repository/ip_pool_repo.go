package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark/model"
)

// ErrAllocationRetry 表示条件式 IP 领取在并发竞争中央败
// （地址在随机 SELECT 与 UPDATE 之间被其他事务取走）；
// 调用方应另选一个地址后重试。
var ErrAllocationRetry = errors.New("repository: concurrent ip claim conflict")

// IPPoolRepository 负责持久化 model.IPPool 行、展开的 model.IP 行
// 以及池-节点白名单（ip_pool_nodes）。
type IPPoolRepository struct {
	pool pgxQuerier
}

// NewIPPoolRepository 创建由 pool 支撑的 IPPoolRepository。
func NewIPPoolRepository(pool pgxQuerier) *IPPoolRepository {
	return &IPPoolRepository{pool: pool}
}

const poolCols = "id, zone_id, name, network_cidr, gateway, dns, created_at"

// CreatePoolWithIPs 在同一事务中插入池及其全部展开的地址行（批量插入）。
// 全局重复的地址（例如与其他池的 CIDR 重叠）会经由
// ips.ip 上的唯一约束产生 ErrConflict。
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

// batchIPInsert 为池的地址构建单条多行 INSERT 语句。
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

// GetPool 返回指定 id 的池；不存在时返回 pgx.ErrNoRows。
func (r *IPPoolRepository) GetPool(ctx context.Context, id int64) (*model.IPPool, error) {
	var p model.IPPool
	err := r.pool.QueryRow(ctx, "SELECT "+poolCols+" FROM ip_pools WHERE id=$1", id).
		Scan(&p.ID, &p.ZoneID, &p.Name, &p.NetworkCIDR, &p.Gateway, &p.DNS, &p.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &p, nil
}

// ListPools 返回按 id 排序的全部池。
func (r *IPPoolRepository) ListPools(ctx context.Context) ([]model.IPPool, error) {
	return r.listPools(ctx, "SELECT "+poolCols+" FROM ip_pools ORDER BY id")
}

// ListPoolsPage 返回按 id 排序的一页池。它服务于分页的 GET /ip-pools
// 端点；ListPools 仍可供内部全量扫描使用。
func (r *IPPoolRepository) ListPoolsPage(ctx context.Context, limit, offset int) ([]model.IPPool, error) {
	return r.listPools(ctx,
		"SELECT "+poolCols+" FROM ip_pools ORDER BY id LIMIT $1 OFFSET $2", limit, offset)
}

// CountPools 返回池的总数，支撑 GET /ip-pools 的 X-Total-Count 响应头。
func (r *IPPoolRepository) CountPools(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM ip_pools").Scan(&n); err != nil {
		return 0, fmt.Errorf("ip pools: count: %w", err)
	}
	return n, nil
}

// ListPoolsByZone 返回指定区域的池，按 id 排序。
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

// GetPoolNodes 返回池白名单中的节点（ip_pool_nodes），按节点 id 排序。
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
		if err := rows.Scan(&n.ID, &n.ZoneID, &n.Name, &n.PveName, &n.Host, &n.Port, &n.APIUser, &n.APITokenSecret, &n.Enabled, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("ip pools: scan pool node: %w", err)
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ip pools: iterate pool nodes: %w", err)
	}
	return nodes, nil
}

// SetPoolNodes 在单个事务中整体替换池的节点白名单（先 DELETE 旧行，再 INSERT 新行）。
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
	// selectFreeIPSQL 在分配事务内部挑选一个随机候选地址。
	// 该候选地址刻意不加锁。
	selectFreeIPSQL = "SELECT id, pool_id, ip, status, vm_id, updated_at FROM ips WHERE pool_id=$1 AND status='free' ORDER BY random() LIMIT 1"
	// claimFreeIPSQL 是原子领取语句：其 WHERE 守卫会对照最新已提交的行版本
	// 重新检查 status='free'，因此并发领取会在行锁上串行化，
	// 且恰好其中一个报告 RowsAffected=1。
	claimFreeIPSQL = "UPDATE ips SET status='used', vm_id=$2, updated_at=now() WHERE id=$1 AND status='free'"
)

// ClaimFreeIP 在调用方的事务内部原子地领取池中一个随机的空闲地址并返回它。
// 它是 AllocateFreeIP 的基于事务的核心，必须由 VM 创建/销毁流程（批次 7）
// 使用：按照 migration 0002 头部的约定，领取与 vms 行的写入必须发生在同一
// 事务中（先以 ip_id 为 NULL 插入 vms 行，再领取 ip，最后设置 vms.ip_id），
// 而释放必须在销毁事务中、vms 删除之前执行，这样被释放的地址永远不会以
// status='used' 且无归属者的状态留存。
//
// 并发语义：随机候选地址在无锁情况下选出，并由单条条件式 UPDATE 领取。
// 在 READ COMMITTED 下，Postgres 会对照最新已提交的行版本重新检查 WHERE
// 子句，因此当两个事务选中同一候选地址时恰好只有一个领取成功，
// 败者得到 RowsAffected=0 并返回 ErrAllocationRetry 让调用方尝试其他
// 候选地址。这里不使用 SELECT FOR UPDATE 长锁，一次领取除目标行外
// 不会阻塞任何其他内容。
//
// 当池中没有空闲地址时返回 pgx.ErrNoRows；当并发事务抢先领取了候选
// 地址时返回 ErrAllocationRetry。vmID 非空时把领取与既有的 vms 行关联；
// FK 环在调用方的事务内部解开（参见 migration 头部说明）。
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

// AllocateFreeIP 在自己的事务中领取池内一个随机的空闲地址并返回它。
// 独立调用方（IP 池服务）使用本方法；必须在更大的事务内预留地址的
// 流程（VM 创建，批次 7）则使用 ClaimFreeIP 并传入自己的事务。
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

// ReleaseIPByVMTx 在调用方的事务内释放 VM 占用的地址（如果有）。
// 幂等操作（释放未知的 vm id 不算错误）。按照 migration 0002 的约定，
// VM 销毁流程（批次 7）必须在删除 vms 行的同一事务中、删除之前执行本
// 方法，这样被释放的地址永远不会以 status='used' 且无归属者的状态留存。
func (r *IPPoolRepository) ReleaseIPByVMTx(ctx context.Context, tx pgx.Tx, vmID int64) error {
	_, err := tx.Exec(ctx,
		"UPDATE ips SET status='free', vm_id=NULL, updated_at=now() WHERE vm_id=$1", vmID)
	return err
}

// ReleaseIPByVM 在自己的事务中释放 VM 占用的地址。
// 独立调用方使用本方法；必须在更大的事务内释放的销毁流程
// 则使用 ReleaseIPByVMTx。
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
