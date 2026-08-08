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
			CPU: 2, MemMB: 2048, DiskGB: 10, Source: model.VMSourceSparkCreated},
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

	items, warnings := mergeVMListItems(local, nodes, nil)
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

// TestMergeVMListItemsStatuses 覆盖任务 8.1 + 全部 PVE 虚拟机可见的完整
// 状态矩阵：creating（pve_vmid=0）、failed（provision_error）、合并的实时
// 状态、VM 从可达节点消失（设计 D5 -> creating），以及仅存在于 PVE 的 VM
// 作为 external 条目并入（设计 D1/D2/D3）。结果按 (node_id, pve_vmid) 排序。
func TestMergeVMListItemsStatuses(t *testing.T) {
	local := []repository.VMWithIP{
		vw(1, 1, 0, "creating-vm"), // 创建中
		{VM: model.VM{ID: 2, UUID: "u", Name: "failed-vm", ZoneID: 1, NodeID: 1,
			CPU: 2, MemMB: 2048, DiskGB: 10, ProvisionError: "create (vmid=100) failed: no space", Source: model.VMSourceSparkCreated}, IP: "10.0.0.6"},
		vw(3, 1, 100, "live-vm"), // 与 PVE 状态合并
		vw(4, 1, 101, "gone-vm"), // PVE 可达但 VM 已消失
	}
	nodes := map[int64]nodeQueryResult{
		// 999 是仅存在于 PVE 的 VM：它作为 external 条目并入。
		1: {Name: "pve1", ZoneID: 1, VMs: []pve.VMStatus{
			{VMID: 100, Name: "live-vm", Status: "running", CPU: 0.25, Cpus: 2,
				Mem: 1073741824, MaxMem: 2147483648, Disk: 5368709120, MaxDisk: 10737418240, Uptime: 12345},
			{VMID: 999, Name: "orphan", Status: "stopped"},
		}},
	}

	items, warnings := mergeVMListItems(local, nodes, nil)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	if len(items) != 5 {
		t.Fatalf("items = %+v, want 5 (orphan 999 merged as external)", items)
	}

	// 排序：按 (node_id, pve_vmid) —— (1,0) creating、(1,0) failed、(1,100)
	// live、(1,101) gone、(1,999) external。
	creating := items[0]
	if creating.Status != model.VMStateCreating || creating.Live != nil || creating.ExternalID != "" {
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
	// external 条目（设计 D2/D3）：合成 id、source=external、规格取摘要值。
	external := items[4]
	if external.ExternalID != "ext-1-999" || external.Status != "stopped" || external.Live == nil {
		t.Fatalf("external item = %+v, want ext-1-999 / stopped with live metrics", external)
	}
	if external.VM.VM.Source != model.VMSourceExternal || external.VM.VM.UUID != "" ||
		external.VM.VM.ID != 0 || external.VM.VM.Name != "orphan" || external.VM.VM.NodeID != 1 ||
		external.VM.VM.PVEVmid != 999 || external.VM.VM.ZoneID != 1 {
		t.Fatalf("external item vm = %+v, want synthesized entry", external.VM.VM)
	}
}

// TestMergeVMListItemsSources 覆盖三类条目的来源标识（spec：列表返回来源
// 标识）：本地 spark_created / claimed 行与 PVE 合并后保留各自 source，
// PVE-only 条目为 external；PVE 模板不并入。
func TestMergeVMListItemsSources(t *testing.T) {
	local := []repository.VMWithIP{
		{VM: model.VM{ID: 1, Name: "spark-vm", NodeID: 1, PVEVmid: 100, Source: model.VMSourceSparkCreated}},
		{VM: model.VM{ID: 2, Name: "claimed-vm", NodeID: 1, PVEVmid: 101, Source: model.VMSourceClaimed}},
	}
	nodes := map[int64]nodeQueryResult{
		1: {Name: "pve1", ZoneID: 1, VMs: []pve.VMStatus{
			{VMID: 100, Name: "spark-vm", Status: "running"},
			{VMID: 101, Name: "claimed-vm", Status: "stopped"},
			{VMID: 200, Name: "ext-vm", Status: "running", Cpus: 4, MaxMem: 8589934592, MaxDisk: 107374182400},
			{VMID: 300, Name: "ubuntu-template", Status: "stopped", Template: 1},
		}},
	}

	items, warnings := mergeVMListItems(local, nodes, nil)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	// 模板 300 不并入；三类条目齐全。
	if len(items) != 3 {
		t.Fatalf("items = %+v, want 3 (template 300 excluded)", items)
	}
	if items[0].VM.VM.Source != model.VMSourceSparkCreated || items[0].VM.VM.ID != 1 {
		t.Fatalf("item 0 = %+v, want spark_created", items[0])
	}
	if items[1].VM.VM.Source != model.VMSourceClaimed || items[1].VM.VM.ID != 2 {
		t.Fatalf("item 1 = %+v, want claimed", items[1])
	}
	ext := items[2]
	if ext.ExternalID != "ext-1-200" || ext.VM.VM.Source != model.VMSourceExternal {
		t.Fatalf("item 2 = %+v, want external ext-1-200", ext)
	}
	// external 规格取 PVE 摘要：4 核 / 8192 MiB / 100 GiB（字节换算）。
	if ext.VM.VM.CPU != 4 || ext.VM.VM.MemMB != 8192 || ext.VM.VM.DiskGB != 100 {
		t.Fatalf("external spec = %+v, want 4/8192/100 from the PVE summary", ext.VM.VM)
	}
}

// TestMergeVMListItemsSortDeterministic 固定翻页稳定性：无论本地行输入
// 顺序与节点 map 迭代顺序如何，输出都按 (node_id, pve_vmid) 升序。
func TestMergeVMListItemsSortDeterministic(t *testing.T) {
	local := []repository.VMWithIP{
		vw(1, 2, 300, "n2-vm3"),
		vw(2, 1, 200, "n1-vm2"),
		vw(3, 1, 0, "n1-creating"),
	}
	nodes := map[int64]nodeQueryResult{
		2: {Name: "pve2", ZoneID: 1, VMs: []pve.VMStatus{{VMID: 300, Name: "n2-vm3", Status: "running"}, {VMID: 400, Name: "n2-ext", Status: "stopped"}}},
		1: {Name: "pve1", ZoneID: 1, VMs: []pve.VMStatus{{VMID: 100, Name: "n1-ext", Status: "running"}, {VMID: 200, Name: "n1-vm2", Status: "stopped"}}},
	}

	items, _ := mergeVMListItems(local, nodes, nil)
	want := [][2]int64{{1, 0}, {1, 100}, {1, 200}, {2, 300}, {2, 400}}
	if len(items) != len(want) {
		t.Fatalf("items = %+v, want %d entries", items, len(want))
	}
	for i, pair := range want {
		item := items[i]
		if item.VM.VM.NodeID != pair[0] || item.VM.VM.PVEVmid != pair[1] {
			t.Fatalf("item %d = node %d / vmid %d, want node %d / vmid %d",
				i, item.VM.VM.NodeID, item.VM.VM.PVEVmid, pair[0], pair[1])
		}
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
	_, warnings := mergeVMListItems(nil, nodes, nil)
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
// 启用节点之列的本地 VM 会被省略并产生警告，语义与节点查询失败相同。警告
// 的 Node 字段输出节点名（nodeNames 映射），与 PVE 失败分支的对外形态一致，
// 绝不输出内部节点 id（reviewer-4）。
func TestMergeVMListItemsUnknownNode(t *testing.T) {
	local := []repository.VMWithIP{
		vw(1, 1, 100, "vm1"),
		vw(2, 3, 200, "vm-on-disabled-node"),
		vw(3, 4, 300, "vm-on-missing-node"),
	}
	nodes := map[int64]nodeQueryResult{
		1: {Name: "pve1", VMs: []pve.VMStatus{{VMID: 100, Status: "running"}}},
	}
	// 节点名映射：3 是已删除/禁用的节点（仍可查到名字），4 已从 pve_nodes
	// 移除（查询返回空，兜底为 id 字符串）。
	nodeNames := map[int64]string{1: "pve1", 3: "pve3"}

	items, warnings := mergeVMListItems(local, nodes, nodeNames)
	if len(items) != 1 || items[0].VM.VM.ID != 1 {
		t.Fatalf("items = %+v, want only vm1 (nodes 3 and 4 dropped)", items)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %+v, want 2", warnings)
	}
	// 按节点名排序：ASCII 上数字 "4"（48）小于字母 "p"（112），因此兜底的
	// id 字符串 "4" 在节点名 pve3 之前。
	if warnings[0].Node != "4" || warnings[1].Node != "pve3" {
		t.Fatalf("warnings = %+v, want node name pve3 and fallback id 4", warnings)
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

	items, warnings, total, err := svc.ListVMs(context.Background(), adminIdentity(), 25, 0)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	// 合并后总数 = 2（pve2 故障，其 VM 与 local 行均被省略）。
	if total != 2 {
		t.Fatalf("total = %d, want 2 (merged count, dead node dropped)", total)
	}
	// 按 (node_id, pve_vmid) 排序：pve1 的 creating(id2, vmid0) 在前，
	// vm1(id1, vmid100) 在后。
	if len(items) != 2 || items[0].VM.VM.ID != 2 || items[1].VM.VM.ID != 1 {
		t.Fatalf("items = %+v, want vm-creating then vm1 from pve1", items)
	}
	if items[0].Status != model.VMStateCreating || items[0].Live != nil {
		t.Fatalf("vm-creating = %+v, want creating without live metrics", items[0])
	}
	if items[1].Status != "running" || items[1].Live == nil {
		t.Fatalf("vm1 = %+v, want merged live status", items[1])
	}
	if items[0].VM.VM.Source != model.VMSourceSparkCreated || items[1].VM.VM.Source != model.VMSourceSparkCreated {
		t.Fatalf("sources = %q / %q, want spark_created", items[0].VM.VM.Source, items[1].VM.VM.Source)
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

	items, warnings, total, err := svc.ListVMs(context.Background(), adminIdentity(), 25, 0)
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
	if _, _, _, err := svc.ListVMs(context.Background(), adminIdentity(), 25, 0); err == nil {
		t.Fatal("ListVMs with a failing zone repo: want an error")
	}

	svc.zoneRepo = &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	svc.nodeRepo = &fakeVMNodeRepository{err: errors.New("node db down")}
	if _, _, _, err := svc.ListVMs(context.Background(), adminIdentity(), 25, 0); err == nil {
		t.Fatal("ListVMs with a failing node repo: want an error")
	}

	svc.nodeRepo = &fakeVMNodeRepository{}
	svc.vmRepo = &fakeVMRepository{listErr: errors.New("vm db down")}
	if _, _, _, err := svc.ListVMs(context.Background(), adminIdentity(), 25, 0); err == nil {
		t.Fatal("ListVMs with a failing vm repo: want an error")
	}
}

// TestVMServiceListVMsPagination 验证 GET /vms 的分页契约（设计 D1）：
// limit/offset 作用于合并排序后的完整条目（本地行 + external），total 是
// 合并后条目总数（含 external、剔除故障节点 VM）。external 条目与本地条目
// 同页混排，翻页稳定不重复。
func TestVMServiceListVMsPagination(t *testing.T) {
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nodes/pve1/qemu" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data": [
			{"vmid": 100, "name": "vm1", "status": "running", "cpu": 0.5, "mem": 1048576, "maxmem": 2097152, "disk": 1073741824, "maxdisk": 2147483648, "uptime": 42},
			{"vmid": 200, "name": "vm3", "status": "stopped", "cpus": 2, "maxmem": 4294967296, "maxdisk": 21474836480},
			{"vmid": 300, "name": "ext-orphan", "status": "running", "cpus": 1, "maxmem": 1073741824, "maxdisk": 10737418240}
		]}`)
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

	// 合并后的完整序列（按 node_id, pve_vmid）：(1,0) vm-creating、
	// (1,100) vm1、(1,200) vm3、(1,300) ext-orphan。total = 4。
	// 第 1 页（limit 2）：前两条本地条目。
	items, warnings, total, err := svc.ListVMs(context.Background(), adminIdentity(), 2, 0)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if total != 4 {
		t.Fatalf("total = %d, want 4 (merged count including external)", total)
	}
	if len(items) != 2 || items[0].VM.VM.ID != 2 || items[1].VM.VM.ID != 1 {
		t.Fatalf("items = %+v, want vm-creating then vm1", items)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}

	// 第 2 页（offset 2）：vm3 与 external 条目 ext-1-300 混排。
	items, _, total, err = svc.ListVMs(context.Background(), adminIdentity(), 2, 2)
	if err != nil {
		t.Fatalf("ListVMs page 2: %v", err)
	}
	if total != 4 {
		t.Fatalf("page 2 total = %d, want 4", total)
	}
	if len(items) != 2 || items[0].VM.VM.ID != 3 || items[1].ExternalID != "ext-1-300" {
		t.Fatalf("items = %+v, want vm3 then external ext-1-300", items)
	}
	if items[1].VM.VM.Source != model.VMSourceExternal {
		t.Fatalf("external source = %q, want external", items[1].VM.VM.Source)
	}
	// external 规格取摘要值：1 核 / 1024 MiB / 10 GiB。
	if items[1].VM.VM.CPU != 1 || items[1].VM.VM.MemMB != 1024 || items[1].VM.VM.DiskGB != 10 {
		t.Fatalf("external spec = %+v, want 1/1024/10 from the PVE summary", items[1].VM.VM)
	}

	// offset 越界：空页，total 不变。
	items, _, total, err = svc.ListVMs(context.Background(), adminIdentity(), 2, 10)
	if err != nil {
		t.Fatalf("ListVMs past the end: %v", err)
	}
	if len(items) != 0 || total != 4 {
		t.Fatalf("items = %+v total = %d, want empty page with total 4", items, total)
	}

	// 翻页稳定性：两页拼接恰好是完整序列（无重复/无遗漏）。
	page1, _, _, err := svc.ListVMs(context.Background(), adminIdentity(), 2, 0)
	if err != nil {
		t.Fatalf("ListVMs page 1 again: %v", err)
	}
	page2, _, _, err := svc.ListVMs(context.Background(), adminIdentity(), 2, 2)
	if err != nil {
		t.Fatalf("ListVMs page 2 again: %v", err)
	}
	joined := append(page1, page2...)
	for i, pair := range [][2]int64{{1, 0}, {1, 100}, {1, 200}, {1, 300}} {
		if joined[i].VM.VM.NodeID != pair[0] || joined[i].VM.VM.PVEVmid != pair[1] {
			t.Fatalf("joined item %d = node %d / vmid %d, want node %d / vmid %d",
				i, joined[i].VM.VM.NodeID, joined[i].VM.VM.PVEVmid, pair[0], pair[1])
		}
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

	if _, err := svc.GetVM(context.Background(), "404", adminIdentity()); !isKind(err, KindNotFound) {
		t.Fatalf("missing vm err = %v, want KindNotFound", err)
	}

	creating := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 0}, IP: "10.0.0.5"}
	svc2 := newVMService(t, &fakeVMRepository{get: creating}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc2.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	item, err := svc2.GetVM(context.Background(), "1", adminIdentity())
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
	item, err = svc3.GetVM(context.Background(), "1", adminIdentity())
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
	item, err := svc.GetVM(context.Background(), "1", adminIdentity())
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
	item, err = svc2.GetVM(context.Background(), "1", adminIdentity())
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
	_, err = svc3.GetVM(context.Background(), "1", adminIdentity())
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
	_, err := svc.GetVM(context.Background(), "1", adminIdentity())
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable for a missing node row", err)
	}

	// 其他数据库错误 -> 普通错误（由处理器呈现为通用的 500）。
	vm2 := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 100}}
	nodeRepo2 := &fakeVMNodeRepository{err: errors.New("node db down")}
	svc2 := newVMService(t, &fakeVMRepository{get: vm2}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo2, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc2.newClient = newClient
	_, err = svc2.GetVM(context.Background(), "1", adminIdentity())
	if err == nil || isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want a plain error, not node_unavailable", err)
	}
}

// ---------- external 详情测试（vm-page-experience，设计 D6） ----------

// TestGetExternalVMDetail 覆盖 ext- 合成标识的详情成功分支：节点 PVE 实时
// 状态读取、externalVMListItem 字段形态（uuid/ip/created_at/updated_at
// 为空、规格取 PVE 摘要）与实时指标透传。
func TestGetExternalVMDetail(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [{"vmid": 300, "name": "ext-vm", "status": "running", "cpus": 2, "maxmem": 4294967296, "maxdisk": 21474836480, "cpu": 0.75, "mem": 2147483648, "disk": 1073741824, "uptime": 99}]}`)
	}))
	defer ok.Close()

	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 5, Name: "pve1", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ok.URL), pve.WithHTTPClient(ok.Client()), pve.WithTimeout(5*time.Second))
	}

	item, err := svc.GetVM(context.Background(), "ext-1-300", adminIdentity())
	if err != nil {
		t.Fatalf("GetVM ext-1-300: %v", err)
	}
	if item.ExternalID != "ext-1-300" || item.Status != "running" {
		t.Fatalf("item = %+v, want ExternalID ext-1-300 with running status", item)
	}
	// 本地行字段保持零值：uuid/ip/created_at/updated_at 空、无镜像/存储绑定。
	vm := item.VM.VM
	if vm.UUID != "" || vm.Name != "ext-vm" || vm.Source != model.VMSourceExternal {
		t.Fatalf("vm fields = %+v, want external source with name from PVE summary", vm)
	}
	if vm.ZoneID != 5 || vm.NodeID != 1 || vm.PVEVmid != 300 {
		t.Fatalf("vm placement = zone %d node %d vmid %d, want zone 5 node 1 vmid 300", vm.ZoneID, vm.NodeID, vm.PVEVmid)
	}
	// 规格取 PVE 摘要：2 核 / 4096 MiB / 20 GiB（字节换算）。
	if vm.CPU != 2 || vm.MemMB != 4096 || vm.DiskGB != 20 {
		t.Fatalf("spec = %+v, want 2/4096/20 from the PVE summary", vm)
	}
	// 实时指标透传。
	if item.Live == nil || item.Live.CPUUsage != 0.75 || item.Live.Mem != 2147483648 ||
		item.Live.MaxMem != 4294967296 || item.Live.Uptime != 99 {
		t.Fatalf("live = %+v, want the pass-through metrics", item.Live)
	}
}

