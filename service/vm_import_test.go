package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
	"spark/repository"
)

// ---------- 假 PVE 服务器 ----------

// newImportPVEServer 构建导入/未托管列表测试用的假 PVE 服务器：脚本化
// ListVMs（GET /nodes/pve1/qemu）与 GetVMConfig
// （GET /nodes/pve1/qemu/{vmid}/config）响应。configs 中缺失的 vmid
// 返回 404；未预期的请求以测试失败暴露。
func newImportPVEServer(t *testing.T, listJSON string, configs map[int64]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/nodes/pve1/qemu" && r.Method == http.MethodGet:
			fmt.Fprint(w, listJSON)
		case strings.HasPrefix(r.URL.Path, "/nodes/pve1/qemu/") && strings.HasSuffix(r.URL.Path, "/config"):
			vmidStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/nodes/pve1/qemu/"), "/config")
			vmid, err := strconv.ParseInt(vmidStr, 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			cfg, ok := configs[vmid]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, cfg)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

// newImportSvc 装配带假仓库与假 PVE 服务器的 VMService；节点的 PVE 集群名
// 固定为 pve1（与 testPVENode 一致），所有 PVE 请求打到 ts。
func newImportSvc(t *testing.T, vmRepo VMRepository, ipRepo VMIPPoolRepository,
	zoneRepo VMZoneRepository, nodeRepo VMNodeRepository, ts *httptest.Server) *VMService {
	t.Helper()
	svc := newVMService(t, vmRepo, ipRepo, zoneRepo, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	return svc
}

// importTestEnv 为导入测试构建区域 1 + 启用节点 1（pve1）+ 池 1
// （10.0.0.0/24，白名单含节点 1）的假环境。
func importTestEnv() (*fakeVMZoneRepository, *fakeVMNodeRepository, *fakeVMIPPoolRepository) {
	node := testPVENode(1)
	zoneRepo := &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{node}}
	ipRepo := &fakeVMIPPoolRepository{
		pools:     []model.IPPool{{ID: 1, ZoneID: 1, Name: "p1", NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1", DNS: "1.1.1.1"}},
		poolNodes: map[int64][]model.PVENode{1: {node}},
	}
	return zoneRepo, nodeRepo, ipRepo
}

// importConfigJSON 是 PVE 返回的 VM 配置响应体：名称/规格/磁盘 + 静态 IP。
func importConfigJSON() string {
	return `{"data": {"name": "web-01", "cores": "2", "memory": "4096", "scsi0": "local-lvm:vm-100-disk-0,size=20G", "ipconfig0": "ip=10.0.0.5/24,gw=10.0.0.1"}}`
}

// importListJSON 是 PVE 返回的节点 VM 列表响应体：仅含 vmid 100。
func importListJSON() string {
	return `{"data": [{"vmid": 100, "name": "web-01", "status": "running"}]}`
}

// ---------- 导入（ImportVM）测试 ----------

// TestImportVMHappyPath 覆盖成功导入：静态 IP 与池 CIDR 匹配，按地址精确
// 领取；断言返回的 VMWithIP、IP、ImportVMTx/SetVMIPIDTx 参数。
func TestImportVMHappyPath(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	ipRepo.claimAddressResults = []claimResult{{ip: model.IP{ID: 8, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}}}
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	vm, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if err != nil {
		t.Fatalf("ImportVM: %v", err)
	}
	if vm.VM.ID != 1 || vm.VM.PVEVmid != 100 || vm.VM.Name != "web-01" {
		t.Fatalf("vm = %+v, want id 1 / pve_vmid 100 / name web-01", vm.VM)
	}
	// 规格来自 PVE 配置：2 核 / 4096 MiB / 20 GiB。
	if vm.VM.CPU != 2 || vm.VM.MemMB != 4096 || vm.VM.DiskGB != 20 {
		t.Fatalf("spec = cpu %d mem %d disk %d, want 2/4096/20", vm.VM.CPU, vm.VM.MemMB, vm.VM.DiskGB)
	}
	// 导入的 VM 不关联镜像/存储类型，且无密码。
	if vm.VM.ImageID != nil || vm.VM.StorageTypeID != nil || vm.VM.PasswordEncrypted != "" {
		t.Fatalf("imported vm must have nil image/storage ids and no password, got %+v", vm.VM)
	}
	if vm.IP != "10.0.0.5" {
		t.Fatalf("ip = %q, want the reused static ip 10.0.0.5", vm.IP)
	}
	if vm.VM.IPID == nil || *vm.VM.IPID != 8 {
		t.Fatalf("ip_id = %v, want 8", vm.VM.IPID)
	}
	// ImportVMTx 参数：非零 pve_vmid、ip_id 为 NULL、可空关联字段为 NULL。
	if vmRepo.imported == nil {
		t.Fatal("ImportVMTx was not called")
	}
	imp := vmRepo.imported
	if imp.UUID == "" || imp.PVEVmid != 100 || imp.ZoneID != 1 || imp.NodeID != 1 ||
		imp.Name != "web-01" || imp.IPID != nil || imp.ImageID != nil || imp.StorageTypeID != nil ||
		imp.PasswordEncrypted != "" || imp.ProvisionError != "" {
		t.Fatalf("ImportVMTx args = %+v", imp)
	}
	// 静态 IP 按地址领取，随后回填 ip_id。
	if len(ipRepo.addressClaims) != 1 || ipRepo.addressClaims[0].poolID != 1 ||
		ipRepo.addressClaims[0].ip != "10.0.0.5" || ipRepo.addressClaims[0].vmID == nil ||
		*ipRepo.addressClaims[0].vmID != 1 {
		t.Fatalf("address claims = %+v, want pool 1 / 10.0.0.5 / vm id 1", ipRepo.addressClaims)
	}
	if vmRepo.linkedIPID != 8 {
		t.Fatalf("linked ip id = %d, want 8", vmRepo.linkedIPID)
	}
}

// TestImportVMRequestNameWins 覆盖名称回退：请求携带 name 时覆盖 PVE 配置名。
func TestImportVMRequestNameWins(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	ipRepo.claimAddressResults = []claimResult{{ip: model.IP{ID: 8, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}}}
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	vm, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, Name: "renamed"})
	if err != nil {
		t.Fatalf("ImportVM: %v", err)
	}
	if vm.VM.Name != "renamed" {
		t.Fatalf("name = %q, want the request name", vm.VM.Name)
	}
}

// TestImportVMAlreadyManaged 覆盖幂等检查：节点上的 pve_vmid 已被托管时
// 拒绝导入（409），且不产生任何 PVE 调用。
func TestImportVMAlreadyManaged(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	vmRepo := &fakeVMRepository{getByNodeVMID: &model.VM{ID: 5, NodeID: 1, PVEVmid: 100}}
	ts := noCallServer(t)
	defer ts.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, ts)

	_, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if !isKind(err, KindVMAlreadyManaged) {
		t.Fatalf("err = %v, want KindVMAlreadyManaged", err)
	}
	if vmRepo.imported != nil {
		t.Fatal("ImportVMTx must not be called for a duplicate import")
	}
}

