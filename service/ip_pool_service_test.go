package service

import (
	"context"
	"errors"
	"math/rand"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/repository"
)

// fakeIPPoolRepository 模拟 IP 池仓库。对于分配，它模拟真实的先 SELECT 后
// 条件 UPDATE 行为：先挑选候选但不持有抢占，其他 goroutine 可能先抢到，只有
// 在地址仍为空闲时抢占才被授予（否则返回 ErrAllocationRetry，镜像条件 UPDATE
// 影响 0 行的情形）。
type fakeIPPoolRepository struct {
	mu          sync.Mutex
	free        []string
	claimed     map[string]bool
	pool        *model.IPPool
	poolErr     error
	poolsByZone []model.IPPool
	nodes       []model.PVENode
	setNodes    []int64
	released    []int64
}

func newFakeIPPoolRepository(free []string) *fakeIPPoolRepository {
	return &fakeIPPoolRepository{
		free:    append([]string(nil), free...),
		claimed: make(map[string]bool),
		pool:    &model.IPPool{ID: 1, ZoneID: 1, Name: "default", NetworkCIDR: "10.0.0.0/24"},
	}
}

func (f *fakeIPPoolRepository) CreatePoolWithIPs(ctx context.Context, pool model.IPPool, ips []model.IP) (*model.IPPool, error) {
	created := pool
	created.ID = 1
	f.poolsByZone = append(f.poolsByZone, created)
	return &created, nil
}

func (f *fakeIPPoolRepository) GetPool(ctx context.Context, id int64) (*model.IPPool, error) {
	if f.poolErr != nil {
		return nil, f.poolErr
	}
	if f.pool == nil {
		return nil, pgx.ErrNoRows
	}
	p := *f.pool
	return &p, nil
}

func (f *fakeIPPoolRepository) ListPools(ctx context.Context) ([]model.IPPool, error) {
	return f.poolsByZone, nil
}

func (f *fakeIPPoolRepository) ListPoolsPage(ctx context.Context, limit, offset int) ([]model.IPPool, error) {
	return slicePage(f.poolsByZone, limit, offset), nil
}

func (f *fakeIPPoolRepository) CountPools(ctx context.Context) (int, error) {
	return len(f.poolsByZone), nil
}

func (f *fakeIPPoolRepository) ListPoolsByZone(ctx context.Context, zoneID int64) ([]model.IPPool, error) {
	return f.poolsByZone, nil
}

func (f *fakeIPPoolRepository) GetPoolNodes(ctx context.Context, poolID int64) ([]model.PVENode, error) {
	return f.nodes, nil
}

func (f *fakeIPPoolRepository) SetPoolNodes(ctx context.Context, poolID int64, nodeIDs []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setNodes = append([]int64(nil), nodeIDs...)
	return nil
}

func (f *fakeIPPoolRepository) AllocateFreeIP(ctx context.Context, poolID int64, vmID *int64) (model.IP, error) {
	f.mu.Lock()
	if len(f.free) == 0 {
		f.mu.Unlock()
		return model.IP{}, pgx.ErrNoRows
	}
	i := rand.Intn(len(f.free))
	cand := f.free[i]
	f.mu.Unlock()

	// 模拟随机 SELECT 与条件 UPDATE 之间的窗口期，期间另一个事务可能抢占
	// 同一地址。
	time.Sleep(200 * time.Microsecond)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed[cand] {
		return model.IP{}, repository.ErrAllocationRetry
	}
	f.claimed[cand] = true
	for j, s := range f.free {
		if s == cand {
			f.free = append(f.free[:j], f.free[j+1:]...)
			break
		}
	}
	return model.IP{ID: 1, PoolID: poolID, IP: cand, Status: model.IPStatusUsed}, nil
}

func (f *fakeIPPoolRepository) ReleaseIPByVM(ctx context.Context, vmID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, vmID)
	return nil
}

