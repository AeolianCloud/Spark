package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spark/model"
	"spark/pve"
	"spark/repository"
)

// ---------- 纯合并测试（任务 8.1/8.3） ----------

func vw(id, nodeID, vmid int64, name string) repository.VMWithIP {
	return repository.VMWithIP{
		VM: model.VM{ID: id, UUID: "uuid", Name: name, ZoneID: 1, NodeID: nodeID, PVEVmid: vmid,
			CPU: 2, MemMB: 2048, DiskGB: 10},
		IP: "10.0.0.5",
	}
}

// TestMergeVMListItemsDownNode 验证任务 8.3：失败的节点贡献一条警告且其
// VM 全部缺席；另一节点的 VM 仍然出现。
func TestMergeVMListItemsDownNode(t *testing.T) {
	local := []repository.VMWithIP{vw(1, 1, 100, "vm1"), vw(2, 2, 200, "vm2"), vw(3, 1, 101, "vm3")}
	nodes := map[int64]nodeQueryResult{
		1: {Name: "pve1", VMs: []pve.VMStatus{{VMID: 100, Status: "running"}, {VMID: 101, Status: "stopped"}}},
		2: {Name: "pve2", Err: errors.New("pve: GET /nodes/pve2/qemu: connection refused")},
	}

	items, warnings := mergeVMListItems(local, nodes)
	if len(items) != 2 || items[0].VM.VM.ID != 1 || items[1].VM.VM.ID != 3 {
		t.Fatalf("items = %+v, want the two VMs of pve1 (node 2 dropped)", items)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %+v, want exactly one", warnings)
	}
	if warnings[0].Node != "pve2" || !strings.Contains(warnings[0].Error, "connection refused") {
		t.Fatalf("warning = %+v, want pve2 with the pve error", warnings[0])
	}
}

// TestMergeVMListItemsStatuses 覆盖任务 8.1 的完整状态矩阵：creating
// （pve_vmid=0）、failed（provision_error）、合并的实时状态、仅存在于 PVE
// 的被跳过，以及 VM 从可达节点消失（设计 D5 -> creating）。
func TestMergeVMListItemsStatuses(t *testing.T) {
	local := []repository.VMWithIP{
		vw(1, 1, 0, "creating-vm"), // 创建中
		{VM: model.VM{ID: 2, UUID: "u", Name: "failed-vm", ZoneID: 1, NodeID: 1,
			CPU: 2, MemMB: 2048, DiskGB: 10, ProvisionError: "create (vmid=100) failed: no space"}, IP: "10.0.0.6"},
		vw(3, 1, 100, "live-vm"), // 与 PVE 状态合并
		vw(4, 1, 101, "gone-vm"), // PVE 可达但 VM 已消失
	}
	nodes := map[int64]nodeQueryResult{
		// 999 是仅存在于 PVE 的 VM：它必须被跳过（没有本地元数据）。
		1: {Name: "pve1", VMs: []pve.VMStatus{
			{VMID: 100, Name: "live-vm", Status: "running", CPU: 0.25, Cpus: 2,
				Mem: 1073741824, MaxMem: 2147483648, Disk: 5368709120, MaxDisk: 10737418240, Uptime: 12345},
			{VMID: 999, Name: "orphan", Status: "stopped"},
		}},
	}

	items, warnings := mergeVMListItems(local, nodes)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	if len(items) != 4 {
		t.Fatalf("items = %+v, want 4 (orphan 999 skipped)", items)
	}

	creating := items[0]
	if creating.Status != model.VMStateCreating || creating.Live != nil {
		t.Fatalf("creating item = %+v, want status creating and no live metrics", creating)
	}
	failed := items[1]
	if failed.Status != model.VMStateFailed || failed.Live != nil {
		t.Fatalf("failed item = %+v, want status failed and no live metrics", failed)
	}
	if !strings.Contains(failed.VM.VM.ProvisionError, "no space") {
		t.Fatalf("failed item must carry the provision error, got %q", failed.VM.VM.ProvisionError)
	}
	live := items[2]
	if live.Status != "running" || live.Live == nil {
		t.Fatalf("live item = %+v, want merged status running", live)
	}
	if live.Live.CPUUsage != 0.25 || live.Live.Mem != 1073741824 || live.Live.MaxMem != 2147483648 ||
		live.Live.Disk != 5368709120 || live.Live.MaxDisk != 10737418240 || live.Live.Uptime != 12345 {
		t.Fatalf("live metrics = %+v, want the PVE values", live.Live)
	}
	// 规格大小保持本地值（设计 D1）：合并项保留数据库中的值。
	if live.VM.VM.CPU != 2 || live.VM.VM.MemMB != 2048 || live.VM.VM.DiskGB != 10 {
		t.Fatalf("merged spec = %+v, want the local DB values", live.VM.VM)
	}
	gone := items[3]
	if gone.Status != model.VMStateCreating || gone.Live != nil {
		t.Fatalf("gone item = %+v, want creating (design D5: PVE 不存在)", gone)
	}
}

