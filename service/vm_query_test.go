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

// ---------- pure merge tests (task 8.1/8.3) ----------

func vw(id, nodeID, vmid int64, name string) repository.VMWithIP {
	return repository.VMWithIP{
		VM: model.VM{ID: id, UUID: "uuid", Name: name, ZoneID: 1, NodeID: nodeID, PVEVmid: vmid,
			CPU: 2, MemMB: 2048, DiskGB: 10},
		IP: "10.0.0.5",
	}
}

// TestMergeVMListItemsDownNode verifies task 8.3: a failed node contributes a
// warning and none of its VMs; the other node's VMs still appear.
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

// TestMergeVMListItemsStatuses covers the full status matrix of task 8.1:
// creating (pve_vmid=0), failed (provision_error), merged live status,
// PVE-only skipped, and VM gone from a reachable node (design D5 -> creating).
func TestMergeVMListItemsStatuses(t *testing.T) {
	local := []repository.VMWithIP{
		vw(1, 1, 0, "creating-vm"), // creating
		{VM: model.VM{ID: 2, UUID: "u", Name: "failed-vm", ZoneID: 1, NodeID: 1,
			CPU: 2, MemMB: 2048, DiskGB: 10, ProvisionError: "create (vmid=100) failed: no space"}, IP: "10.0.0.6"},
		vw(3, 1, 100, "live-vm"), // merged with the PVE status
		vw(4, 1, 101, "gone-vm"), // PVE reachable but the VM vanished
	}
	nodes := map[int64]nodeQueryResult{
		// 999 is a PVE-only VM: it must be skipped (no local metadata).
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
	// Spec sizes stay local (design D1): merged item keeps the DB values.
	if live.VM.VM.CPU != 2 || live.VM.VM.MemMB != 2048 || live.VM.VM.DiskGB != 10 {
		t.Fatalf("merged spec = %+v, want the local DB values", live.VM.VM)
	}
	gone := items[3]
	if gone.Status != model.VMStateCreating || gone.Live != nil {
		t.Fatalf("gone item = %+v, want creating (design D5: PVE 不存在)", gone)
	}
}

// TestMergeVMListItemsWarningOrder checks deterministic output: warnings are
// sorted by node name regardless of map iteration order.
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

// TestMergeVMListItemsUnknownNode covers the disabled/unknown-node branch: a
// local VM whose node is not among the queried enabled nodes is omitted and
// produces a warning, with the same semantics as a failed node query.
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
	// Sorted by node key (the id string): "3" before "4".
	if warnings[0].Node != "3" || warnings[1].Node != "4" {
		t.Fatalf("warnings = %+v, want nodes 3 then 4", warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w.Error, "not among enabled nodes") {
			t.Fatalf("warning = %+v, want the disabled-node message", w)
		}
	}
}

// ---------- service-level list test (task 8.1/8.3) ----------

// TestVMServiceListVMs drives the full list path: two zones/nodes, one PVE
// server per node, one node failing. The merged output must contain the
// reachable node's VMs (live + creating) and a warning for the dead node.
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
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(clients[host].URL), pve.WithHTTPClient(clients[host].Client()), pve.WithTimeout(5*time.Second))
	}

	items, warnings, err := svc.ListVMs(context.Background())
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
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

// TestVMServiceListVMsRepoErrors pins down the hard-failure paths of the
// list: the zone / enabled-node / local-metadata reads are prerequisites, so
// their errors fail the whole request instead of degrading into warnings
// (warnings are reserved for the per-node PVE failures, task 8.3).
func TestVMServiceListVMsRepoErrors(t *testing.T) {
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, &fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})

	svc.zoneRepo = &fakeVMZoneRepository{err: errors.New("zone db down")}
	if _, _, err := svc.ListVMs(context.Background()); err == nil {
		t.Fatal("ListVMs with a failing zone repo: want an error")
	}

	svc.zoneRepo = &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	svc.nodeRepo = &fakeVMNodeRepository{err: errors.New("node db down")}
	if _, _, err := svc.ListVMs(context.Background()); err == nil {
		t.Fatal("ListVMs with a failing node repo: want an error")
	}

	svc.nodeRepo = &fakeVMNodeRepository{}
	svc.vmRepo = &fakeVMRepository{listErr: errors.New("vm db down")}
	if _, _, err := svc.ListVMs(context.Background()); err == nil {
		t.Fatal("ListVMs with a failing vm repo: want an error")
	}
}

// ---------- detail tests (task 8.2/8.3) ----------

// TestGetVMDetailStatuses covers the local branches: not_found, creating
// (pve_vmid=0) and failed (provision_error) — none of them touches PVE.
func TestGetVMDetailStatuses(t *testing.T) {
	ts := noCallServer(t)
	defer ts.Close()

	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	if _, err := svc.GetVM(context.Background(), 404); !isKind(err, KindNotFound) {
		t.Fatalf("missing vm err = %v, want KindNotFound", err)
	}

	creating := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 0}, IP: "10.0.0.5"}
	svc2 := newVMService(t, &fakeVMRepository{get: creating}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc2.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
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
	svc3.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
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

// TestGetVMDetailMergesLive drives the PVE branches with a fake node server:
// found -> merged live status; absent -> creating (design D5); node failure
// -> node_unavailable (task 8.3, never a fake creating).
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

	newClient := func(srv *httptest.Server) func(host, apiUser, apiTokenSecret string) *pve.Client {
		return func(host, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient("x", apiUser, apiTokenSecret,
				pve.WithBaseURL(srv.URL), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second))
		}
	}

	// PVE reachable, VM present -> merged live status.
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

	// PVE reachable, VM absent -> creating (D5), no error.
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

	// Node failure -> node_unavailable (8.3), never disguised as creating.
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

// TestGetVMDetailGetNodeFails covers the node-lookup branches of the detail
// path: a missing node row (disabled/removed independently of the vms rows)
// is a node_unavailable — never a fake creating — and any other repository
// failure stays a plain error.
func TestGetVMDetailGetNodeFails(t *testing.T) {
	ts := noCallServer(t)
	defer ts.Close()
	newClient := func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	// Node row absent (pgx.ErrNoRows) -> node_unavailable.
	vm := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 99, PVEVmid: 100}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, Name: "pve1"}}}
	svc := newVMService(t, &fakeVMRepository{get: vm}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = newClient
	_, err := svc.GetVM(context.Background(), 1)
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable for a missing node row", err)
	}

	// Other DB error -> plain error (rendered as a generic 500 by the handler).
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