// scriptedIPPoolRepository 服务于确定性的重试循环测试：前几次调用返回脚本
// 化的结果，之后返回成功。
type scriptedIPPoolRepository struct {
	pool        *model.IPPool
	poolErr     error
	retryCount  int  // 初始 ErrAllocationRetry 结果的数量
	alwaysRetry bool // 每次调用都以 ErrAllocationRetry 失败
	noRows      bool // 每次调用都以 pgx.ErrNoRows 失败
	calls       int
	successIP   model.IP
}

func (s *scriptedIPPoolRepository) CreatePoolWithIPs(ctx context.Context, pool model.IPPool, ips []model.IP) (*model.IPPool, error) {
	return nil, errors.New("unused")
}

func (s *scriptedIPPoolRepository) GetPool(ctx context.Context, id int64) (*model.IPPool, error) {
	if s.poolErr != nil {
		return nil, s.poolErr
	}
	if s.pool == nil {
		return nil, pgx.ErrNoRows
	}
	p := *s.pool
	return &p, nil
}

func (s *scriptedIPPoolRepository) ListPools(ctx context.Context) ([]model.IPPool, error) {
	return nil, errors.New("unused")
}

func (s *scriptedIPPoolRepository) ListPoolsPage(ctx context.Context, limit, offset int) ([]model.IPPool, error) {
	return nil, errors.New("unused")
}

func (s *scriptedIPPoolRepository) CountPools(ctx context.Context) (int, error) {
	return 0, errors.New("unused")
}

func (s *scriptedIPPoolRepository) ListPoolsByZone(ctx context.Context, zoneID int64) ([]model.IPPool, error) {
	return nil, errors.New("unused")
}

func (s *scriptedIPPoolRepository) GetPoolNodes(ctx context.Context, poolID int64) ([]model.PVENode, error) {
	return nil, errors.New("unused")
}

func (s *scriptedIPPoolRepository) SetPoolNodes(ctx context.Context, poolID int64, nodeIDs []int64) error {
	return errors.New("unused")
}

func (s *scriptedIPPoolRepository) AllocateFreeIP(ctx context.Context, poolID int64, vmID *int64) (model.IP, error) {
	s.calls++
	switch {
	case s.noRows:
		return model.IP{}, pgx.ErrNoRows
	case s.alwaysRetry:
		return model.IP{}, repository.ErrAllocationRetry
	case s.calls <= s.retryCount:
		return model.IP{}, repository.ErrAllocationRetry
	default:
		return s.successIP, nil
	}
}

func (s *scriptedIPPoolRepository) ReleaseIPByVM(ctx context.Context, vmID int64) error {
	return nil
}

func TestAllocateIPRetriesThenSucceeds(t *testing.T) {
	repo := &scriptedIPPoolRepository{
		pool:       &model.IPPool{ID: 1, ZoneID: 1},
		retryCount: 2,
		successIP:  model.IP{ID: 7, PoolID: 1, IP: "10.0.0.2", Status: model.IPStatusUsed},
	}
	svc := NewIPPoolService(repo, nil, nil)

	ip, err := svc.AllocateIP(context.Background(), 1)
	if err != nil {
		t.Fatalf("AllocateIP: %v", err)
	}
	if ip.IP != "10.0.0.2" {
		t.Fatalf("ip = %+v", ip)
	}
	if repo.calls != 3 {
		t.Fatalf("calls = %d, want 3 (2 retries + 1 success)", repo.calls)
	}
}

func TestAllocateIPExhaustedAfterMaxRetries(t *testing.T) {
	repo := &scriptedIPPoolRepository{pool: &model.IPPool{ID: 1, ZoneID: 1}, alwaysRetry: true}
	svc := NewIPPoolService(repo, nil, nil)

	_, err := svc.AllocateIP(context.Background(), 1)
	if !isKind(err, KindIPExhausted) {
		t.Fatalf("err = %v, want KindIPExhausted", err)
	}
	if repo.calls != maxAllocationAttempts {
		t.Fatalf("calls = %d, want %d", repo.calls, maxAllocationAttempts)
	}
}