// TestMergeVMListItemsWarningOrder 检查输出的确定性：无论 map 迭代顺序
// 如何，警告都按节点名排序。
func TestMergeVMListItemsWarningOrder(t *testing.T) {
	nodes := map[int64]nodeQueryResult{
		3: {Name: "pve-c", Err: errors.New("c down")},
		1: {Name: "pve-a", Err: errors.New("a down")},
		2: {Name: "pve-b", Err: errors.New("b down")},
	}
	_, warnings := mergeVMListItems(nil, nodes)
	if len(warnings) != 3 {
		t.Fatalf("warnings = %+v, want 3", warnings)
	}
	if warnings[0].Node != "pve-a" || warnings[1].Node != "pve-b" || warnings[2].Node != "pve-c" {
		t.Fatalf("warnings = %+v, want sorted by node name", warnings)
	}
	if len(warnings) == 0 {
		t.Fatalf("warnings must be a non-nil empty-capable slice, got %d", len(warnings))
	}
}

// TestMergeVMListItemsUnknownNode 覆盖禁用/未知节点分支：节点不在被查询
// 启用节点之列的本地 VM 会被省略并产生警告，语义与节点查询失败相同。
func TestMergeVMListItemsUnknownNode(t *testing.T) {
	local := []repository.VMWithIP{
		vw(1, 1, 100, "vm1"),
		vw(2, 3, 200, "vm-on-disabled-node"),
		vw(3, 4, 300, "vm-on-missing-node"),
	}
	nodes := map[int64]nodeQueryResult{
		1: {Name: "pve1", VMs: []pve.VMStatus{{VMID: 100, Status: "running"}}},
	}

	items, warnings := mergeVMListItems(local, nodes)
	if len(items) != 1 || items[0].VM.VM.ID != 1 {
		t.Fatalf("items = %+v, want only vm1 (nodes 3 and 4 dropped)", items)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %+v, want 2", warnings)
	}
	// 按节点键（id 字符串）排序："3" 在 "4" 之前。
	if warnings[0].Node != "3" || warnings[1].Node != "4" {
		t.Fatalf("warnings = %+v, want nodes 3 then 4", warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w.Error, "not among enabled nodes") {
			t.Fatalf("warning = %+v, want the disabled-node message", w)
		}
	}
}

// ---------- 服务级列表测试（任务 8.1/8.3） ----------

