package service

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/repository"
)

// IPPoolRepository 是 IPPoolService 依赖的 IP 池数据访问层。
type IPPoolRepository interface {
	CreatePoolWithIPs(ctx context.Context, pool model.IPPool, ips []model.IP) (*model.IPPool, error)
	GetPool(ctx context.Context, id int64) (*model.IPPool, error)
	ListPools(ctx context.Context) ([]model.IPPool, error)
	ListPoolsPage(ctx context.Context, limit, offset int) ([]model.IPPool, error)
	CountPools(ctx context.Context) (int, error)
	ListPoolsByZone(ctx context.Context, zoneID int64) ([]model.IPPool, error)
	GetPoolNodes(ctx context.Context, poolID int64) ([]model.PVENode, error)
	SetPoolNodes(ctx context.Context, poolID int64, nodeIDs []int64) error
	AllocateFreeIP(ctx context.Context, poolID int64, vmID *int64) (model.IP, error)
	ReleaseIPByVM(ctx context.Context, vmID int64) error
}

// IPPoolService 实现 IP 池的业务规则：CIDR 展开创建、节点白名单以及
// 并发安全的随机分配。
type IPPoolService struct {
	poolRepo IPPoolRepository
	zoneRepo ZoneRepository
	nodeRepo NodeRepository
}

// NewIPPoolService 使用给定的仓库创建一个 IPPoolService。
func NewIPPoolService(poolRepo IPPoolRepository, zoneRepo ZoneRepository, nodeRepo NodeRepository) *IPPoolService {
	return &IPPoolService{poolRepo: poolRepo, zoneRepo: zoneRepo, nodeRepo: nodeRepo}
}

// KindIPExhausted 表示"没有可用空闲地址"。该值位于 errors.go 中共享 kind
// 的 iota 范围之外（该范围归其他批次所有），以避免本文件与它们的改动产生
// 耦合。
const KindIPExhausted ErrorKind = 101

// ipExhaustedf 构造一个 KindIPExhausted 服务错误。
func ipExhaustedf(format string, args ...any) *Error {
	return &Error{Kind: KindIPExhausted, Message: fmt.Sprintf(format, args...)}
}

const (
	// maxPoolPrefixBits 是允许的最短网络掩码长度：一个 /22 池展开为 1024 个
	// 地址（约 1021 条可用 ip 行）；更大的网段（如含 2048 个地址的 /21）会被
	// 拒绝，以保持池创建的低开销并防止意外创建过大的池。
	maxPoolPrefixBits = 22
	// maxAllocationAttempts 限制并发抢占持续输掉条件更新竞态时的重试循环
	// 次数（repository.ErrAllocationRetry）。
	maxAllocationAttempts = 5
)

// CreateIPPool 在区域中创建一个池，并为 CIDR 的每个可用地址物化一条 ip 行
// （排除网络地址、广播地址和网关）。区域必须存在，名称在区域内必须唯一，
// CIDR 必须是 IPv4 且不能大于 /maxPoolPrefixBits，/31 和 /32 会被拒绝（没有
// 可用地址），网关必须在 CIDR 内，但既不能是网络地址也不能是广播地址。
func (s *IPPoolService) CreateIPPool(ctx context.Context, zoneID int64, name, networkCIDR, gateway, dns string) (*model.IPPool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, badRequestf("pool name is required")
	}
	if _, err := s.zoneRepo.GetZone(ctx, zoneID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("zone %d not found", zoneID)
		}
		return nil, fmt.Errorf("create pool: check zone: %w", err)
	}

	prefix, err := netip.ParsePrefix(networkCIDR)
	if err != nil {
		return nil, badRequestf("invalid network_cidr %q: %v", networkCIDR, err)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() {
		return nil, badRequestf("network_cidr %q: only IPv4 networks are supported", prefix)
	}
	if prefix.Bits() < maxPoolPrefixBits {
		return nil, badRequestf("network_cidr %q is too large: maximum pool size is /%d", prefix, maxPoolPrefixBits)
	}
	if prefix.Bits() >= 31 {
		return nil, badRequestf("network_cidr %q: a /%d network has no usable addresses (network, broadcast and gateway excluded)", prefix, prefix.Bits())
	}
	gatewayAddr, err := netip.ParseAddr(strings.TrimSpace(gateway))
	if err != nil {
		return nil, badRequestf("invalid gateway %q: %v", gateway, err)
	}
	if !prefix.Contains(gatewayAddr) {
		return nil, badRequestf("gateway %s is outside network %s", gatewayAddr, prefix)
	}
	if gatewayAddr == prefix.Addr() {
		return nil, badRequestf("gateway %s must not be the network address of %s", gatewayAddr, prefix)
	}
	if gatewayAddr == lastAddr4(prefix) {
		return nil, badRequestf("gateway %s must not be the broadcast address of %s", gatewayAddr, prefix)
	}

	pools, err := s.poolRepo.ListPoolsByZone(ctx, zoneID)
	if err != nil {
		return nil, fmt.Errorf("create pool: list pools: %w", err)
	}
	for _, p := range pools {
		if p.Name == name {
			return nil, conflictf("pool %q already exists in zone %d", name, zoneID)
		}
	}

	ips := expandPoolIPs(prefix, gatewayAddr)
	if len(ips) == 0 {
		return nil, badRequestf("network %s has no usable addresses after excluding network, broadcast and gateway", prefix)
	}

	pool := model.IPPool{
		ZoneID: zoneID, Name: name, NetworkCIDR: prefix.String(),
		Gateway: gatewayAddr.String(), DNS: strings.TrimSpace(dns),
	}
	created, err := s.poolRepo.CreatePoolWithIPs(ctx, pool, ips)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, conflictf("an address of %s is already used by another pool (overlapping CIDR)", prefix)
		}
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return created, nil
}

