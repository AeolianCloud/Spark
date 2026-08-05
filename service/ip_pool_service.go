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

// IPPoolRepository is the IP pool data access the IPPoolService depends on.
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

// IPPoolService implements the business rules for IP pools: creation with
// CIDR expansion, node whitelisting and concurrency-safe random allocation.
type IPPoolService struct {
	poolRepo IPPoolRepository
	zoneRepo ZoneRepository
	nodeRepo NodeRepository
}

// NewIPPoolService creates an IPPoolService backed by the repositories.
func NewIPPoolService(poolRepo IPPoolRepository, zoneRepo ZoneRepository, nodeRepo NodeRepository) *IPPoolService {
	return &IPPoolService{poolRepo: poolRepo, zoneRepo: zoneRepo, nodeRepo: nodeRepo}
}

// KindIPExhausted marks "no free address available". The value sits outside
// the iota range of the shared kinds in errors.go (owned by other batches) to
// avoid coupling this file to their edits.
const KindIPExhausted ErrorKind = 101

// ipExhaustedf builds a KindIPExhausted service error.
func ipExhaustedf(format string, args ...any) *Error {
	return &Error{Kind: KindIPExhausted, Message: fmt.Sprintf(format, args...)}
}

const (
	// maxPoolPrefixBits is the smallest allowed network mask length: a /22
	// pool expands to 1024 addresses (~1021 usable ip rows); anything larger
	// (e.g. /21 with 2048 addresses) is rejected to keep pool creation cheap
	// and prevent accidentally huge pools.
	maxPoolPrefixBits = 22
	// maxAllocationAttempts bounds the retry loop when concurrent claims
	// keep losing the conditional-update race (repository.ErrAllocationRetry).
	maxAllocationAttempts = 5
)

// CreateIPPool creates a pool in a zone and materializes one ip row per
// usable address of the CIDR (network, broadcast and gateway excluded). The
// zone must exist, the name must be unique within the zone, the CIDR must be
// IPv4 and no larger than /maxPoolPrefixBits, /31 and /32 are rejected (no
// usable addresses), and the gateway must fall inside the CIDR but neither on
// the network address nor on the broadcast address.
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

// expandPoolIPs materializes every address of the prefix as a free ip row
// except the network address, the IPv4 broadcast address and the gateway.
// CreateIPPool rejects a gateway on the network/broadcast address before this
// is called, but the exclusions are kept defensively for direct callers.
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

// lastAddr4 computes the broadcast address of an IPv4 prefix (all host bits
// set).
func lastAddr4(prefix netip.Prefix) netip.Addr {
	a := prefix.Addr().As4()
	for i := prefix.Bits(); i < 32; i++ {
		a[i/8] |= 1 << (7 - uint(i%8))
	}
	return netip.AddrFrom4(a)
}

// ListPools returns the pools of GET /ip-pools. With zoneID set the zone's
// pools are returned in full — a zone embeds few pools, so the zone-filtered
// list is deliberately not paginated (total is the zone's pool count).
// Without zoneID the pools are paged by limit/offset and total is the
// overall pool count. The zone must exist.
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

// GetPoolNodes returns the nodes whitelisted for the pool; the pool must
// exist.
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

// SetPoolNodes replaces the pool's node whitelist. Every node must exist and
// belong to the pool's zone (cross-zone whitelisting is a conflict);
// duplicate ids are collapsed and sorted.
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

// AllocateIP claims a random free address of the pool and returns it. The
// claim itself is atomic (single conditional UPDATE in the repository), so
// concurrent allocations never hand out the same address; when a claim loses
// a race the call retries, up to maxAllocationAttempts. An exhausted pool
// (or one that keeps losing the race) yields a KindIPExhausted error.
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

// AllocateIPInZone claims a random free address of the pool after checking
// that the pool belongs to the zone. Cross-zone usage (ip-pool spec) is
// refused with a conflict; the allocation itself is delegated to AllocateIP.
// VM creation (task 7.1) selects the pool and calls this to enforce the zone
// boundary.
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

// ReleaseIPByVM frees the address claimed by the VM, if any. Idempotent: an
// unknown vm id is not an error. Per the migration conventions the caller
// (VM destroy, task 7.4) must run this inside the same transaction that
// deletes the vms row and before the delete, so a freed address never ends up
// with status='used' and no owner.
func (s *IPPoolService) ReleaseIPByVM(ctx context.Context, vmID int64) error {
	if err := s.poolRepo.ReleaseIPByVM(ctx, vmID); err != nil {
		return fmt.Errorf("release ip of vm %d: %w", vmID, err)
	}
	return nil
}