// TestVMServiceListVMs 驱动完整列表路径：两个区域/节点，每个节点一个 PVE
// 服务器，其中一个节点失败。合并输出必须包含可达节点的 VM（live +
// creating）以及针对死节点的警告。
func TestVMServiceListVMs(t *testing.T) {
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nodes/pve1/qemu" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data": [{"vmid": 100, "name": "vm1", "status": "running", "cpu": 0.5, "mem": 1048576, "maxmem": 2097152, "disk": 1073741824, "maxdisk": 2147483648, "uptime": 42}]}`)
	}))
	defer alive.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"errors": {"_": "pve daemon down"}}`)
	}))
	defer dead.Close()
	clients := map[string]*httptest.Server{"h1": alive, "h2": dead}

	zoneRepo := &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
		{ID: 2, ZoneID: 1, Name: "pve2", Host: "h2", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	vmRepo := &fakeVMRepository{vms: []repository.VMWithIP{
		vw(1, 1, 100, "vm1"),
		vw(2, 1, 0, "vm-creating"),
		vw(3, 2, 200, "vm-on-dead-node"),
	}}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, zoneRepo, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(clients[host].URL), pve.WithHTTPClient(clients[host].Client()), pve.WithTimeout(5*time.Second))
	}

	items, warnings, total, err := svc.ListVMs(context.Background(), 25, 0)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3 (all local VMs)", total)
	}
	if len(items) != 2 || items[0].VM.VM.ID != 1 || items[1].VM.VM.ID != 2 {
		t.Fatalf("items = %+v, want vm1 (merged) and vm-creating from pve1", items)
	}
	if items[0].Status != "running" || items[0].Live == nil {
		t.Fatalf("vm1 = %+v, want merged live status", items[0])
	}
	if items[1].Status != model.VMStateCreating || items[1].Live != nil {
		t.Fatalf("vm-creating = %+v, want creating without live metrics", items[1])
	}
	if len(warnings) != 1 || warnings[0].Node != "pve2" {
		t.Fatalf("warnings = %+v, want the pve2 failure", warnings)
	}
	if !strings.Contains(warnings[0].Error, "pve daemon down") {
		t.Fatalf("warning error = %q, want the PVE message", warnings[0].Error)
	}
}

