package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
	"spark/repository"
)

// listVMsTimeout bounds the whole list query (DB reads plus every node's PVE
// list call). The node calls run in parallel, so the budget is the total wall
// time for all of them, not the sum of their individual latencies: the
// deadline is derived from the request context, and any node call that has
// not answered when it fires fails with a context error that becomes a
// warning — the same partial-failure semantics as an unreachable node
// (task 8.3).
const listVMsTimeout = 10 * time.Second

// LiveVMStatus is the pass-through runtime portion of a VM, read live from
// the PVE node (design D1; the DB only stores metadata). Sizes are bytes and
// CPUUsage is the fraction of the configured cores currently in use; a
// stopped VM reports zero values.
type LiveVMStatus struct {
	Status   string
	CPUUsage float64
	Mem      int64
	MaxMem   int64
	Disk     int64
	MaxDisk  int64
	Uptime   int64
}

// VMListItem is one merged row of the pass-through list/detail (task 8.1/8.2):
// the local metadata (spec values as requested at create time, design D1)
// plus the live status when the VM exists on PVE. Status is "creating" while
// the VM has no PVE counterpart (or it vanished, design D5) and "failed" when
// provisioning failed; in both cases Live is nil.
type VMListItem struct {
	VM     repository.VMWithIP
	Status string
	Live   *LiveVMStatus
}

// NodeWarning is a partial-failure notice attached to the list response: the
// node's live query failed (unreachable, TLS, auth, ...) and its VMs are
// omitted from the list (task 8.3). Error is safe to surface: the PVE client
// sanitizes its own errors (pve.NewClient redacts the API user and never
// echoes the token secret), and this package only copies them verbatim.
type NodeWarning struct {
	Node  string
	Error string
}

// nodeQueryResult carries one node's PVE list outcome for mergeVMListItems:
// the live VM list, or the failure that replaced it.
type nodeQueryResult struct {
	Name string
	VMs  []pve.VMStatus
	Err  error
}

// mergeVMListItems is the pure merge of the pass-through list (task 8.1):
// for every local VM the live status is looked up in its node's PVE list and
// merged; VMs without a PVE counterpart are reported as creating/failed; VMs
// of failed nodes are omitted and collected into warnings. VMs whose node is
// not among the queried enabled nodes (disabled or removed) are omitted and
// warned about the same way. The result keeps the local (id) order and the
// warnings are sorted by node name, so the output is deterministic. PVE-only
// VMs (present on a node, no local row) have no metadata to merge and are
// deliberately skipped: they are not managed by this service.
//
// It is a pure function of its inputs so the merge semantics are unit-testable
// without PVE or DB access.
func mergeVMListItems(local []repository.VMWithIP, nodes map[int64]nodeQueryResult) ([]VMListItem, []NodeWarning) {
	items := make([]VMListItem, 0, len(local))
	// disabled collects the warnings for local VMs whose node is not among
	// the queried enabled nodes, keyed by the node id string so they merge
	// into the same deterministic sort as the query-failure warnings.
	disabled := make(map[string]string)
	// indexes caches one vmid -> status lookup map per node, built once per
	// node in O(P) so merging every VM of that node is O(1) instead of a
	// linear scan of the PVE list on each row.
	indexes := make(map[int64]map[int64]pve.VMStatus)
	for i := range local {
		vm := local[i]
		res, ok := nodes[vm.VM.NodeID]
		if !ok {
			// Local metadata points at a node that is not among the enabled
			// nodes that were queried (disabled or removed): the VM is omitted
			// and a warning is emitted, mirroring the failed-node semantics.
			disabled[strconv.FormatInt(vm.VM.NodeID, 10)] = fmt.Sprintf("node %d not among enabled nodes", vm.VM.NodeID)
			continue
		}
		if res.Err != nil {
			// The node's live query failed: its VMs are omitted as a partial
			// failure (task 8.3); the warning is collected below.
			continue
		}
		switch {
		case vm.VM.ProvisionError != "":
			// provision_error wins over creating: the detached chain failed.
			items = append(items, VMListItem{VM: vm, Status: model.VMStateFailed})
		case vm.VM.PVEVmid == 0:
			items = append(items, VMListItem{VM: vm, Status: model.VMStateCreating})
		default:
			idx, ok := indexes[vm.VM.NodeID]
			if !ok {
				idx = make(map[int64]pve.VMStatus, len(res.VMs))
				for _, v := range res.VMs {
					idx[v.VMID] = v
				}
				indexes[vm.VM.NodeID] = idx
			}
			if st, found := idx[vm.VM.PVEVmid]; found {
				items = append(items, VMListItem{VM: vm, Status: st.Status, Live: &LiveVMStatus{
					Status: st.Status, CPUUsage: st.CPU, Mem: st.Mem, MaxMem: st.MaxMem,
					Disk: st.Disk, MaxDisk: st.MaxDisk, Uptime: st.Uptime,
				}})
				continue
			}
			// The node answers but the VM is gone from it: per design D5 the
			// state reads as not-provisioned (creating). The row keeps its
			// pve_vmid so operators can spot the inconsistency.
			items = append(items, VMListItem{VM: vm, Status: model.VMStateCreating})
		}
	}

	names := make([]string, 0, len(nodes)+len(disabled))
	errs := make(map[string]string, len(nodes)+len(disabled))
	for _, res := range nodes {
		if res.Err == nil {
			continue
		}
		errs[res.Name] = res.Err.Error()
		names = append(names, res.Name)
	}
	for id, msg := range disabled {
		errs[id] = msg
		names = append(names, id)
	}
	sort.Strings(names)
	warnings := make([]NodeWarning, 0, len(names))
	for _, n := range names {
		warnings = append(warnings, NodeWarning{Node: n, Error: errs[n]})
	}
	return items, warnings
}