func TestAllocateIPNoFreeAddress(t *testing.T) {
	repo := &scriptedIPPoolRepository{pool: &model.IPPool{ID: 1, ZoneID: 1}, noRows: true}
	svc := NewIPPoolService(repo, nil, nil)

	_, err := svc.AllocateIP(context.Background(), 1)
	if !isKind(err, KindIPExhausted) {
		t.Fatalf("err = %v, want KindIPExhausted", err)
	}
	if repo.calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on empty pool)", repo.calls)
	}
}

func TestAllocateIPPoolNotFound(t *testing.T) {
	repo := &scriptedIPPoolRepository{poolErr: pgx.ErrNoRows}
	svc := NewIPPoolService(repo, nil, nil)

	_, err := svc.AllocateIP(context.Background(), 404)
	if !isKind(err, KindNotFound) {
		t.Fatalf("err = %v, want KindNotFound", err)
	}
}

func TestAllocateIPInZone(t *testing.T) {
	repo := &scriptedIPPoolRepository{
		pool:      &model.IPPool{ID: 1, ZoneID: 5},
		successIP: model.IP{ID: 7, PoolID: 1, IP: "10.0.0.2", Status: model.IPStatusUsed},
	}
	svc := NewIPPoolService(repo, nil, nil)

	// 错误的区域 -> conflict。
	if _, err := svc.AllocateIPInZone(context.Background(), 3, 1); !isKind(err, KindConflict) {
		t.Fatalf("cross-zone err = %v, want KindConflict", err)
	}
	// 正确的区域 -> 成功分配。
	ip, err := svc.AllocateIPInZone(context.Background(), 5, 1)
	if err != nil {
		t.Fatalf("AllocateIPInZone: %v", err)
	}
	if ip.IP != "10.0.0.2" {
		t.Fatalf("ip = %+v", ip)
	}
}

// TestConcurrentAllocateIP 在模拟了仓库丢失更新窗口的情况下，对含 M 个空闲
// 地址的池执行 N 次并发分配：任何地址都绝不能发放两次，超出池容量的请求
// 必须以 ip_exhausted 返回。
func TestConcurrentAllocateIP(t *testing.T) {
	const (
		poolSize = 3
		requestN = 12
	)
	free := []string{"10.0.0.2", "10.0.0.3", "10.0.0.4"}[:poolSize]
	repo := newFakeIPPoolRepository(free)
	svc := NewIPPoolService(repo, nil, nil)

	var wg sync.WaitGroup
	results := make(chan result, requestN)
	for i := 0; i < requestN; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ip, err := svc.AllocateIP(context.Background(), 1)
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{ip: ip.IP}
		}()
	}
	wg.Wait()
	close(results)

	got := make([]result, 0, requestN)
	for r := range results {
		got = append(got, r)
	}

	allocated := make([]string, 0)
	exhausted := 0
	seen := make(map[string]bool)
	for _, r := range got {
		switch {
		case r.err == nil:
			if seen[r.ip] {
				t.Fatalf("ip %s allocated twice", r.ip)
			}
			seen[r.ip] = true
			allocated = append(allocated, r.ip)
		case isKind(r.err, KindIPExhausted):
			exhausted++
		default:
			t.Fatalf("unexpected error kind: %v", r.err)
		}
	}

	if len(allocated)+exhausted != requestN {
		t.Fatalf("results = %d, want %d", len(allocated)+exhausted, requestN)
	}
	if len(allocated) > poolSize {
		t.Fatalf("allocated %d addresses, pool has only %d", len(allocated), poolSize)
	}
	// 最多存在 poolSize 个地址，因此至少有 requestN-poolSize 个请求必须以
	// 耗尽为由被拒绝。
	if exhausted < requestN-poolSize {
		t.Fatalf("exhausted = %d, want >= %d", exhausted, requestN-poolSize)
	}
	// 每个已分配的地址都必须来自该池。
	poolAddrs := map[string]bool{"10.0.0.2": true, "10.0.0.3": true, "10.0.0.4": true}
	for _, ip := range allocated {
		if !poolAddrs[ip] {
			t.Fatalf("address %s not in pool", ip)
		}
	}
}