// TestImportVMNotFoundOnNode 覆盖真实"VM 已从 PVE 删除"场景：列表不含该
// vmid 且配置端点返回 404。导入必须先查列表再读配置（M1），否则 PVE 404
// 会被误判为节点不可用（503）——这里断言返回 404 vm_not_found_on_node。
func TestImportVMNotFoundOnNode(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	vmRepo := &fakeVMRepository{}
	// 列表只含 vmid 200，导入 100 -> not_found；configs 为空使配置端点
	// 返回 404，锁定"config 404 不应吞成 503"的行为。
	srv := newImportPVEServer(t, `{"data": [{"vmid": 200, "name": "other", "status": "stopped"}]}`, nil)
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	_, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if !isKind(err, KindVMNotFoundOnNode) {
		t.Fatalf("err = %v, want KindVMNotFoundOnNode", err)
	}
	if vmRepo.imported != nil {
		t.Fatal("ImportVMTx must not be called when the vm is absent on the node")
	}
}

// TestImportVMTemplateRejected 覆盖 PVE 模板导入拒绝：列表条目 template==1
// -> 400 bad_request，且不落库。
func TestImportVMTemplateRejected(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, `{"data": [{"vmid": 100, "name": "ubuntu-cloud", "status": "stopped", "template": 1}]}`, nil)
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	_, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if !isKind(err, KindBadRequest) {
		t.Fatalf("err = %v, want KindBadRequest", err)
	}
	if vmRepo.imported != nil {
		t.Fatal("ImportVMTx must not be called for a pve template")
	}
}