// findVM returns the live status of the VM with the given vmid within the
// node's list, if present. The list merge (mergeVMListItems) uses a
// prebuilt vmid map instead; findVM serves the single-VM detail path.
func findVM(vms []pve.VMStatus, vmid int64) (pve.VMStatus, bool) {
	for _, v := range vms {
		if v.VMID == vmid {
			return v, true
		}
	}
	return pve.VMStatus{}, false
}

// ListVMs implements the pass-through list (task 8.1, design D1): the enabled
// nodes of every zone are queried with exactly one PVE list call per node
// (design D1), one page of the local metadata is read (with IPs), and both
// are merged. The whole query runs under a request-level budget (listVMsTimeout).
// The node queries run in parallel so the total latency is bounded by the
// slowest node, not by the sum of all nodes; a node whose list call fails —
// including the shared deadline firing — contributes a warning instead of its
// VMs (task 8.3) and never fails the whole request.
//
// Pagination (limit/offset) applies to the local vms metadata query: the
// SQL LIMIT/OFFSET runs on the vms table and the PVE merge only sees the
// page's rows, so the worst case is maxPageLimit rows merged per node. total
// is the total local VM count (CountVMs): the merge can drop rows of failed
// or disabled nodes, so total may exceed the item count of the page. The
// warnings logic is unchanged: only the page's local rows are considered.
func (s *VMService) ListVMs(ctx context.Context, limit, offset int) ([]VMListItem, []NodeWarning, int, error) {
	// Request-level total timeout: bounds DB reads plus all parallel node
	// calls together, so a slow or hanging node cannot stretch the request.
	// The same deadline is shared by every node goroutine below; when it
	// fires, the pending calls fail with a context error that surfaces as a
	// warning like any other node failure.
	ctx, cancel := context.WithTimeout(ctx, listVMsTimeout)
	defer cancel()

	zones, err := s.zoneRepo.ListZones(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("list vms: list zones: %w", err)
	}
	nodes := make([]model.PVENode, 0)
	for _, z := range zones {
		zn, err := s.nodeRepo.ListEnabledNodesByZone(ctx, z.ID)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("list vms: enabled nodes of zone %d: %w", z.ID, err)
		}
		nodes = append(nodes, zn...)
	}

	// One PVE list call per enabled node, parallelized: each goroutine writes
	// only its own slot of the results slice (distinct indices, no shared
	// state), so no locking is needed. nodeQueryResult values are collected
	// into a map afterwards for the merge.
	results := make([]nodeQueryResult, len(nodes))
	var wg sync.WaitGroup
	for i := range nodes {
		n := nodes[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := s.newClient(n.Host, n.APIUser, n.APITokenSecret)
			vms, err := client.ListVMs(ctx, n.Name)
			if err != nil {
				// Partial failure (task 8.3): the node's VMs are dropped and a
				// warning is emitted. The message is safe to surface — the PVE
				// client never embeds credentials in its errors (defense in
				// depth: do not rewrap with credentials here either).
				results[i] = nodeQueryResult{Name: n.Name, Err: err}
				return
			}
			results[i] = nodeQueryResult{Name: n.Name, VMs: vms}
		}()
	}
	wg.Wait()

	perNode := make(map[int64]nodeQueryResult, len(results))
	for i := range results {
		perNode[nodes[i].ID] = results[i]
	}

	local, err := s.vmRepo.ListVMsPage(ctx, limit, offset)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("list vms: local metadata: %w", err)
	}
	total, err := s.vmRepo.CountVMs(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("list vms: count: %w", err)
	}
	items, warnings := mergeVMListItems(local, perNode)
	return items, warnings, total, nil
}

