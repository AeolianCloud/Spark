package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"spark/config"
	"spark/crypto"
	"spark/model"
	"spark/pve"
	"spark/repository"
)

var vmTestTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// ---------- 测试替身 ----------

// fakeTx 以最小方式实现 pgx.Tx：服务测试只直接使用 Commit/Rollback；仓库
// 调用把它当作不透明句柄并忽略它。
type fakeTx struct {
	committed  bool
	rolledBack bool
}

func (f *fakeTx) Begin(ctx context.Context) (pgx.Tx, error) { return nil, errors.New("unused") }
func (f *fakeTx) Commit(ctx context.Context) error          { f.committed = true; return nil }
func (f *fakeTx) Rollback(ctx context.Context) error        { f.rolledBack = true; return nil }
func (f *fakeTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (f *fakeTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults { return nil }
func (f *fakeTx) LargeObjects() pgx.LargeObjects                               { return pgx.LargeObjects{} }
func (f *fakeTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (f *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}
func (f *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row { return nil }
func (f *fakeTx) Conn() *pgx.Conn                                               { return nil }

type fakeBeginner struct {
	tx *fakeTx
}

func (f *fakeBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	if f.tx == nil {
		f.tx = &fakeTx{}
	}
	return f.tx, nil
}

// fakeVMRepository 是供服务测试使用的可脚本化 VMRepository。它记录每次
// 变更的参数，并提供可配置的 GetVM。
//
// mu 保护分离式供给 goroutine（UpdateVMPVEVMID、SetProvisionError）写入的
// 字段，防止与测试 goroutine 的读取产生竞争，使 -race 保持干净。
type fakeVMRepository struct {
	mu               sync.Mutex
	created          *model.VM
	createErr        error
	get              *repository.VMWithIP
	getErr           error
	vms              []repository.VMWithIP
	listErr          error
	linkedIPID       int64
	linkedVMID       int64
	updateVmidDiskGB int64
	updateVmidErr    error
	provisionErrors  []string
	setProvisionErr  error
	updatedSpecs     []specUpdate
	updateSpecErr    error
	deleted          []int64
	deleteErr        error
}

// specUpdate 记录一次 UpdateSpec 调用：乐观锁基线（服务读取的旧值）和
// 持久化的新值。
type specUpdate struct {
	old spec
	new spec
}

type spec struct {
	cpu    int
	memMB  int64
	diskGB int64
}

// vmid 返回供给 goroutine 最后关联的 VMID，可安全地并发轮询。
func (f *fakeVMRepository) vmid() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.linkedVMID
}

func (f *fakeVMRepository) CreateVMTx(ctx context.Context, tx pgx.Tx, vm model.VM) (*model.VM, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	created := vm
	created.ID = 1
	created.CreatedAt = vmTestTime
	created.UpdatedAt = vmTestTime
	f.created = &created
	return &created, nil
}

func (f *fakeVMRepository) GetVM(ctx context.Context, id int64) (*repository.VMWithIP, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.get == nil {
		return nil, pgx.ErrNoRows
	}
	return f.get, nil
}

func (f *fakeVMRepository) ListVMs(ctx context.Context) ([]repository.VMWithIP, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.vms, nil
}

func (f *fakeVMRepository) ListVMsPage(ctx context.Context, limit, offset int) ([]repository.VMWithIP, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return slicePage(f.vms, limit, offset), nil
}

func (f *fakeVMRepository) CountVMs(ctx context.Context) (int, error) {
	if f.listErr != nil {
		return 0, f.listErr
	}
	return len(f.vms), nil
}

func (f *fakeVMRepository) SetVMIPIDTx(ctx context.Context, tx pgx.Tx, id, ipID int64) error {
	f.linkedIPID = ipID
	return nil
}

func (f *fakeVMRepository) UpdateVMPVEVMID(ctx context.Context, id, vmid, diskGB int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateVmidErr != nil {
		return f.updateVmidErr
	}
	f.linkedVMID = vmid
	f.updateVmidDiskGB = diskGB
	return nil
}

func (f *fakeVMRepository) SetProvisionError(ctx context.Context, id int64, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setProvisionErr != nil {
		return f.setProvisionErr
	}
	f.provisionErrors = append(f.provisionErrors, message)
	return nil
}

func (f *fakeVMRepository) UpdateSpec(ctx context.Context, id int64, newCPU int, newMemMB, newDiskGB int64, oldCPU int, oldMemMB, oldDiskGB int64) error {
	if f.updateSpecErr != nil {
		return f.updateSpecErr
	}
	f.updatedSpecs = append(f.updatedSpecs, specUpdate{
		old: spec{cpu: oldCPU, memMB: oldMemMB, diskGB: oldDiskGB},
		new: spec{cpu: newCPU, memMB: newMemMB, diskGB: newDiskGB},
	})
	if f.get != nil {
		f.get.VM.CPU = newCPU
		f.get.VM.MemMB = newMemMB
		f.get.VM.DiskGB = newDiskGB
	}
	return nil
}

func (f *fakeVMRepository) DeleteVMTx(ctx context.Context, tx pgx.Tx, id int64) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

// fakeVMIPPoolRepository 是供服务测试使用的可脚本化 VMIPPoolRepository：
// 脚本化的池列表/节点以及脚本化的抢占结果。
type fakeVMIPPoolRepository struct {
	pools        []model.IPPool
	poolNodes    map[int64][]model.PVENode
	claimResults []claimResult // 按顺序消费
	claimDefault error         // 脚本耗尽时返回
	claimedVMIDs []int64
	released     []int64
}

type claimResult struct {
	ip  model.IP
	err error
}

func (f *fakeVMIPPoolRepository) ListPoolsByZone(ctx context.Context, zoneID int64) ([]model.IPPool, error) {
	return f.pools, nil
}

func (f *fakeVMIPPoolRepository) GetPoolNodes(ctx context.Context, poolID int64) ([]model.PVENode, error) {
	return f.poolNodes[poolID], nil
}

func (f *fakeVMIPPoolRepository) ClaimFreeIP(ctx context.Context, tx pgx.Tx, poolID int64, vmID *int64) (model.IP, error) {
	if vmID != nil {
		f.claimedVMIDs = append(f.claimedVMIDs, *vmID)
	}
	if len(f.claimResults) > 0 {
		r := f.claimResults[0]
		f.claimResults = f.claimResults[1:]
		return r.ip, r.err
	}
	if f.claimDefault != nil {
		return model.IP{}, f.claimDefault
	}
	return model.IP{}, pgx.ErrNoRows
}

func (f *fakeVMIPPoolRepository) ReleaseIPByVMTx(ctx context.Context, tx pgx.Tx, vmID int64) error {
	f.released = append(f.released, vmID)
	return nil
}

// fakeVMZoneRepository 是供服务测试使用的可脚本化 VMZoneRepository。
type fakeVMZoneRepository struct {
	zones []model.Zone
	err   error
}

func (f *fakeVMZoneRepository) GetZone(ctx context.Context, id int64) (*model.Zone, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.zones {
		if f.zones[i].ID == id {
			z := f.zones[i]
			return &z, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeVMZoneRepository) ListZones(ctx context.Context) ([]model.Zone, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]model.Zone, 0, len(f.zones))
	for i := range f.zones {
		out = append(out, f.zones[i])
	}
	return out, nil
}

// fakeVMNodeRepository 是供服务测试使用的可脚本化 VMNodeRepository。
type fakeVMNodeRepository struct {
	nodes []model.PVENode
	err   error
}

func (f *fakeVMNodeRepository) GetNode(ctx context.Context, id int64) (*model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.nodes {
		if f.nodes[i].ID == id {
			n := f.nodes[i]
			return &n, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeVMNodeRepository) ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]model.PVENode, 0)
	for _, n := range f.nodes {
		if n.ZoneID == zoneID && n.Enabled {
			out = append(out, n)
		}
	}
	return out, nil
}

// fakeVMImageRepository 是供服务测试使用的可脚本化 VMImageRepository。
type fakeVMImageRepository struct {
	images    []model.Image
	nodeNames []string
	err       error
}

func (f *fakeVMImageRepository) Get(ctx context.Context, id int64) (*model.Image, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.images {
		if f.images[i].ID == id {
			img := f.images[i]
			return &img, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeVMImageRepository) EnabledNodeNamesByZone(ctx context.Context, zoneID int64) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.nodeNames, nil
}

// fakeVMStorageTypeRepository 是供服务测试使用的可脚本化
// VMStorageTypeRepository。
type fakeVMStorageTypeRepository struct {
	types []model.StorageType
}

func (f *fakeVMStorageTypeRepository) Get(ctx context.Context, id int64) (*model.StorageType, error) {
	for i := range f.types {
		if f.types[i].ID == id {
			st := f.types[i]
			return &st, nil
		}
	}
	return nil, pgx.ErrNoRows
}

// ---------- 辅助函数 ----------

// testCipher 使用确定性的 32 字节密钥构建密码器。
func testCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	cfg := config.Default()
	cfg.Crypto.EncryptionKey = base64.StdEncoding.EncodeToString(key)
	c, err := crypto.NewCipher(cfg)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

// newVMService 用测试替身和假事务开启器装配一个 VMService；节点选择器被
// 桩化为返回第一个候选。默认 PVE 客户端将分离式供给链指向完整的成功链服务器，
// 因此会启动供给链的测试能安静且确定性地完成；真正测试供给链本身的测试会
// 覆盖 newClient。
func newVMService(t *testing.T, vmRepo VMRepository, ipRepo VMIPPoolRepository,
	zoneRepo VMZoneRepository, nodeRepo VMNodeRepository, imageRepo VMImageRepository,
	stRepo VMStorageTypeRepository) *VMService {
	t.Helper()
	svc := NewVMService(&fakeBeginner{}, vmRepo, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo, testCipher(t))
	svc.selectNode = firstReachableCandidate
	srv := newScriptedProvisionServer(t, "15G")
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	return svc
}

// waitForProvision 阻塞直到分离式供给 goroutine 完成（fake 仓库的
// UpdateVMPVEVMID 已被调用）或短暂超时，使测试不与 goroutine 竞争，且其
// 服务器保持打开。
func waitForProvision(t *testing.T, repo *fakeVMRepository) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if repo.vmid() != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("provisioning goroutine did not finish in time")
}

// firstReachableCandidate 是假节点选择器：返回第一个候选，空列表时返回
// node_unavailable 错误。
func firstReachableCandidate(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
	if len(nodes) == 0 {
		return model.PVENode{}, nodeUnavailablef("no candidates")
	}
	return nodes[0], nil
}

// scriptedSelectNode 构建一个假节点选择器，返回第一个名称不在 dead 中的
// 候选。
func scriptedSelectNode(dead map[string]bool) func(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
	return func(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
		for _, n := range nodes {
			if !dead[n.Name] {
				return n, nil
			}
		}
		return model.PVENode{}, nodeUnavailablef("no reachable candidate")
	}
}

// testPVENode 是带有效 PVE token 凭据的节点（拆分形式 "root@pam" +
// "spark=uuid" -> PVEAPIToken=root@pam!spark=uuid）。
func testPVENode(id int64) model.PVENode {
	return model.PVENode{ID: id, ZoneID: 1, Name: "pve1", Host: "h",
		APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true}
}

// createEnv 为快乐路径创建测试构建完全有效的假环境：区域 1 带一个启用节点、
// 一个将该节点加入白名单的池、一个存在于该节点上的镜像以及一个存储类型。
func createEnv() (*fakeVMZoneRepository, *fakeVMImageRepository, *fakeVMStorageTypeRepository, *fakeVMNodeRepository, *fakeVMIPPoolRepository) {
	node := testPVENode(1)
	zoneRepo := &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	imageRepo := &fakeVMImageRepository{
		images: []model.Image{{ID: 1, Name: "debian-12-cloud", DefaultUser: "debian",
			NodeImages: map[string]string{"pve1": "/templates/debian-12-cloud.qcow2"}}},
		nodeNames: []string{"pve1"},
	}
	stRepo := &fakeVMStorageTypeRepository{types: []model.StorageType{{ID: 1, Name: "ssd", PVEStorage: "local-ssd"}}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{node}}
	ipRepo := &fakeVMIPPoolRepository{
		pools:     []model.IPPool{{ID: 1, ZoneID: 1, Name: "p1", NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1", DNS: "1.1.1.1"}},
		poolNodes: map[int64][]model.PVENode{1: {node}},
	}
	return zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo
}

func validCreateRequest() CreateVMRequest {
	return CreateVMRequest{Name: "vm1", CPU: 2, MemMB: 2048, DiskGB: 10, ImageID: 1, StorageTypeID: 1, ZoneID: 1, Password: "s3cret"}
}

// ---------- 校验测试 ----------

func TestValidateCreateVMRequest(t *testing.T) {
	req := validCreateRequest()
	if err := validateCreateVMRequest(req); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*CreateVMRequest)
	}{
		{"empty name", func(r *CreateVMRequest) { r.Name = " " }},
		{"name with leading dash", func(r *CreateVMRequest) { r.Name = "-vm1" }},
		{"name with leading dot", func(r *CreateVMRequest) { r.Name = ".vm1" }},
		{"name with space", func(r *CreateVMRequest) { r.Name = "vm 1" }},
		{"name with slash", func(r *CreateVMRequest) { r.Name = "vm/1" }},
		{"name with colon", func(r *CreateVMRequest) { r.Name = "vm:1" }},
		{"empty password", func(r *CreateVMRequest) { r.Password = "" }},
		{"zero cpu", func(r *CreateVMRequest) { r.CPU = 0 }},
		{"zero mem", func(r *CreateVMRequest) { r.MemMB = 0 }},
		{"zero disk", func(r *CreateVMRequest) { r.DiskGB = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := req
			tc.mutate(&r)
			if err := validateCreateVMRequest(r); !isKind(err, KindBadRequest) {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	}
	// PVE 合法的名称变体（首字符之后允许短横线/点/下划线）。
	for _, name := range []string{"vm-1", "vm_1", "vm.1", "v1.2-3_x"} {
		r := req
		r.Name = name
		if err := validateCreateVMRequest(r); err != nil {
			t.Fatalf("name %q rejected: %v", name, err)
		}
	}
}

func TestValidateResizeSpec(t *testing.T) {
	current := model.VM{CPU: 2, MemMB: 2048, DiskGB: 10}
	cpu := 4
	mem := int64(4096)
	disk := int64(20)
	smallDisk := int64(5)

	// 无字段 -> bad request。
	if err := validateResizeSpec(nil, nil, nil, current); !isKind(err, KindBadRequest) {
		t.Fatalf("no fields err = %v, want KindBadRequest", err)
	}
	// 磁盘缩小 -> KindDiskShrinkNotAllowed。
	if err := validateResizeSpec(nil, nil, &smallDisk, current); !isKind(err, KindDiskShrinkNotAllowed) {
		t.Fatalf("shrink err = %v, want KindDiskShrinkNotAllowed", err)
	}
	// 磁盘相等 -> 允许（文档化的无操作）。
	if err := validateResizeSpec(nil, nil, &current.DiskGB, current); err != nil {
		t.Fatalf("equal disk err = %v, want nil (no-op)", err)
	}
	// 全部增大 -> 允许。
	if err := validateResizeSpec(&cpu, &mem, &disk, current); err != nil {
		t.Fatalf("grow err = %v, want nil", err)
	}
	// 非正值 -> bad request。
	zeroMem := int64(0)
	zeroCPU := 0
	if err := validateResizeSpec(nil, &zeroMem, nil, current); !isKind(err, KindBadRequest) {
		t.Fatalf("zero mem err = %v, want KindBadRequest", err)
	}
	if err := validateResizeSpec(&zeroCPU, nil, nil, current); !isKind(err, KindBadRequest) {
		t.Fatalf("zero cpu err = %v, want KindBadRequest", err)
	}
}

func TestPoolCandidates(t *testing.T) {
	poolNodes := []model.PVENode{
		{ID: 3, Name: "n3"},
		{ID: 1, Name: "n1"},
		{ID: 2, Name: "n2"},
	}
	enabled := []model.PVENode{
		{ID: 1, Name: "n1"},
		{ID: 2, Name: "n2"},
	}
	got := poolCandidates(poolNodes, enabled)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("candidates = %+v, want [n1 n2] in node id order", got)
	}
	if got := poolCandidates(poolNodes, nil); len(got) != 0 {
		t.Fatalf("no enabled nodes: candidates = %+v, want empty", got)
	}
}

// ---------- 创建流程测试 ----------

func TestCreateVMHappyPath(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	vmRepo := &fakeVMRepository{}
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 7, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}}}
	svc := newVMService(t, vmRepo, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	vm, err := svc.CreateVM(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if vm.VM.ID != 1 || vm.VM.PVEVmid != 0 || vm.VM.Name != "vm1" {
		t.Fatalf("vm = %+v", vm.VM)
	}
	if vm.IP != "10.0.0.5" {
		t.Fatalf("ip = %q, want 10.0.0.5", vm.IP)
	}
	if vm.VM.IPID == nil || *vm.VM.IPID != 7 {
		t.Fatalf("ip_id = %v, want 7", vm.VM.IPID)
	}
	// 事务顺序：抢占先关联已创建的 vm id，随后写入 ip_id 关联。
	if len(ipRepo.claimedVMIDs) != 1 || ipRepo.claimedVMIDs[0] != 1 {
		t.Fatalf("claimed vm ids = %v, want [1]", ipRepo.claimedVMIDs)
	}
	if vmRepo.linkedIPID != 7 {
		t.Fatalf("linked ip id = %d, want 7", vmRepo.linkedIPID)
	}
	waitForProvision(t, vmRepo)
}

// TestCreateVMEncryptsPassword 固定密码在交给仓库之前已被加密：存储值与
// 明文不同，且能解密回明文。
func TestCreateVMEncryptsPassword(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	vmRepo := &fakeVMRepository{}
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 7, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}}}
	svc := newVMService(t, vmRepo, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	const pw = "s3cret-pw-123"
	req := validCreateRequest()
	req.Password = pw
	if _, err := svc.CreateVM(context.Background(), req); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	enc := vmRepo.created.PasswordEncrypted
	if enc == "" || enc == pw {
		t.Fatalf("stored password = %q, want an encrypted value", enc)
	}
	plain, err := testCipher(t).Decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt stored password: %v", err)
	}
	if plain != pw {
		t.Fatalf("decrypted = %q, want %q", plain, pw)
	}
	waitForProvision(t, vmRepo)
}