type result struct {
	ip  string
	err error
}

func TestReleaseIPByVM(t *testing.T) {
	repo := newFakeIPPoolRepository(nil)
	svc := NewIPPoolService(repo, nil, nil)

	if err := svc.ReleaseIPByVM(context.Background(), 9); err != nil {
		t.Fatalf("ReleaseIPByVM: %v", err)
	}
	if len(repo.released) != 1 || repo.released[0] != 9 {
		t.Fatalf("released = %v, want [9]", repo.released)
	}
}

func TestCreateIPPoolValidation(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{{ID: 1, Name: "cn-east-1"}}}
	svc := NewIPPoolService(newFakeIPPoolRepository(nil), zoneRepo, &fakeNodeRepository{})

	cases := []struct {
		name   string
		zoneID int64
		cidr   string
		gw     string
		kind   ErrorKind
	}{
		{"unknown zone", 99, "10.0.0.0/24", "10.0.0.1", KindNotFound},
		{"empty name", 1, "10.0.0.0/24", "10.0.0.1", KindBadRequest},
		{"invalid cidr", 1, "not-a-cidr", "10.0.0.1", KindBadRequest},
		{"ipv6 unsupported", 1, "fd00::/64", "fd00::1", KindBadRequest},
		{"too large", 1, "10.0.0.0/21", "10.0.0.1", KindBadRequest},
		{"gateway outside", 1, "10.0.0.0/24", "10.1.0.1", KindBadRequest},
		{"invalid gateway", 1, "10.0.0.0/24", "xyz", KindBadRequest},
		{"no usable addresses", 1, "10.0.0.1/32", "10.0.0.1", KindBadRequest},
		{"/31 rejected", 1, "10.0.0.0/31", "10.0.0.1", KindBadRequest},
		{"gateway on network address", 1, "10.0.0.0/30", "10.0.0.0", KindBadRequest},
		{"gateway on broadcast address", 1, "10.0.0.0/30", "10.0.0.3", KindBadRequest},
	}
	for _, tc := range cases {
		name := tc.name
		if name == "empty name" {
			name = " "
		}
		if _, err := svc.CreateIPPool(context.Background(), tc.zoneID, name, tc.cidr, tc.gw, ""); !isKind(err, tc.kind) {
			t.Fatalf("%s: err = %v, want kind %d", tc.name, err, tc.kind)
		}
	}
}

func TestCreateIPPoolSuccessExpandsCIDR(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{{ID: 1, Name: "cn-east-1"}}}
	repo := newFakeIPPoolRepository(nil)
	svc := NewIPPoolService(repo, zoneRepo, &fakeNodeRepository{})

	pool, err := svc.CreateIPPool(context.Background(), 1, "default", "10.0.0.0/30", "10.0.0.1", "1.1.1.1")
	if err != nil {
		t.Fatalf("CreateIPPool: %v", err)
	}
	if pool.NetworkCIDR != "10.0.0.0/30" || pool.Gateway != "10.0.0.1" || pool.ZoneID != 1 {
		t.Fatalf("unexpected pool: %+v", pool)
	}
}

func TestCreateIPPoolDuplicateName(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{{ID: 1, Name: "cn-east-1"}}}
	repo := newFakeIPPoolRepository(nil)
	svc := NewIPPoolService(repo, zoneRepo, &fakeNodeRepository{})

	if _, err := svc.CreateIPPool(context.Background(), 1, "default", "10.0.0.0/30", "10.0.0.1", ""); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := svc.CreateIPPool(context.Background(), 1, "default", "10.0.0.4/30", "10.0.0.5", ""); !isKind(err, KindConflict) {
		t.Fatalf("duplicate name err = %v, want KindConflict", err)
	}
}