// expandPoolIPs 将前缀的每个地址物化为一条空闲 ip 行，但网络地址、IPv4
// 广播地址和网关除外。CreateIPPool 在调用本函数之前会拒绝位于网络/广播地址
// 上的网关，但排除逻辑仍保留为防御性措施，以保护直接调用者。
func expandPoolIPs(prefix netip.Prefix, gateway netip.Addr) []model.IP {
	addr := prefix.Addr()
	excluded := map[string]struct{}{addr.String(): {}}
	if addr.Is4() {
		excluded[lastAddr4(prefix).String()] = struct{}{}
	}
	excluded[gateway.String()] = struct{}{}

	ips := make([]model.IP, 0)
	for prefix.Contains(addr) {
		if _, skip := excluded[addr.String()]; !skip {
			ips = append(ips, model.IP{IP: addr.String(), Status: model.IPStatusFree})
		}
		addr = addr.Next()
		if !addr.IsValid() {
			break
		}
	}
	return ips
}

// lastAddr4 计算 IPv4 前缀的广播地址（所有主机位均置 1）。
func lastAddr4(prefix netip.Prefix) netip.Addr {
	a := prefix.Addr().As4()
	for i := prefix.Bits(); i < 32; i++ {
		a[i/8] |= 1 << (7 - uint(i%8))
	}
	return netip.AddrFrom4(a)
}

// ListPools 返回 GET /ip-pools 的池列表。设置了 zoneID 时返回该区域的全部
// 池——一个区域包含的池很少，因此按区域过滤的列表刻意不做分页（total 为该
// 区域的池数量）。未设置 zoneID 时按 limit/offset 分页，total 为全部池数量。
// 区域必须存在。
func (s *IPPoolService) ListPools(ctx context.Context, zoneID *int64, limit, offset int) ([]model.IPPool, int, error) {
	if zoneID == nil {
		pools, err := s.poolRepo.ListPoolsPage(ctx, limit, offset)
		if err != nil {
			return nil, 0, fmt.Errorf("list pools: %w", err)
		}
		total, err := s.poolRepo.CountPools(ctx)
		if err != nil {
			return nil, 0, fmt.Errorf("list pools: count: %w", err)
		}
		return pools, total, nil
	}
	if _, err := s.zoneRepo.GetZone(ctx, *zoneID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, notFoundf("zone %d not found", *zoneID)
		}
		return nil, 0, fmt.Errorf("list pools: check zone: %w", err)
	}
	pools, err := s.poolRepo.ListPoolsByZone(ctx, *zoneID)
	if err != nil {
		return nil, 0, fmt.Errorf("list pools: %w", err)
	}
	return pools, len(pools), nil
}

// GetPoolNodes 返回池白名单中的节点；池必须存在。
func (s *IPPoolService) GetPoolNodes(ctx context.Context, poolID int64) ([]model.PVENode, error) {
	if _, err := s.poolRepo.GetPool(ctx, poolID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("pool %d not found", poolID)
		}
		return nil, fmt.Errorf("get pool nodes: %w", err)
	}
	nodes, err := s.poolRepo.GetPoolNodes(ctx, poolID)
	if err != nil {
		return nil, fmt.Errorf("get pool nodes: %w", err)
	}
	return nodes, nil
}