func TestCreateVMValidationOrder(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	svc := newVMService(t, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	req := validCreateRequest()
	// 未知区域 -> not_found。
	r := req
	r.ZoneID = 99
	if _, err := svc.CreateVM(context.Background(), r); !isKind(err, KindNotFound) {
		t.Fatalf("unknown zone err = %v, want KindNotFound", err)
	}
	// 未知镜像 -> not_found。
	r = req
	r.ImageID = 99
	if _, err := svc.CreateVM(context.Background(), r); !isKind(err, KindNotFound) {
		t.Fatalf("unknown image err = %v, want KindNotFound", err)
	}
	// 未知存储类型 -> not_found。
	r = req
	r.StorageTypeID = 99
	if _, err := svc.CreateVM(context.Background(), r); !isKind(err, KindNotFound) {
		t.Fatalf("unknown storage err = %v, want KindNotFound", err)
	}
	// 镜像未出现在所有启用节点上 -> KindImageNotAvailable。
	imgRepo := &fakeVMImageRepository{
		images: []model.Image{{ID: 1, Name: "debian-12-cloud", DefaultUser: "debian",
			NodeImages: map[string]string{"pve1": "/t.img"}}},
		nodeNames: []string{"pve1", "pve2"},
	}
	svc2 := newVMService(t, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo, imgRepo, stRepo)
	if _, err := svc2.CreateVM(context.Background(), req); !isKind(err, KindImageNotAvailable) {
		t.Fatalf("image not available err = %v, want KindImageNotAvailable", err)
	}
	// 空密码 -> bad_request。
	r = req
	r.Password = ""
	if _, err := svc.CreateVM(context.Background(), r); !isKind(err, KindBadRequest) {
		t.Fatalf("empty password err = %v, want KindBadRequest", err)
	}
	// 零 cpu -> bad_request。
	r = req
	r.CPU = 0
	if _, err := svc.CreateVM(context.Background(), r); !isKind(err, KindBadRequest) {
		t.Fatalf("zero cpu err = %v, want KindBadRequest", err)
	}
}

func TestCreateVMClaimRetriesThenSucceeds(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	vmRepo := &fakeVMRepository{}
	ipRepo.claimResults = []claimResult{
		{err: repository.ErrAllocationRetry},
		{err: repository.ErrAllocationRetry},
		{ip: model.IP{ID: 7, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}},
	}
	svc := newVMService(t, vmRepo, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	vm, err := svc.CreateVM(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if vm.IP != "10.0.0.5" {
		t.Fatalf("ip = %q", vm.IP)
	}
	if len(ipRepo.claimResults) != 0 {
		t.Fatalf("claim script not fully consumed: %+v", ipRepo.claimResults)
	}
	waitForProvision(t, vmRepo)
}

func TestCreateVMClaimExhausted(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	ipRepo.claimResults = make([]claimResult, vmClaimRetries)
	for i := range ipRepo.claimResults {
		ipRepo.claimResults[i] = claimResult{err: repository.ErrAllocationRetry}
	}
	svc := newVMService(t, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	_, err := svc.CreateVM(context.Background(), validCreateRequest())
	if !isKind(err, KindIPExhausted) {
		t.Fatalf("err = %v, want KindIPExhausted", err)
	}
}

func TestCreateVMClaimNoFreeAddress(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	ipRepo.claimDefault = pgx.ErrNoRows
	svc := newVMService(t, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	_, err := svc.CreateVM(context.Background(), validCreateRequest())
	if !isKind(err, KindIPExhausted) {
		t.Fatalf("err = %v, want KindIPExhausted", err)
	}
}

// TestSelectPoolAndNodeSkipsUnreachablePools 验证 D4：不可达的池会被跳过
// 转而使用下一个，没有可达池的区域会产生 node_unavailable。
func TestSelectPoolAndNodeSkipsUnreachablePools(t *testing.T) {
	deadNode := model.PVENode{ID: 1, ZoneID: 1, Name: "pve-dead", Host: "h1", Enabled: true}
	aliveNode := model.PVENode{ID: 2, ZoneID: 1, Name: "pve-alive", Host: "h2", Enabled: true}
	zoneRepo := &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{deadNode, aliveNode}}
	ipRepo := &fakeVMIPPoolRepository{
		pools: []model.IPPool{
			{ID: 1, ZoneID: 1, Name: "dead-pool", NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
			{ID: 2, ZoneID: 1, Name: "alive-pool", NetworkCIDR: "10.0.1.0/24", Gateway: "10.0.1.1"},
		},
		poolNodes: map[int64][]model.PVENode{1: {deadNode}, 2: {aliveNode}},
	}
	svc := NewVMService(&fakeBeginner{}, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{}, testCipher(t))

	// 池 1 的唯一节点是死的，池 2 的可达 -> 池 2 胜出。
	svc.selectNode = scriptedSelectNode(map[string]bool{"pve-dead": true})
	pool, node, err := svc.selectPoolAndNode(context.Background(), 1)
	if err != nil {
		t.Fatalf("selectPoolAndNode: %v", err)
	}
	if pool.ID != 2 || node.Name != "pve-alive" {
		t.Fatalf("pool = %+v node = %+v, want pool 2 / pve-alive", pool, node)
	}

	// 两个池的节点都是死的 -> node_unavailable。
	svc.selectNode = scriptedSelectNode(map[string]bool{"pve-dead": true, "pve-alive": true})
	_, _, err = svc.selectPoolAndNode(context.Background(), 1)
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}

	// 完全没有池 -> node_unavailable（候选集为空）。
	ipRepo.pools = nil
	_, _, err = svc.selectPoolAndNode(context.Background(), 1)
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("no pools err = %v, want KindNodeUnavailable", err)
	}
}

// ---------- 供给链测试 ----------

// scriptedProvisionServer 应答整条成功的供给链（nextid、create、任务状态、
// config），并记录 CreateVM 被要求执行的内容。requestDisk/configSize 控制
// resize 行为。
type scriptedProvisionServer struct {
	t          *testing.T
	createBody map[string]any
	configSize string // 返回的 scsi0 配置中的 size= 值
	creates    int
	resizes    int
	destroyed  bool
	requests   map[string]int
}

func newScriptedProvisionServer(t *testing.T, configSize string) *scriptedProvisionServer {
	s := &scriptedProvisionServer{t: t, configSize: configSize, requests: map[string]int{}}
	return s
}

func (s *scriptedProvisionServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/cluster/nextid":
			fmt.Fprint(w, `{"data": "100"}`)
		case r.URL.Path == "/nodes/pve1/qemu" && r.Method == http.MethodPost:
			s.creates++
			s.createBody = map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&s.createBody); err != nil {
				s.t.Errorf("decode create body: %v", err)
			}
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmcreate:100:root@pam:"}`)
		case strings.HasPrefix(r.URL.Path, "/nodes/pve1/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprint(w, `{"data": {"upid": "UPID:pve1:0:0:0:qmcreate:100:root@pam:", "node": "pve1", "type": "qmcreate", "id": "100", "user": "root@pam", "status": "stopped", "exitstatus": "OK"}}`)
		case r.URL.Path == "/nodes/pve1/qemu/100/config" && r.Method == http.MethodGet:
			fmt.Fprintf(w, `{"data": {"bootdisk": "scsi0", "scsi0": "local-ssd:vm-100-disk-0,size=%s", "cores": "2", "memory": "2048"}}`, s.configSize)
		case r.URL.Path == "/nodes/pve1/qemu/100/resize":
			s.resizes++
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:resize:100:root@pam:"}`)
		case r.URL.Path == "/nodes/pve1/qemu/100" && r.Method == http.MethodDelete:
			s.destroyed = true
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmdestroy:100:root@pam:"}`)
		default:
			s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

// TestProvisionSuccessChain 验证完整的单步链：create 在单次调用中携带
// import-from 磁盘、cloud-init 磁盘、vmbr0 网络和 cloud-init 注入，pve_vmid
// 及最终磁盘大小落入数据库。镜像大于请求，因此不进行 resize，持久化实际大小。
func TestProvisionSuccessChain(t *testing.T) {
	srv := newScriptedProvisionServer(t, "15G")
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	const pw = "pw-injected"
	err := svc.provision(context.Background(),
		model.VM{ID: 1, Name: "vm1", MemMB: 2048, CPU: 2, DiskGB: 10},
		model.PVENode{Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
		&model.Image{Name: "debian-12-cloud", DefaultUser: "debian", NodeImages: map[string]string{"pve1": "/templates/d.qcow2"}},
		&model.StorageType{PVEStorage: "local-ssd"},
		model.IPPool{NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1", DNS: "1.1.1.1"},
		pw, "10.0.0.5")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// 单步创建请求体（D5）。
	body := srv.createBody
	if body["vmid"] != float64(100) || body["name"] != "vm1" ||
		body["memory"] != float64(2048) || body["cores"] != float64(2) ||
		body["scsi0"] != "local-ssd:0,import-from=/templates/d.qcow2" ||
		body["ide2"] != "local-ssd:cloudinit" ||
		body["net0"] != "virtio,bridge=vmbr0" ||
		body["bootdisk"] != "scsi0" || body["scsihw"] != "virtio-scsi-pci" ||
		body["ciuser"] != "debian" || body["cipassword"] != pw ||
		body["ipconfig0"] != "ip=10.0.0.5/24,gw=10.0.0.1" ||
		body["nameserver"] != "1.1.1.1" {
		t.Fatalf("create body = %v", body)
	}
	if srv.resizes != 0 {
		t.Fatalf("resizes = %d, want 0 (image already >= request)", srv.resizes)
	}
	// 元数据：记录 vmid，磁盘同步为实际镜像大小 15G。
	if vmRepo.linkedVMID != 100 {
		t.Fatalf("vmid = %d, want 100", vmRepo.linkedVMID)
	}
	if vmRepo.updateVmidDiskGB != 15 {
		t.Fatalf("disk_gb = %d, want 15 (actual image size)", vmRepo.updateVmidDiskGB)
	}
}

// TestProvisionSuccessChainWithResize 覆盖扩展路径：导入的镜像小于请求，
// 因此会运行 resize 任务并持久化请求的大小。
func TestProvisionSuccessChainWithResize(t *testing.T) {
	srv := newScriptedProvisionServer(t, "5G")
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	err := svc.provision(context.Background(),
		model.VM{ID: 1, Name: "vm1", MemMB: 2048, CPU: 2, DiskGB: 20},
		model.PVENode{Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
		&model.Image{Name: "img", DefaultUser: "debian", NodeImages: map[string]string{"pve1": "/t.img"}},
		&model.StorageType{PVEStorage: "local-ssd"},
		model.IPPool{NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
		"pw", "10.0.0.5")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if srv.resizes != 1 {
		t.Fatalf("resizes = %d, want 1", srv.resizes)
	}
	if vmRepo.updateVmidDiskGB != 20 {
		t.Fatalf("disk_gb = %d, want 20 (requested)", vmRepo.updateVmidDiskGB)
	}
}

// TestProvisionFailureSetsSanitizedError 驱动分离式链的失败分支：PVE 错误
// 消息包含明文密码，持久化的 provision_error 绝不能泄露它。
func TestProvisionFailureSetsSanitizedError(t *testing.T) {
	const pw = "s3cretpw-leak-check"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"errors": {"cipassword": "password %s rejected by policy"}}`, pw)
	}))
	defer srv.Close()

	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(srv.URL), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second))
	}

	err := svc.provision(context.Background(),
		model.VM{ID: 1, Name: "vm1", MemMB: 2048, CPU: 2, DiskGB: 10},
		model.PVENode{Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
		&model.Image{Name: "img", DefaultUser: "debian", NodeImages: map[string]string{"pve1": "/t.img"}},
		&model.StorageType{PVEStorage: "local-ssd"},
		model.IPPool{NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
		pw, "10.0.0.5")
	if err == nil {
		t.Fatal("provision succeeded, want failure")
	}
	if len(vmRepo.provisionErrors) != 1 {
		t.Fatalf("provision errors = %d, want 1", len(vmRepo.provisionErrors))
	}
	msg := vmRepo.provisionErrors[0]
	if strings.Contains(msg, pw) {
		t.Fatalf("provision_error leaks the password: %q", msg)
	}
	if !strings.Contains(msg, "rejected by policy") {
		t.Fatalf("provision_error = %q, want the PVE message", msg)
	}
}

// TestProvisionMissingImagePath 记录镜像在所选中节点上没有路径时的供给失败。
func TestProvisionMissingImagePath(t *testing.T) {
	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})

	err := svc.provision(context.Background(),
		model.VM{ID: 1, Name: "vm1", MemMB: 2048, CPU: 2, DiskGB: 10},
		model.PVENode{Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
		&model.Image{Name: "img", DefaultUser: "debian", NodeImages: map[string]string{"other": "/t.img"}},
		&model.StorageType{PVEStorage: "local-ssd"},
		model.IPPool{NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
		"pw", "10.0.0.5")
	if err == nil {
		t.Fatal("provision succeeded, want failure")
	}
	if len(vmRepo.provisionErrors) != 1 {
		t.Fatalf("provision errors = %d, want 1", len(vmRepo.provisionErrors))
	}
}

// TestProvisionFailureAfterCreateIncludesVMID 覆盖半创建状态：create 任务在
// PVE 上成功（vmid=100）但等待它失败，因此持久化的 provision_error 必须携带
// vmid，以便运维定位并清理孤儿 VM。
func TestProvisionFailureAfterCreateIncludesVMID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/cluster/nextid":
			fmt.Fprint(w, `{"data": "100"}`)
		case r.URL.Path == "/nodes/pve1/qemu" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmcreate:100:root@pam:"}`)
		case strings.HasPrefix(r.URL.Path, "/nodes/pve1/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"errors": {"_": "task died"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(srv.URL), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second))
	}

	err := svc.provision(context.Background(),
		model.VM{ID: 1, Name: "vm1", MemMB: 2048, CPU: 2, DiskGB: 10},
		model.PVENode{Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
		&model.Image{Name: "img", DefaultUser: "debian", NodeImages: map[string]string{"pve1": "/t.img"}},
		&model.StorageType{PVEStorage: "local-ssd"},
		model.IPPool{NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
		"pw", "10.0.0.5")
	if err == nil {
		t.Fatal("provision succeeded, want failure")
	}
	if len(vmRepo.provisionErrors) != 1 {
		t.Fatalf("provision errors = %d, want 1", len(vmRepo.provisionErrors))
	}
	msg := vmRepo.provisionErrors[0]
	if !strings.Contains(msg, "vmid=100") || !strings.Contains(msg, "create succeeded") {
		t.Fatalf("provision_error = %q, want it to carry vmid=100 and the create-succeeded marker", msg)
	}
}

// TestSanitizeProvisionError 固定脱敏与长度限制行为。
func TestSanitizeProvisionError(t *testing.T) {
	msg := sanitizeProvisionError(errors.New("cipassword 'abc123' failed for 'abc123'"), "abc123")
	if strings.Contains(msg, "abc123") {
		t.Fatalf("message still carries the secret: %q", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Fatalf("message = %q, want [redacted] marker", msg)
	}
	long := strings.Repeat("x", maxProvisionErrorLen+100)
	msg = sanitizeProvisionError(errors.New(long), "secret")
	if len(msg) != maxProvisionErrorLen {
		t.Fatalf("length = %d, want %d", len(msg), maxProvisionErrorLen)
	}
}

// TestFailProvisionBoundsLength 固定 failProvision 契约：先拼接步骤前缀，
// 再对整条消息脱敏（先脱敏后截断），因此冗长的 PVE 错误无法把存储的
// provision_error 推过 maxProvisionErrorLen，前缀绝不会被截掉，跨越截断边界
// 的机密也绝不会泄露片段。
func TestFailProvisionBoundsLength(t *testing.T) {
	const secret = "s3cretpw-boundary"
	prefix := "create succeeded (vmid=100) but wait create failed: "
	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})

	// 机密恰好结束于带前缀消息的截断点之后，因此先截断后脱敏的顺序会留下
	// 片段。
	err := svc.failProvision(context.Background(), 1, 100, "wait create",
		errors.New(strings.Repeat("a", maxProvisionErrorLen-len(prefix)-4)+secret), secret)
	if err == nil {
		t.Fatal("failProvision returned nil")
	}
	if len(vmRepo.provisionErrors) != 1 {
		t.Fatalf("provision errors = %d, want 1", len(vmRepo.provisionErrors))
	}
	msg := vmRepo.provisionErrors[0]
	if !strings.HasPrefix(msg, prefix) {
		t.Fatalf("provision_error = %q, want the step prefix preserved", msg)
	}
	if utf8.RuneCountInString(msg) != maxProvisionErrorLen {
		t.Fatalf("provision_error length = %d runes, want %d", utf8.RuneCountInString(msg), maxProvisionErrorLen)
	}
	if strings.Contains(msg, secret) || strings.Contains(msg, "s3cr") {
		t.Fatalf("provision_error leaks the secret or a fragment: %q", msg)
	}
	if !utf8.ValidString(msg) {
		t.Fatalf("provision_error is not valid UTF-8: %q", msg)
	}
}

// TestSanitizeProvisionErrorRedactsBeforeTruncate 固定先脱敏后截断的顺序：
// 跨越长度边界的机密绝不能泄露部分片段，且结果必须保持合法的 UTF-8（朴素的
// 字节截断会切分多字节 rune，数据库会拒绝该值，Postgres 22021）。
func TestSanitizeProvisionErrorRedactsBeforeTruncate(t *testing.T) {
	secret := "PASSWORD-1234-5678"
	msg := strings.Repeat("a", maxProvisionErrorLen-8) + secret
	out := sanitizeProvisionError(errors.New(msg), secret)
	if strings.Contains(out, secret) {
		t.Fatalf("message leaks the secret across the truncation boundary: %q", out)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("message is not valid UTF-8: %q", out)
	}
}

// TestSanitizeProvisionErrorKeepsUTF8 检查多字节消息的 rune 安全截断：在
// UTF-8 字符内部按字节切割会产生非法序列（Postgres 拒绝它，错误 22021）。
func TestSanitizeProvisionErrorKeepsUTF8(t *testing.T) {
	msg := strings.Repeat("界", maxProvisionErrorLen+3)
	out := sanitizeProvisionError(errors.New(msg))
	if !utf8.ValidString(out) {
		t.Fatalf("message is not valid UTF-8: %q", out)
	}
	if got := utf8.RuneCountInString(out); got != maxProvisionErrorLen {
		t.Fatalf("rune count = %d, want %d", got, maxProvisionErrorLen)
	}
}

func TestParseDiskSizeGB(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"local-ssd:vm-100-disk-0,size=10G", 10},
		{"local-ssd:vm-100-disk-0,size=5G,backup=0", 5},
		{"local:vm-100-disk-0,size=10737418240", 10},
		{"local:vm-100-disk-0,size=1.5G", 1},
	}
	for _, tc := range cases {
		got, err := parseDiskSizeGB(tc.in)
		if err != nil {
			t.Fatalf("parseDiskSizeGB(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("parseDiskSizeGB(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if _, err := parseDiskSizeGB("local:vm-100-disk-0"); err == nil {
		t.Fatal("disk string without size should fail")
	}
}

// ---------- 生命周期操作测试 ----------

func provisionedVM() *repository.VMWithIP {
	return &repository.VMWithIP{
		VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 100, CPU: 2, MemMB: 2048, DiskGB: 10, Name: "vm1"},
		IP: "10.0.0.5",
	}
}

// startStopRestartServer 以 UPID 应答三个状态端点。
func startStopRestartServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := false
		for _, action := range []string{"/start", "/stop", "/reboot"} {
			if strings.HasSuffix(r.URL.Path, action) {
				ok = true
			}
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmstart:100:root@pam:"}`)
	}))
}

func TestStartStopRestart(t *testing.T) {
	ts := startStopRestartServer(t)
	defer ts.Close()

	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true}}}
	svc := newVMService(t, &fakeVMRepository{get: provisionedVM()}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	ctx := context.Background()
	if err := svc.Start(ctx, 1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := svc.Stop(ctx, 1); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := svc.Restart(ctx, 1); err != nil {
		t.Fatalf("Restart: %v", err)
	}
}

func TestStartVMNotFound(t *testing.T) {
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if err := svc.Start(context.Background(), 404); !isKind(err, KindNotFound) {
		t.Fatalf("err = %v, want KindNotFound", err)
	}
}

func TestStartVMNotProvisioned(t *testing.T) {
	vm := provisionedVM()
	vm.VM.PVEVmid = 0
	svc := newVMService(t, &fakeVMRepository{get: vm}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if err := svc.Start(context.Background(), 1); !isKind(err, KindVMNotReady) {
		t.Fatalf("err = %v, want KindVMNotReady", err)
	}
}

// TestStartVMPVENotFound 将 PVE 404（VM 在节点上已不存在）映射为
// vm_not_ready。
func TestStartVMPVENotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors": {"_": "VM 100 not found on this node"}}`)
	}))
	defer ts.Close()

	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, &fakeVMRepository{get: provisionedVM()}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	if err := svc.Start(context.Background(), 1); !isKind(err, KindVMNotReady) {
		t.Fatalf("err = %v, want KindVMNotReady", err)
	}
}

func TestDestroyFlow(t *testing.T) {
	srv := newScriptedProvisionServer(t, "10G")
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM()}
	ipRepo := &fakeVMIPPoolRepository{}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, vmRepo, ipRepo, &fakeVMZoneRepository{}, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	if err := svc.Destroy(context.Background(), 1); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !srv.destroyed {
		t.Fatal("PVE destroy was not invoked")
	}
	// IP 在同一事务中释放且行被删除。
	if len(ipRepo.released) != 1 || ipRepo.released[0] != 1 {
		t.Fatalf("released = %v, want [1]", ipRepo.released)
	}
	if len(vmRepo.deleted) != 1 || vmRepo.deleted[0] != 1 {
		t.Fatalf("deleted = %v, want [1]", vmRepo.deleted)
	}
}