func TestSetPoolNodesValidation(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{{ID: 1, Name: "cn-east-1"}}}
	nodeRepo := &fakeNodeRepository{nodes: []model.PVENode{
		{ID: 10, ZoneID: 1, Name: "pve1", Host: "h1"},
		{ID: 20, ZoneID: 2, Name: "other-zone", Host: "h2"},
	}}

	// 未知池 -> not_found。
	missing := newFakeIPPoolRepository(nil)
	missing.poolErr = pgx.ErrNoRows
	svc := NewIPPoolService(missing, zoneRepo, nodeRepo)
	if err := svc.SetPoolNodes(context.Background(), 404, []int64{10}); !isKind(err, KindNotFound) {
		t.Fatalf("unknown pool err = %v, want KindNotFound", err)
	}

	svc = NewIPPoolService(newFakeIPPoolRepository(nil), zoneRepo, nodeRepo)
	// 未知节点 -> not_found。
	if err := svc.SetPoolNodes(context.Background(), 1, []int64{99}); !isKind(err, KindNotFound) {
		t.Fatalf("unknown node err = %v, want KindNotFound", err)
	}
	// 跨区域节点 -> conflict。
	if err := svc.SetPoolNodes(context.Background(), 1, []int64{20}); !isKind(err, KindConflict) {
		t.Fatalf("cross-zone err = %v, want KindConflict", err)
	}
	// 重复的 id 会被去重。
	if err := svc.SetPoolNodes(context.Background(), 1, []int64{10, 10, 10}); err != nil {
		t.Fatalf("set pool nodes: %v", err)
	}
}

func TestExpandPoolIPs(t *testing.T) {
	cases := []struct {
		cidr    string
		gateway string
		want    []string
	}{
		{"10.0.0.0/30", "10.0.0.1", []string{"10.0.0.2"}},
		{"10.0.0.0/29", "10.0.0.1", []string{"10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6"}},
		// 网关位于网络地址上：只丢弃网络地址本身，10.0.0.1 仍然可用
		{"10.0.0.0/30", "10.0.0.0", []string{"10.0.0.1", "10.0.0.2"}},
		// 网关位于广播地址上：只丢弃广播地址，10.0.0.1 仍然可用
		{"10.0.0.0/30", "10.0.0.3", []string{"10.0.0.1", "10.0.0.2"}},
		// /24 保留完整范围减去网络/广播/网关地址
		{"10.0.0.0/24", "10.0.0.1", nil}, // 长度在下方断言
	}
	for _, tc := range cases {
		prefix := mustParsePrefix(t, tc.cidr)
		gw := mustParseAddr(t, tc.gateway)
		ips := expandPoolIPs(prefix, gw)
		if tc.cidr == "10.0.0.0/24" {
			if len(ips) != 253 {
				t.Fatalf("10.0.0.0/24: got %d ips, want 253", len(ips))
			}
			if ips[0].IP != "10.0.0.2" || ips[len(ips)-1].IP != "10.0.0.254" {
				t.Fatalf("10.0.0.0/24: range = [%s, %s]", ips[0].IP, ips[len(ips)-1].IP)
			}
			continue
		}
		got := make([]string, 0, len(ips))
		for _, ip := range ips {
			if ip.Status != model.IPStatusFree {
				t.Fatalf("%s: status = %q, want free", ip.IP, ip.Status)
			}
			got = append(got, ip.IP)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.cidr, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %v, want %v", tc.cidr, got, tc.want)
			}
		}
	}
}

func mustParsePrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse prefix %q: %v", s, err)
	}
	return p
}

func mustParseAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse addr %q: %v", s, err)
	}
	return a
}

func isKind(err error, kind ErrorKind) bool {
	var serr *Error
	return errors.As(err, &serr) && serr.Kind == kind
}