// TestImportVMInvalidName 覆盖请求 name 不合法（不匹配 vmNamePattern）：
// -> 400 bad_request（与 CreateVM 相同的校验语义）。
func TestImportVMInvalidName(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	_, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, Name: "bad name!"})
	if !isKind(err, KindBadRequest) {
		t.Fatalf("err = %v, want KindBadRequest", err)
	}
	if vmRepo.imported != nil {
		t.Fatal("ImportVMTx must not be called for an invalid name")
	}
}

// TestImportVMRequestNameTooLong 覆盖请求 name 超长（超过契约 maxLength
// 128 字符）：-> 400 bad_request，且不落库。
func TestImportVMRequestNameTooLong(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	_, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, Name: strings.Repeat("n", maxImportedNameLen+1)})
	if !isKind(err, KindBadRequest) {
		t.Fatalf("err = %v, want KindBadRequest", err)
	}
	if vmRepo.imported != nil {
		t.Fatal("ImportVMTx must not be called for an overlong request name")
	}
}

// TestImportVMLongPVENameTruncated 覆盖 PVE 配置名超长：截断到
// maxImportedNameLen（128 字符）而非拒绝。
func TestImportVMLongPVENameTruncated(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	// 配置不含静态 IP，回退路径随机分配需要结果脚本。
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 8, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}}}
	longName := strings.Repeat("n", 200)
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(),
		map[int64]string{100: `{"data": {"name": "` + longName + `", "cores": "2", "memory": "4096", "scsi0": "local-lvm:vm-100-disk-0,size=20G"}}`})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	vm, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if err != nil {
		t.Fatalf("ImportVM: %v", err)
	}
	if vm.VM.Name != longName[:maxImportedNameLen] || len(vm.VM.Name) != maxImportedNameLen {
		t.Fatalf("name = %q (len %d), want truncated to %d chars", vm.VM.Name, len(vm.VM.Name), maxImportedNameLen)
	}
}

// TestImportVMLongChinesePVENameTruncatedByRune 覆盖多字节 PVE 配置名超长：
// 按字符（rune）截断到 maxImportedNameLen，不产生非法 UTF-8（字节切片会
// 截出半个字符）。
func TestImportVMLongChinesePVENameTruncatedByRune(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 8, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}}}
	longName := strings.Repeat("中文", 100) // 200 个字符、400 字节
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(),
		map[int64]string{100: `{"data": {"name": "` + longName + `", "cores": "2", "memory": "4096", "scsi0": "local-lvm:vm-100-disk-0,size=20G"}}`})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	vm, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if err != nil {
		t.Fatalf("ImportVM: %v", err)
	}
	if !utf8.ValidString(vm.VM.Name) {
		t.Fatalf("name = %q is not valid utf-8", vm.VM.Name)
	}
	if n := utf8.RuneCountInString(vm.VM.Name); n != maxImportedNameLen {
		t.Fatalf("name rune count = %d, want %d", n, maxImportedNameLen)
	}
}