// TestVMServiceListVMsUsesPveName 验证节点 PveName 非空时列表调用使用
// PveName 作为 PVE 请求路径（任务 4.3）：业务名 pve1、集群名 aeoliancloud
// 的节点请求 GET /nodes/aeoliancloud/qemu，业务名绝不出现。
func TestVMServiceListVMsUsesPveName(t *testing.T) {
	var paths []string
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/nodes/aeoliancloud/qemu" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data": [{"vmid": 100, "name": "vm1", "status": "running", "cpu": 0.5, "mem": 1048576, "maxmem": 2097152, "disk": 1073741824, "maxdisk": 2147483648, "uptime": 42}]}`)
	}))
	defer alive.Close()

	zoneRepo := &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", PveName: "aeoliancloud", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	vmRepo := &fakeVMRepository{vms: []repository.VMWithIP{vw(1, 1, 100, "vm1")}}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, zoneRepo, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(alive.URL), pve.WithHTTPClient(alive.Client()), pve.WithTimeout(5*time.Second))
	}

	items, warnings, total, err := svc.ListVMs(context.Background(), 25, 0)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Status != "running" || items[0].Live == nil {
		t.Fatalf("items = %+v total = %d, want the merged running vm", items, total)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	if len(paths) != 1 || paths[0] != "/nodes/aeoliancloud/qemu" {
		t.Fatalf("paths = %v, want exactly /nodes/aeoliancloud/qemu", paths)
	}
}

// TestVMServiceListVMsRepoErrors 固定列表的硬失败路径：区域/启用节点/本地
// 元数据读取是前置条件，因此它们的错误会让整个请求失败，而不是降级为警告
// （警告仅保留给每节点的 PVE 失败，任务 8.3）。
func TestVMServiceListVMsRepoErrors(t *testing.T) {
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, &fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})

	svc.zoneRepo = &fakeVMZoneRepository{err: errors.New("zone db down")}
	if _, _, _, err := svc.ListVMs(context.Background(), 25, 0); err == nil {
		t.Fatal("ListVMs with a failing zone repo: want an error")
	}

	svc.zoneRepo = &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	svc.nodeRepo = &fakeVMNodeRepository{err: errors.New("node db down")}
	if _, _, _, err := svc.ListVMs(context.Background(), 25, 0); err == nil {
		t.Fatal("ListVMs with a failing node repo: want an error")
	}

	svc.nodeRepo = &fakeVMNodeRepository{}
	svc.vmRepo = &fakeVMRepository{listErr: errors.New("vm db down")}
	if _, _, _, err := svc.ListVMs(context.Background(), 25, 0); err == nil {
		t.Fatal("ListVMs with a failing vm repo: want an error")
	}
}

// TestVMServiceListVMsPagination 验证 GET /vms 的分页契约：limit/offset
// 选择本地元数据行的页（PVE 合并只看到该页），total 始终是本地 VM 总数，
// 即使某节点宕机且其 VM 从该页被丢弃。
func TestVMServiceListVMsPagination(t *testing.T) {
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nodes/pve1/qemu" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data": [{"vmid": 100, "name": "vm1", "status": "running", "cpu": 0.5, "mem": 1048576, "maxmem": 2097152, "disk": 1073741824, "maxdisk": 2147483648, "uptime": 42}]}`)
	}))
	defer alive.Close()
	clients := map[string]*httptest.Server{"h1": alive}

	zoneRepo := &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	vmRepo := &fakeVMRepository{vms: []repository.VMWithIP{
		vw(1, 1, 100, "vm1"),
		vw(2, 1, 0, "vm-creating"),
		vw(3, 1, 200, "vm3"),
	}}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, zoneRepo, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(clients[host].URL), pve.WithHTTPClient(clients[host].Client()), pve.WithTimeout(5*time.Second))
	}

	// 第 1 页（offset 0，limit 2）：按 id 顺序的前两行本地记录。
	items, warnings, total, err := svc.ListVMs(context.Background(), 2, 0)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(items) != 2 || items[0].VM.VM.ID != 1 || items[1].VM.VM.ID != 2 {
		t.Fatalf("items = %+v, want local rows 1 and 2", items)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}

	// 第 2 页（offset 2，limit 2）：最后一行，实时合并。
	items, _, total, err = svc.ListVMs(context.Background(), 2, 2)
	if err != nil {
		t.Fatalf("ListVMs page 2: %v", err)
	}
	if total != 3 {
		t.Fatalf("page 2 total = %d, want 3", total)
	}
	if len(items) != 1 || items[0].VM.VM.ID != 3 {
		t.Fatalf("items = %+v, want local row 3", items)
	}

	// offset 越界：空页，total 不变。
	items, _, total, err = svc.ListVMs(context.Background(), 2, 10)
	if err != nil {
		t.Fatalf("ListVMs past the end: %v", err)
	}
	if len(items) != 0 || total != 3 {
		t.Fatalf("items = %+v total = %d, want empty page with total 3", items, total)
	}
}

// ---------- 详情测试（任务 8.2/8.3） ----------

// TestGetVMDetailStatuses 覆盖本地分支：not_found、creating（pve_vmid=0）
// 和 failed（provision_error）——它们都不接触 PVE。
func TestGetVMDetailStatuses(t *testing.T) {
	ts := noCallServer(t)
	defer ts.Close()

	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	if _, err := svc.GetVM(context.Background(), 404); !isKind(err, KindNotFound) {
		t.Fatalf("missing vm err = %v, want KindNotFound", err)
	}

	creating := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 0}, IP: "10.0.0.5"}
	svc2 := newVMService(t, &fakeVMRepository{get: creating}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc2.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	item, err := svc2.GetVM(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetVM (creating): %v", err)
	}
	if item.Status != model.VMStateCreating || item.Live != nil {
		t.Fatalf("item = %+v, want creating without live metrics", item)
	}

	failed := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 0, ProvisionError: "create failed: no space"}}
	svc3 := newVMService(t, &fakeVMRepository{get: failed}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc3.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	item, err = svc3.GetVM(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetVM (failed): %v", err)
	}
	if item.Status != model.VMStateFailed || item.Live != nil {
		t.Fatalf("item = %+v, want failed without live metrics", item)
	}
	if item.VM.VM.ProvisionError != "create failed: no space" {
		t.Fatalf("provision error = %q, want it carried", item.VM.VM.ProvisionError)
	}
}