// TestDestroyUnprovisioned 跳过 PVE 调用（还没有 pve_vmid）但仍清理本地记录
// 和 IP。
func TestDestroyUnprovisioned(t *testing.T) {
	vm := provisionedVM()
	vm.VM.PVEVmid = 0
	vmRepo := &fakeVMRepository{get: vm}
	ipRepo := &fakeVMIPPoolRepository{}
	svc := newVMService(t, vmRepo, ipRepo, &fakeVMZoneRepository{}, &fakeVMNodeRepository{},
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	// newVMService 的 newClient 对所有请求都应答 503；PVE 调用绝不能发生，
	// 因此任何调用都会导致下面的测试失败。

	if err := svc.Destroy(context.Background(), 1); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(ipRepo.released) != 1 || len(vmRepo.deleted) != 1 {
		t.Fatalf("released = %v, deleted = %v", ipRepo.released, vmRepo.deleted)
	}
}

func TestDestroyVMNotFound(t *testing.T) {
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if err := svc.Destroy(context.Background(), 404); !isKind(err, KindNotFound) {
		t.Fatalf("err = %v, want KindNotFound", err)
	}
}

// TestDestroyPVEFailureKeepsRecordAndIP 验证 PVE 失败会中止销毁：IP 和行
// 都未被触碰。
func TestDestroyPVEFailureKeepsRecordAndIP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors": {"_": "cannot destroy"}}`)
	}))
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM()}
	ipRepo := &fakeVMIPPoolRepository{}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, vmRepo, ipRepo, &fakeVMZoneRepository{}, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	if err := svc.Destroy(context.Background(), 1); err == nil {
		t.Fatal("Destroy succeeded, want PVE failure")
	}
	if len(ipRepo.released) != 0 || len(vmRepo.deleted) != 0 {
		t.Fatalf("released = %v, deleted = %v, want untouched", ipRepo.released, vmRepo.deleted)
	}
}