// TestImportVMNodeUnavailable 覆盖节点 PVE 调用失败：配置读取失败 -> 503，
// 且不落库。
func TestImportVMNodeUnavailable(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	vmRepo := &fakeVMRepository{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"errors": {"_": "pve daemon down"}}`)
	}))
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	_, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}
	if vmRepo.imported != nil {
		t.Fatal("ImportVMTx must not be called when the node is unavailable")
	}
}

// TestImportVMStaticIPNoMatchingPool 覆盖静态 IP 无匹配池（CIDR 不包含）：
// 回退到池内随机分配。
func TestImportVMStaticIPNoMatchingPool(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	// 池 CIDR 与静态 IP 10.0.0.5 不匹配。
	ipRepo.pools[0].NetworkCIDR = "10.1.0.0/24"
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 9, PoolID: 1, IP: "10.1.0.7", Status: model.IPStatusUsed}}}
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	vm, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if err != nil {
		t.Fatalf("ImportVM: %v", err)
	}
	if vm.IP != "10.1.0.7" {
		t.Fatalf("ip = %q, want the fallback allocation 10.1.0.7", vm.IP)
	}
	// 未发生按地址领取，回退走 ClaimFreeIP（vm id 已关联）。
	if len(ipRepo.addressClaims) != 0 {
		t.Fatalf("address claims = %+v, want none", ipRepo.addressClaims)
	}
	if len(ipRepo.claimedVMIDs) != 1 || ipRepo.claimedVMIDs[0] != 1 {
		t.Fatalf("claimed vm ids = %v, want [1]", ipRepo.claimedVMIDs)
	}
}

// TestImportVMStaticIPClaimRace 覆盖静态 IP 被并发抢占（ClaimIPByAddressTx
// 返回 ErrAllocationRetry）：回退从池分配新地址，请求不失败（设计 D3
// Risks）。
func TestImportVMStaticIPClaimRace(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	ipRepo.claimAddressResults = []claimResult{{err: repository.ErrAllocationRetry}}
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 9, PoolID: 1, IP: "10.0.0.9", Status: model.IPStatusUsed}}}
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	vm, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if err != nil {
		t.Fatalf("ImportVM: %v", err)
	}
	if vm.IP != "10.0.0.9" {
		t.Fatalf("ip = %q, want the fallback allocation 10.0.0.9", vm.IP)
	}
	if len(ipRepo.addressClaims) != 1 {
		t.Fatalf("address claims = %+v, want the single by-address attempt", ipRepo.addressClaims)
	}
	if len(ipRepo.claimedVMIDs) != 1 || ipRepo.claimedVMIDs[0] != 1 {
		t.Fatalf("claimed vm ids = %v, want [1]", ipRepo.claimedVMIDs)
	}
}

// TestImportVMStaticIPClaimContinuesNextPool 覆盖静态 IP 命中多个池且第一
// 个池领取失败（ErrAllocationRetry）：继续尝试 CIDR 同样包含该地址的下
// 一个池（m3），而非直接回退随机分配。
func TestImportVMStaticIPClaimContinuesNextPool(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	// 池 1 与池 2 的 CIDR 都包含静态 IP 10.0.0.5 且白名单都含节点 1；
	// 池 1 领取失败，池 2 按地址领取成功。
	ipRepo.pools = []model.IPPool{
		{ID: 1, ZoneID: 1, Name: "p1", NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
		{ID: 2, ZoneID: 1, Name: "p2", NetworkCIDR: "10.0.0.0/16", Gateway: "10.0.0.1"},
	}
	ipRepo.poolNodes = map[int64][]model.PVENode{1: {testPVENode(1)}, 2: {testPVENode(1)}}
	ipRepo.claimAddressResults = []claimResult{{err: repository.ErrAllocationRetry}, {ip: model.IP{ID: 8, PoolID: 2, IP: "10.0.0.5", Status: model.IPStatusUsed}}}
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	vm, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if err != nil {
		t.Fatalf("ImportVM: %v", err)
	}
	if vm.IP != "10.0.0.5" {
		t.Fatalf("ip = %q, want the reused static ip from pool 2", vm.IP)
	}
	// 两个池都被尝试：池 1 领取失败后继续尝试池 2。
	if len(ipRepo.addressClaims) != 2 ||
		ipRepo.addressClaims[0].poolID != 1 || ipRepo.addressClaims[1].poolID != 2 {
		t.Fatalf("address claims = %+v, want pool 1 then pool 2", ipRepo.addressClaims)
	}
}

// TestImportVMPoolExhausted 覆盖池耗尽：回退路径 ClaimFreeIP 无空闲地址
// 时返回 ip_exhausted（设计 D3）。
func TestImportVMPoolExhausted(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	// 静态 IP 不匹配池，回退随机分配也耗尽。
	ipRepo.pools[0].NetworkCIDR = "10.1.0.0/24"
	ipRepo.claimDefault = pgx.ErrNoRows
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	_, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if !isKind(err, KindIPExhausted) {
		t.Fatalf("err = %v, want KindIPExhausted", err)
	}
}

// TestImportVMStaticIPNoPoolAllowsNode 覆盖静态 IP 的池 CIDR 匹配但白名单
// 不含该节点：静态 IP 未命中，回退到区域内白名单包含该节点的其他池分配
// （设计 D3 场景"静态 IP 无匹配池"）。
func TestImportVMStaticIPNoPoolAllowsNode(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	// 池 1 的 CIDR 匹配静态 IP 10.0.0.5，但白名单只含节点 2；池 2 白名单
	// 含节点 1，作为回退目标。
	ipRepo.pools = []model.IPPool{
		{ID: 1, ZoneID: 1, Name: "p1", NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
		{ID: 2, ZoneID: 1, Name: "p2", NetworkCIDR: "10.1.0.0/24", Gateway: "10.1.0.1"},
	}
	ipRepo.poolNodes = map[int64][]model.PVENode{1: {testPVENode(2)}, 2: {testPVENode(1)}}
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 9, PoolID: 2, IP: "10.1.0.7", Status: model.IPStatusUsed}}}
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	vm, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if err != nil {
		t.Fatalf("ImportVM: %v", err)
	}
	if vm.IP != "10.1.0.7" {
		t.Fatalf("ip = %q, want the fallback allocation 10.1.0.7", vm.IP)
	}
	if len(ipRepo.addressClaims) != 0 {
		t.Fatalf("address claims = %+v, want none", ipRepo.addressClaims)
	}
}

// TestImportVMSpecifiedPool 覆盖用户指定池分支：指定池可用时在其内分配；
// 池不属于该区域、白名单不含节点或池不存在时返回 bad_request。
func TestImportVMSpecifiedPool(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	ipRepo.pools = []model.IPPool{{ID: 5, ZoneID: 1, Name: "p5", NetworkCIDR: "10.2.0.0/24", Gateway: "10.2.0.1"}}
	ipRepo.poolNodes = map[int64][]model.PVENode{5: {testPVENode(1)}}
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 9, PoolID: 5, IP: "10.2.0.3", Status: model.IPStatusUsed}}}
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	// 指定池成功：静态 IP 10.0.0.5 被忽略，改用池 5 的随机分配。
	vm, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, IPPoolID: 5})
	if err != nil {
		t.Fatalf("ImportVM: %v", err)
	}
	if vm.IP != "10.2.0.3" {
		t.Fatalf("ip = %q, want the pool-5 allocation 10.2.0.3", vm.IP)
	}
	if len(ipRepo.addressClaims) != 0 {
		t.Fatalf("address claims = %+v, want none", ipRepo.addressClaims)
	}

	// 池属于其他区域 -> bad_request。
	ipRepo.pools[0].ZoneID = 2
	_, err = svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, IPPoolID: 5})
	if !isKind(err, KindBadRequest) {
		t.Fatalf("wrong-zone pool err = %v, want KindBadRequest", err)
	}
	ipRepo.pools[0].ZoneID = 1

	// 白名单不含该节点 -> bad_request。
	ipRepo.poolNodes[5] = nil
	_, err = svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, IPPoolID: 5})
	if !isKind(err, KindBadRequest) {
		t.Fatalf("not-allowed pool err = %v, want KindBadRequest", err)
	}

	// 池不存在 -> bad_request。
	_, err = svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, IPPoolID: 99})
	if !isKind(err, KindBadRequest) {
		t.Fatalf("missing pool err = %v, want KindBadRequest", err)
	}
}

// TestImportVMNodeValidation 覆盖节点相关校验：未知节点 not_found、节点
// 不属于该区域 bad_request、节点禁用 node_unavailable。
func TestImportVMNodeValidation(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	vmRepo := &fakeVMRepository{}
	ts := noCallServer(t)
	defer ts.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, ts)

	// 未知节点 -> not_found。
	_, err := svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 99, PVEVmid: 100})
	if !isKind(err, KindNotFound) {
		t.Fatalf("unknown node err = %v, want KindNotFound", err)
	}
	// 节点不属于该区域 -> bad_request。
	nodeRepo.nodes[0].ZoneID = 2
	_, err = svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if !isKind(err, KindBadRequest) {
		t.Fatalf("wrong-zone node err = %v, want KindBadRequest", err)
	}
	// 节点禁用 -> node_unavailable。
	nodeRepo.nodes[0].ZoneID = 1
	nodeRepo.nodes[0].Enabled = false
	_, err = svc.ImportVM(context.Background(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("disabled node err = %v, want KindNodeUnavailable", err)
	}
}

// ---------- 未托管候选（ListUnmanagedVMs）测试 ----------

// TestListUnmanagedVMs 覆盖混合场景：已托管的 vmid 被过滤、PVE 模板被
// 过滤、运行中的候选用摘要字段、已停止的候选（摘要为零值）用 GetVMConfig
// 补全规格，结果按 VMID 升序。
func TestListUnmanagedVMs(t *testing.T) {
	// 节点 1 已托管 vmid 100；节点 2 的托管行不影响节点 1 的候选。
	vmRepo := &fakeVMRepository{vms: []repository.VMWithIP{
		{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 100, Name: "managed"}},
		{VM: model.VM{ID: 2, NodeID: 2, PVEVmid: 200, Name: "other-node"}},
	}}
	listJSON := `{"data": [
		{"vmid": 100, "name": "managed", "status": "running", "cpus": 2, "maxmem": 2147483648, "maxdisk": 10737418240},
		{"vmid": 150, "name": "ubuntu-cloud", "status": "stopped", "template": 1},
		{"vmid": 200, "name": "web-02", "status": "running", "cpus": 4, "maxmem": 8589934592, "maxdisk": 107374182400},
		{"vmid": 300, "name": "", "status": "stopped"}
	]}`
	// vmid 300 已停止（摘要零值）：配置补全 4 核 / 8192 MiB / 50G+10G 磁盘。
	configs := map[int64]string{300: `{"data": {"name": "legacy-db", "cores": "4", "memory": "8192", "scsi0": "local:vm-300-disk-0,size=50G", "virtio1": "local:vm-300-disk-1,size=10G"}}`}
	srv := newImportPVEServer(t, listJSON, configs)
	defer srv.Close()

	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)

	cands, err := svc.ListUnmanagedVMs(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListUnmanagedVMs: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %+v, want exactly 2 (managed 100 and template 150 filtered)", cands)
	}
	// 按 VMID 升序。
	first, second := cands[0], cands[1]
	if first.VMID != 200 || second.VMID != 300 {
		t.Fatalf("order = %d, %d, want 200 then 300", first.VMID, second.VMID)
	}
	// 运行中的候选取摘要字段（MaxMem 字节 >> 20 = 8192 MiB，MaxDisk >> 30
	// = 100 GiB）。
	if first.Name != "web-02" || first.Status != "running" || first.CPU != 4 ||
		first.MemMB != 8192 || first.DiskGB != 100 {
		t.Fatalf("candidate 200 = %+v, want web-02 running 4/8192/100", first)
	}
	// 已停止的候选从配置补全：名称/核数/内存/磁盘求和（50G+10G）。
	if second.Name != "legacy-db" || second.Status != "stopped" || second.CPU != 4 ||
		second.MemMB != 8192 || second.DiskGB != 60 {
		t.Fatalf("candidate 300 = %+v, want legacy-db stopped 4/8192/60 (config-completed)", second)
	}
}

// TestListUnmanagedVMsNodeNotFound 覆盖未知节点 -> not_found。
func TestListUnmanagedVMsNodeNotFound(t *testing.T) {
	zoneRepo, _, ipRepo := importTestEnv()
	ts := noCallServer(t)
	defer ts.Close()
	svc := newImportSvc(t, &fakeVMRepository{}, ipRepo, zoneRepo, &fakeVMNodeRepository{}, ts)

	_, err := svc.ListUnmanagedVMs(context.Background(), 99)
	if !isKind(err, KindNotFound) {
		t.Fatalf("err = %v, want KindNotFound", err)
	}
}

// TestListUnmanagedVMsDisabledNode 覆盖节点被禁用（与 ImportVM 一致的
// node_unavailable 语义，L4）：不发起任何 PVE 调用。
func TestListUnmanagedVMsDisabledNode(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	nodeRepo.nodes[0].Enabled = false
	ts := noCallServer(t)
	defer ts.Close()
	svc := newImportSvc(t, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo, ts)

	_, err := svc.ListUnmanagedVMs(context.Background(), 1)
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}
}

// TestListUnmanagedVMsNodeUnavailable 覆盖节点 PVE 列表失败 -> 503。
func TestListUnmanagedVMsNodeUnavailable(t *testing.T) {
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"errors": {"_": "bad gateway"}}`)
	}))
	defer srv.Close()
	svc := newImportSvc(t, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo, srv)

	_, err := svc.ListUnmanagedVMs(context.Background(), 1)
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}
}