// TestGetVMDetailMergesLive 用假节点服务器驱动 PVE 分支：找到 -> 合并实时
// 状态；不存在 -> creating（设计 D5）；节点失败 -> node_unavailable（任务
// 8.3，绝不是伪造的 creating）。
func TestGetVMDetailMergesLive(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [{"vmid": 100, "name": "vm1", "status": "running", "cpu": 0.75, "mem": 2048, "maxmem": 4096, "disk": 512, "maxdisk": 1024, "uptime": 99}]}`)
	}))
	defer ok.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"errors": {"_": "bad gateway"}}`)
	}))
	defer down.Close()

	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
		{ID: 2, ZoneID: 1, Name: "pve2", Host: "h2", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
	}}

	newClient := func(srv *httptest.Server) func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient("x", apiUser, apiTokenSecret,
				pve.WithBaseURL(srv.URL), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second))
		}
	}

	// PVE 可达、VM 存在 -> 合并实时状态。
	vm := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 100, CPU: 2, MemMB: 2048, DiskGB: 10}, IP: "10.0.0.5"}
	svc := newVMService(t, &fakeVMRepository{get: vm}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = newClient(ok)
	item, err := svc.GetVM(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if item.Status != "running" || item.Live == nil || item.Live.CPUUsage != 0.75 || item.Live.Uptime != 99 {
		t.Fatalf("item = %+v, want merged live status", item)
	}
	if item.VM.VM.CPU != 2 || item.VM.VM.MemMB != 2048 || item.VM.VM.DiskGB != 10 {
		t.Fatalf("spec = %+v, want the local DB values", item.VM.VM)
	}

	// PVE 可达、VM 不存在 -> creating（D5），无错误。
	gone := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 500}}
	svc2 := newVMService(t, &fakeVMRepository{get: gone}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc2.newClient = newClient(ok)
	item, err = svc2.GetVM(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetVM (absent): %v", err)
	}
	if item.Status != model.VMStateCreating || item.Live != nil {
		t.Fatalf("item = %+v, want creating", item)
	}

	// 节点失败 -> node_unavailable（8.3），绝不伪装成 creating。
	vm2 := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 2, PVEVmid: 100}}
	svc3 := newVMService(t, &fakeVMRepository{get: vm2}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc3.newClient = newClient(down)
	_, err = svc3.GetVM(context.Background(), 1)
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}
	if !strings.Contains(err.Error(), "pve2") || !strings.Contains(err.Error(), "bad gateway") {
		t.Fatalf("err = %q, want the node name and the sanitized reason", err)
	}
}

// TestGetVMDetailGetNodeFails 覆盖详情路径的节点查找分支：缺失的节点行
// （与 vms 行相互独立地被禁用/移除）是 node_unavailable——绝不是伪造的
// creating——其他任何仓库失败保持普通错误。
func TestGetVMDetailGetNodeFails(t *testing.T) {
	ts := noCallServer(t)
	defer ts.Close()
	newClient := func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	// 节点行缺失（pgx.ErrNoRows）-> node_unavailable。
	vm := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 99, PVEVmid: 100}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, Name: "pve1"}}}
	svc := newVMService(t, &fakeVMRepository{get: vm}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = newClient
	_, err := svc.GetVM(context.Background(), 1)
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable for a missing node row", err)
	}

	// 其他数据库错误 -> 普通错误（由处理器呈现为通用的 500）。
	vm2 := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 100}}
	nodeRepo2 := &fakeVMNodeRepository{err: errors.New("node db down")}
	svc2 := newVMService(t, &fakeVMRepository{get: vm2}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo2, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc2.newClient = newClient
	_, err = svc2.GetVM(context.Background(), 1)
	if err == nil || isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want a plain error, not node_unavailable", err)
	}
}