// TestDestroyPVE404CleansUpLocal 将销毁时的 PVE 404（VM 已在节点上被移除，
// 例如被运维删除）视为"已销毁"：本地清理仍然执行且销毁成功。
func TestDestroyPVE404CleansUpLocal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors": {"_": "VM 100 not found on this node"}}`)
	}))
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM()}
	ipRepo := &fakeVMIPPoolRepository{}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, vmRepo, ipRepo, &fakeVMZoneRepository{}, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	if err := svc.Destroy(context.Background(), 1); err != nil {
		t.Fatalf("Destroy with PVE 404: %v", err)
	}
	if len(ipRepo.released) != 1 || ipRepo.released[0] != 1 {
		t.Fatalf("released = %v, want [1]", ipRepo.released)
	}
	if len(vmRepo.deleted) != 1 || vmRepo.deleted[0] != 1 {
		t.Fatalf("deleted = %v, want [1]", vmRepo.deleted)
	}
}

// ---------- 调整（resize）测试 ----------

// noCallServer 在使用 PVE 客户端时使测试失败；用于断言校验/无操作路径上
// 不会发生 PVE 调用。
func noCallServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected PVE call: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
}

func TestResizeRejectsShrinkBeforePVE(t *testing.T) {
	ts := noCallServer(t)
	defer ts.Close()

	svc := newVMService(t, &fakeVMRepository{get: provisionedVM()}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}},
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	disk := int64(5)
	if _, err := svc.Resize(context.Background(), 1, nil, nil, &disk); !isKind(err, KindDiskShrinkNotAllowed) {
		t.Fatalf("err = %v, want KindDiskShrinkNotAllowed", err)
	}
}