// TestGetExternalVMNodeMissing 覆盖 ext- 详情的节点行缺失分支：与本地详情
// 路径相同的 node_unavailable（503）语义，绝不伪造状态。
func TestGetExternalVMNodeMissing(t *testing.T) {
	ts := noCallServer(t)
	defer ts.Close()
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, Name: "pve1"}}}
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	_, err := svc.GetVM(context.Background(), "ext-99-300", adminIdentity())
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable for a missing node row", err)
	}
}

// TestGetExternalVMNodeUnreachable 覆盖 ext- 详情的节点调用失败分支：
// node_unavailable（503），错误消息带节点名与脱敏原因，绝不伪装状态。
func TestGetExternalVMNodeUnreachable(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"errors": {"_": "bad gateway"}}`)
	}))
	defer down.Close()
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 5, Name: "pve1", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(down.URL), pve.WithHTTPClient(down.Client()), pve.WithTimeout(5*time.Second))
	}
	_, err := svc.GetVM(context.Background(), "ext-1-300", adminIdentity())
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}
	if !strings.Contains(err.Error(), "pve1") || !strings.Contains(err.Error(), "bad gateway") {
		t.Fatalf("err = %q, want the node name and the sanitized reason", err)
	}
}

// TestGetExternalVMRemoved 覆盖 ext- 详情的两个 404 场景：节点可达但 VM 已
// 从 PVE 移除 -> vm_not_found_on_node；PVE 模板同样以 vm_not_found_on_node
// 呈现（模板不是可查看的运行实体）。
func TestGetExternalVMRemoved(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [{"vmid": 300, "name": "vm1", "status": "running", "cpus": 1, "maxmem": 1073741824, "maxdisk": 10737418240}]}`)
	}))
	defer ok.Close()
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 5, Name: "pve1", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	newClient := func(srv *httptest.Server) func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient("x", apiUser, apiTokenSecret,
				pve.WithBaseURL(srv.URL), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second))
		}
	}
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = newClient(ok)

	// VM 已从 PVE 移除（vmid 不在列表中）。
	_, err := svc.GetVM(context.Background(), "ext-1-999", adminIdentity())
	if !isKind(err, KindVMNotFoundOnNode) {
		t.Fatalf("err = %v, want KindVMNotFoundOnNode for a removed vm", err)
	}

	// PVE 模板：同样 404，不对外提供详情。
	tpl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [{"vmid": 400, "name": "base-template", "status": "stopped", "cpus": 1, "maxmem": 1073741824, "maxdisk": 10737418240, "template": 1}]}`)
	}))
	defer tpl.Close()
	svc2 := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc2.newClient = newClient(tpl)
	_, err = svc2.GetVM(context.Background(), "ext-1-400", adminIdentity())
	if !isKind(err, KindVMNotFoundOnNode) {
		t.Fatalf("err = %v, want KindVMNotFoundOnNode for a pve template", err)
	}
}

// TestGetExternalVMManagedRoutesLocal 覆盖 G1：ext- 标识指向的
// (nodeID, pve_vmid) 已有本地托管行时，详情改走本地形态路径返回——数字
// 行 id、uuid/ip 等本地字段齐全、规格取本地 DB 值（而非 PVE 摘要），与
// 列表差集与 resolveVMTarget 的生命周期路由语义一致。
func TestGetExternalVMManagedRoutesLocal(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [{"vmid": 300, "name": "pve-name", "status": "running", "cpus": 2, "maxmem": 1073741824, "maxdisk": 10737418240, "cpu": 0.75, "mem": 536870912, "uptime": 99}]}`)
	}))
	defer ok.Close()

	managed := &repository.VMWithIP{
		VM: model.VM{ID: 7, UUID: "u-7", Name: "vm7", NodeID: 1, PVEVmid: 300,
			CPU: 4, MemMB: 4096, DiskGB: 30, Source: model.VMSourceClaimed},
		IP: "10.0.0.7",
	}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 5, Name: "pve1", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	// getByNodeVMID 命中本地托管行（ID 7），GetVM 按行 id 返回本地形态。
	vmRepo := &fakeVMRepository{getByNodeVMID: &managed.VM, get: managed}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ok.URL), pve.WithHTTPClient(ok.Client()), pve.WithTimeout(5*time.Second))
	}

	item, err := svc.GetVM(context.Background(), "ext-1-300", adminIdentity())
	if err != nil {
		t.Fatalf("GetVM ext-1-300 (managed): %v", err)
	}
	// 本地形态：无 ExternalID，数字行 id 与本地字段（uuid/ip/source）齐全。
	if item.ExternalID != "" {
		t.Fatalf("ExternalID = %q, want empty for a managed vm", item.ExternalID)
	}
	if item.VM.VM.ID != 7 || item.VM.VM.UUID != "u-7" || item.VM.VM.Name != "vm7" ||
		item.VM.VM.Source != model.VMSourceClaimed || item.VM.IP != "10.0.0.7" {
		t.Fatalf("vm = %+v, want the local row values (id 7 / uuid u-7 / claimed / ip)", item.VM)
	}
	// 规格取本地 DB 值（4/4096/30）而非 PVE 摘要（cpus 2）。
	if item.VM.VM.CPU != 4 || item.VM.VM.MemMB != 4096 || item.VM.VM.DiskGB != 30 {
		t.Fatalf("spec = %+v, want the local row spec 4/4096/30", item.VM.VM)
	}
	// 实时状态照常从 PVE 合并。
	if item.Status != "running" || item.Live == nil || item.Live.CPUUsage != 0.75 {
		t.Fatalf("live = %+v, want running with pass-through metrics", item)
	}
}