// GetVM implements the pass-through detail (task 8.2, design D5/D6): the
// local metadata plus the live status read from the VM's node.
//
//	VM row absent               -> not_found
//	pve_vmid == 0               -> creating (failed when provision_error set)
//	node row absent             -> node_unavailable (503), never disguised
//	                               as creating
//	node's list call fails      -> node_unavailable (503), never disguised
//	                               as creating (task 8.3)
//	node answers, VM absent     -> creating (design D5: PVE 不存在 → 创建中)
func (s *VMService) GetVM(ctx context.Context, id int64) (*VMListItem, error) {
	vm, err := s.vmRepo.GetVM(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("vm %d not found", id)
		}
		return nil, fmt.Errorf("get vm %d: %w", id, err)
	}
	switch {
	case vm.VM.ProvisionError != "":
		return &VMListItem{VM: *vm, Status: model.VMStateFailed}, nil
	case vm.VM.PVEVmid == 0:
		return &VMListItem{VM: *vm, Status: model.VMStateCreating}, nil
	}

	node, err := s.nodeRepo.GetNode(ctx, vm.VM.NodeID)
	if err != nil {
		// A VM whose node row is gone (disabled/removed independently of the
		// vms rows) cannot report a live status: this is the same
		// node_unavailable semantics as an unreachable node (task 8.3),
		// never a fake "creating".
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nodeUnavailablef("node %d of vm %d not found", vm.VM.NodeID, id)
		}
		return nil, fmt.Errorf("get vm %d: get node %d: %w", id, vm.VM.NodeID, err)
	}
	client := s.newClient(node.Host, node.APIUser, node.APITokenSecret)
	vms, err := client.ListVMs(ctx, node.Name)
	if err != nil {
		// Task 8.3: a node failure on the detail path is an explicit error,
		// not a fake "creating". The pve client sanitizes its own errors
		// (never embeds the token), so the message is safe to surface.
		return nil, nodeUnavailablef("node %q unavailable: %v", node.Name, err)
	}
	if st, found := findVM(vms, vm.VM.PVEVmid); found {
		return &VMListItem{VM: *vm, Status: st.Status, Live: &LiveVMStatus{
			Status: st.Status, CPUUsage: st.CPU, Mem: st.Mem, MaxMem: st.MaxMem,
			Disk: st.Disk, MaxDisk: st.MaxDisk, Uptime: st.Uptime,
		}}, nil
	}
	// The node is reachable but the VM no longer exists on it (removed
	// outside the service): design D5 reads this as not-provisioned.
	return &VMListItem{VM: *vm, Status: model.VMStateCreating}, nil
}