// TestResizeNoOpEqualDisk 验证文档化的磁盘相等无操作：没有 PVE 调用，也
// 没有规格持久化。
func TestResizeNoOpEqualDisk(t *testing.T) {
	ts := noCallServer(t)
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM()}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}},
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	disk := int64(10) // 等于当前 10G
	vm, err := svc.Resize(context.Background(), 1, nil, nil, &disk)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if vm.VM.DiskGB != 10 {
		t.Fatalf("disk_gb = %d, want 10", vm.VM.DiskGB)
	}
	if len(vmRepo.updatedSpecs) != 0 {
		t.Fatalf("UpdateSpec called for a no-op: %+v", vmRepo.updatedSpecs)
	}
}

// resizeServer 应答 PUT /config（同步）、PUT /resize（UPID）以及 resize
// 任务状态。
func resizeServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/nodes/pve1/qemu/100/config" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"data": {"bootdisk": "scsi0", "scsi0": "local-ssd:vm-100-disk-0,size=10G"}}`)
		case r.URL.Path == "/nodes/pve1/qemu/100/config" && r.Method == http.MethodPut:
			fmt.Fprint(w, `{"data": null}`)
		case r.URL.Path == "/nodes/pve1/qemu/100/resize":
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:resize:100:root@pam:"}`)
		case strings.HasPrefix(r.URL.Path, "/nodes/pve1/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprint(w, `{"data": {"upid": "UPID:pve1:0:0:0:resize:100:root@pam:", "node": "pve1", "type": "resize", "id": "100", "user": "root@pam", "status": "stopped", "exitstatus": "OK"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func TestResizeGrowAll(t *testing.T) {
	ts := resizeServer(t)
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM()}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{}, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	cpu := 4
	mem := int64(4096)
	disk := int64(20)
	vm, err := svc.Resize(context.Background(), 1, &cpu, &mem, &disk)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if vm.VM.CPU != 4 || vm.VM.MemMB != 4096 || vm.VM.DiskGB != 20 {
		t.Fatalf("vm = %+v", vm.VM)
	}
	if len(vmRepo.updatedSpecs) != 1 {
		t.Fatalf("updated specs = %+v, want 1", vmRepo.updatedSpecs)
	}
	u := vmRepo.updatedSpecs[0]
	if u.new.cpu != 4 || u.new.memMB != 4096 || u.new.diskGB != 20 {
		t.Fatalf("new spec = %+v, want cpu=4 mem=4096 disk=20", u.new)
	}
	if u.old.cpu != 2 || u.old.memMB != 2048 || u.old.diskGB != 10 {
		t.Fatalf("old spec (optimistic lock baseline) = %+v, want cpu=2 mem=2048 disk=10", u.old)
	}
}

// TestResizeSpecConflict 覆盖乐观锁分支：并发的调整在读取与持久化之间提交，
// 因此 UpdateSpec 报告 ErrSpecConflict，调用方呈现 KindConflict。
func TestResizeSpecConflict(t *testing.T) {
	ts := resizeServer(t)
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM(), updateSpecErr: repository.ErrSpecConflict}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{}, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	cpu := 4
	_, err := svc.Resize(context.Background(), 1, &cpu, nil, nil)
	if !isKind(err, KindConflict) {
		t.Fatalf("err = %v, want KindConflict", err)
	}
	if !strings.Contains(err.Error(), "规格已被并发修改") {
		t.Fatalf("err = %q, want the concurrent-modification message", err)
	}
}