// TestGetExternalVMNodeDisabled 覆盖 S1：ext- 详情的节点已被禁用时返回
// node_unavailable（503），与 resolveVMTarget 的先例一致，绝不报告状态。
func TestGetExternalVMNodeDisabled(t *testing.T) {
	ts := noCallServer(t)
	defer ts.Close()
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 5, Name: "pve1", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: false},
	}}
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	_, err := svc.GetVM(context.Background(), "ext-1-300", adminIdentity())
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable for a disabled node", err)
	}
}

// TestGetVMInvalidRefs 覆盖 ext- 合成标识与数字 id 的格式校验边界：前导零、
// 0 段、缺段、多余段、负数、非数字、int64 溢出段、非 ASCII 数字等一律
// KindInvalidVMRef（400 invalid_vm_id）。
func TestGetVMInvalidRefs(t *testing.T) {
	ts := noCallServer(t)
	defer ts.Close()
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	cases := []string{
		"ext-0-100",                    // nodeID 为 0
		"ext-1-0",                      // vmid 为 0
		"ext-01-100",                   // nodeID 前导零
		"ext-1-01",                     // vmid 前导零
		"ext-1",                        // 缺段
		"ext-1-2-3",                    // 多余段
		"ext--1-2",                     // 负数（nodeID 段为空）
		"ext-1--2",                     // 负数（vmid 段为空）
		"ext-abc-100",                  // 非数字段
		"ext-",                         // 空 rest
		"ext-9999999999999999999999-2", // nodeID 段超出 int64 范围（ParseInt 溢出）
		"ext-1-9999999999999999999999", // vmid 段超出 int64 范围（ParseInt 溢出）
		"ext-１-２",                      // 全角数字（非 ASCII，正则不匹配）
		"-1",                           // 负数数字 id
		"0",                            // 0 数字 id
		"007",                          // 数字 id 前导零
		"9223372036854775808",          // 数字 id 超出 int64 范围（MaxInt64+1，ParseInt 溢出）
		"abc",                          // 非数字非 ext-
	}
	for _, id := range cases {
		_, err := svc.GetVM(context.Background(), id, adminIdentity())
		if !isKind(err, KindInvalidVMRef) {
			t.Fatalf("GetVM(%q) err = %v, want KindInvalidVMRef", id, err)
		}
	}
}