// SetPoolNodes 替换池的节点白名单。每个节点都必须存在且属于池所在区域
// （跨区域白名单视为冲突）；重复的 id 会被去重并排序。
func (s *IPPoolService) SetPoolNodes(ctx context.Context, poolID int64, nodeIDs []int64) error {
	pool, err := s.poolRepo.GetPool(ctx, poolID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return notFoundf("pool %d not found", poolID)
		}
		return fmt.Errorf("set pool nodes: %w", err)
	}
	seen := make(map[int64]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if _, dup := seen[nodeID]; dup {
			continue
		}
		seen[nodeID] = struct{}{}
		node, err := s.nodeRepo.GetNode(ctx, nodeID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return notFoundf("node %d not found", nodeID)
			}
			return fmt.Errorf("set pool nodes: check node: %w", err)
		}
		if node.ZoneID != pool.ZoneID {
			return conflictf("node %d belongs to zone %d, pool %d belongs to zone %d", nodeID, node.ZoneID, poolID, pool.ZoneID)
		}
	}
	deduped := make([]int64, 0, len(seen))
	for id := range seen {
		deduped = append(deduped, id)
	}
	sort.Slice(deduped, func(i, j int) bool { return deduped[i] < deduped[j] })
	if err := s.poolRepo.SetPoolNodes(ctx, poolID, deduped); err != nil {
		return fmt.Errorf("set pool nodes: %w", err)
	}
	return nil
}

// AllocateIP 抢占池中一个随机的空闲地址并返回。抢占本身是原子的（仓库中的
// 单条条件 UPDATE），因此并发分配绝不会给出相同的地址；当抢占在竞态中失败
// 时，调用会重试，最多 maxAllocationAttempts 次。地址耗尽的池（或一直输掉
// 竞态的池）会产生 KindIPExhausted 错误。
func (s *IPPoolService) AllocateIP(ctx context.Context, poolID int64) (model.IP, error) {
	if _, err := s.poolRepo.GetPool(ctx, poolID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.IP{}, notFoundf("pool %d not found", poolID)
		}
		return model.IP{}, fmt.Errorf("allocate ip: %w", err)
	}
	for attempt := 0; attempt < maxAllocationAttempts; attempt++ {
		ip, err := s.poolRepo.AllocateFreeIP(ctx, poolID, nil)
		switch {
		case err == nil:
			return ip, nil
		case errors.Is(err, repository.ErrAllocationRetry):
			continue
		case errors.Is(err, pgx.ErrNoRows):
			return model.IP{}, ipExhaustedf("pool %d has no free ip", poolID)
		default:
			return model.IP{}, fmt.Errorf("allocate ip in pool %d: %w", poolID, err)
		}
	}
	return model.IP{}, ipExhaustedf("pool %d has no free ip after %d attempts", poolID, maxAllocationAttempts)
}

// AllocateIPInZone 在确认池属于该区域后，抢占池中一个随机的空闲地址。
// 跨区域使用（ip-pool 规范）会以冲突错误拒绝；分配本身委托给 AllocateIP。
// VM 创建（任务 7.1）选择池后调用本函数以强制区域边界。
func (s *IPPoolService) AllocateIPInZone(ctx context.Context, zoneID, poolID int64) (model.IP, error) {
	pool, err := s.poolRepo.GetPool(ctx, poolID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.IP{}, notFoundf("pool %d not found", poolID)
		}
		return model.IP{}, fmt.Errorf("allocate ip in zone: %w", err)
	}
	if pool.ZoneID != zoneID {
		return model.IP{}, conflictf("pool %d belongs to zone %d, not zone %d", poolID, pool.ZoneID, zoneID)
	}
	return s.AllocateIP(ctx, poolID)
}

// ReleaseIPByVM 释放该 VM 抢占的地址（如有）。幂等：未知的 vm id 不是错误。
// 按照迁移约定，调用方（VM 销毁，任务 7.4）必须在删除 vms 行的同一事务内、
// 且先于删除执行本调用，这样被释放的地址永远不会以 status='used' 且无所有
// 者的状态残留。
func (s *IPPoolService) ReleaseIPByVM(ctx context.Context, vmID int64) error {
	if err := s.poolRepo.ReleaseIPByVM(ctx, vmID); err != nil {
		return fmt.Errorf("release ip of vm %d: %w", vmID, err)
	}
	return nil
}
