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

// listVMsTimeout 限制整个列表查询（数据库读取加每个节点的 PVE 列表调用）。
// 节点调用并行执行，因此该预算指它们总的墙钟时间，而非各自延迟之和：截止
// 时间由请求 context 推导，任何在其触发时尚未响应的节点调用都会以 context
// 错误失败并成为一条警告——与不可达节点相同的部分失败语义（任务 8.3）。
const listVMsTimeout = 10 * time.Second

// LiveVMStatus 是 VM 的透传运行时部分，从 PVE 节点实时读取（设计 D1；数据库
// 仅存储元数据）。大小单位为字节，CPUUsage 是当前已配置核心被占用的比例；
// 已停止的 VM 报告零值。
type LiveVMStatus struct {
	Status   string
	CPUUsage float64
	Mem      int64
	MaxMem   int64
	Disk     int64
	MaxDisk  int64
	Uptime   int64
}

// VMListItem 是透传列表/详情的合并行（任务 8.1/8.2）：本地元数据（创建时
// 请求的规格值，设计 D1）加上 VM 存在于 PVE 时的实时状态。当 VM 没有对应的
// PVE 实体（或它已消失，设计 D5）时 Status 为 "creating"，供给失败时为
// "failed"；这两种情况下 Live 均为 nil。
type VMListItem struct {
	VM     repository.VMWithIP
	Status string
	Live   *LiveVMStatus
}

// NodeWarning 是附加到列表响应的部分失败通知：节点的实时查询失败（不可达、
// TLS、认证失败等），其 VM 从列表中省略（任务 8.3）。Error 可安全展示：PVE
// 客户端会脱敏自己的错误（pve.NewClient 脱敏 API 用户且绝不回显 token 密钥），
// 本包只是原样复制它们。
type NodeWarning struct {
	Node  string
	Error string
}

// nodeQueryResult 为 mergeVMListItems 携带一个节点的 PVE 列表结果：实时 VM
// 列表，或替代它的失败。
type nodeQueryResult struct {
	Name string
	VMs  []pve.VMStatus
	Err  error
}

// mergeVMListItems 是透传列表（任务 8.1）的纯合并：对每个本地 VM，在其节点
// 的 PVE 列表中查找实时状态并合并；没有 PVE 对应实体的 VM 报告为
// creating/failed；失败节点的 VM 被省略并收集进警告。节点不在被查询的启用
// 节点之列（被禁用或已移除）的 VM 同样被省略并给出警告。结果保持本地（id）
// 顺序，警告按节点名排序，因此输出是确定性的。仅存在于 PVE 的 VM（节点上
// 存在、无本地行）没有可合并的元数据，被刻意跳过：它们不受本服务管理。
//
// 它是输入的纯函数，因此合并语义无需访问 PVE 或数据库即可进行单元测试。
func mergeVMListItems(local []repository.VMWithIP, nodes map[int64]nodeQueryResult) ([]VMListItem, []NodeWarning) {
	items := make([]VMListItem, 0, len(local))
	// disabled 收集节点不在被查询启用节点之列的本地 VM 的警告，以节点 id 字符串
	// 为键，使它们与查询失败的警告合并到同一次确定性排序中。
	disabled := make(map[string]string)
	// indexes 为每个节点缓存一个 vmid -> status 查找映射，每个节点以 O(P) 构建
	// 一次，使该节点每个 VM 的合并为 O(1)，而不是每行都对 PVE 列表做线性扫描。
	indexes := make(map[int64]map[int64]pve.VMStatus)
	for i := range local {
		vm := local[i]
		res, ok := nodes[vm.VM.NodeID]
		if !ok {
			// 本地元数据指向的节点不在被查询的启用节点之列（被禁用或已移除）：
			// 该 VM 被省略并产生一条警告，镜像失败节点的语义。
			disabled[strconv.FormatInt(vm.VM.NodeID, 10)] = fmt.Sprintf("node %d not among enabled nodes", vm.VM.NodeID)
			continue
		}
		if res.Err != nil {
			// 节点的实时查询失败：其 VM 作为部分失败被省略（任务 8.3）；警告在
			// 下方收集。
			continue
		}
		switch {
		case vm.VM.ProvisionError != "":
			// provision_error 优先于 creating：分离式供给链已失败。
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
			// 节点有响应但 VM 已不在其中：按设计 D5，状态读取为未供给（creating）。
			// 该行保留其 pve_vmid，便于运维发现不一致。
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

// nodeName 返回节点在 PVE API 路径与镜像键中使用的集群节点名（任务 4.3）：
// PveName 非空时使用 PveName，否则回退到业务名 Name——兼容迁移回填前的存量
// 数据，保证"PveName 缺省 = Name"的行为不回归（设计 D3）。
func nodeName(n model.PVENode) string {
	if n.PveName != "" {
		return n.PveName
	}
	return n.Name
}

// findVM 返回节点列表中指定 vmid 的 VM 的实时状态（如果存在）。列表合并
// （mergeVMListItems）改用预构建的 vmid 映射；findVM 服务于单 VM 详情路径。
func findVM(vms []pve.VMStatus, vmid int64) (pve.VMStatus, bool) {
	for _, v := range vms {
		if v.VMID == vmid {
			return v, true
		}
	}
	return pve.VMStatus{}, false
}

// ListVMs 实现透传列表（任务 8.1，设计 D1）：查询每个区域的启用节点，每个
// 节点恰好一次 PVE 列表调用（设计 D1），读取一页本地元数据（含 IP），并将
// 两者合并。整个查询运行在请求级预算（listVMsTimeout）之内。节点查询并行
// 执行，因此总延迟由最慢的节点决定，而非所有节点延迟之和；列表调用失败的
// 节点——包括共享截止时间触发——贡献一条警告而非其 VM（任务 8.3），绝不会
// 让整个请求失败。
//
// 分页（limit/offset）作用于本地 vms 元数据查询：SQL LIMIT/OFFSET 在 vms
// 表上执行，PVE 合并只看到该页的行，因此最坏情况是每个节点合并 maxPageLimit
// 行。total 是本地 VM 总数（CountVMs）：合并可能丢弃失败或禁用节点的行，
// 因此 total 可能超过该页的行数。警告逻辑不变：只考虑该页的本地行。
func (s *VMService) ListVMs(ctx context.Context, limit, offset int) ([]VMListItem, []NodeWarning, int, error) {
	// 请求级总超时：一起限制数据库读取与所有并行节点调用，因此慢或挂起的节点
	// 无法拉长请求。下方每个节点 goroutine 共享同一个截止时间；当它触发时，
	// 挂起的调用会以 context 错误失败，并像其他任何节点失败一样呈现为警告。
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

	// 每个启用节点一次 PVE 列表调用，并行执行：每个 goroutine 只写 results
	// 切片自己的槽位（索引互不相同，无共享状态），因此无需加锁。之后将
	// nodeQueryResult 值收集到 map 中供合并使用。
	results := make([]nodeQueryResult, len(nodes))
	var wg sync.WaitGroup
	for i := range nodes {
		n := nodes[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := s.newClient(n.Host, n.Port, n.APIUser, n.APITokenSecret)
			vms, err := client.ListVMs(ctx, nodeName(n))
			if err != nil {
				// 部分失败（任务 8.3）：丢弃该节点的 VM 并产生一条警告。消息可安全
				// 展示——PVE 客户端绝不在其错误中内嵌凭据（纵深防御：这里也不要
				// 用凭据重新包装）。
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

// GetVM 实现透传详情（任务 8.2，设计 D5/D6）：本地元数据加上从 VM 所在节点
// 读取的实时状态。
//
//	VM 行不存在                 -> not_found
//	pve_vmid == 0               -> creating（已设置 provision_error 时为 failed）
//	节点行不存在                -> node_unavailable (503)，绝不伪装成 creating
//	节点列表调用失败            -> node_unavailable (503)，绝不伪装成
//	                               creating（任务 8.3）
//	节点有响应但 VM 不存在      -> creating（设计 D5：PVE 不存在 → 创建中）
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
		// 节点行已消失的 VM（与 vms 行相互独立地被禁用/移除）无法报告实时状态：
		// 这与不可达节点具有相同的 node_unavailable 语义（任务 8.3），绝不是
		// 伪造的 "creating"。
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nodeUnavailablef("node %d of vm %d not found", vm.VM.NodeID, id)
		}
		return nil, fmt.Errorf("get vm %d: get node %d: %w", id, vm.VM.NodeID, err)
	}
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	vms, err := client.ListVMs(ctx, nodeName(*node))
	if err != nil {
		// 任务 8.3：详情路径上的节点失败是显式错误，不是伪造的 "creating"。
		// pve 客户端会脱敏自己的错误（绝不内嵌 token），因此消息可安全展示。
		return nil, nodeUnavailablef("node %q unavailable: %v", nodeName(*node), err)
	}
	if st, found := findVM(vms, vm.VM.PVEVmid); found {
		return &VMListItem{VM: *vm, Status: st.Status, Live: &LiveVMStatus{
			Status: st.Status, CPUUsage: st.CPU, Mem: st.Mem, MaxMem: st.MaxMem,
			Disk: st.Disk, MaxDisk: st.MaxDisk, Uptime: st.Uptime,
		}}, nil
	}
	// 节点可达但 VM 已不在其上（在服务之外被移除）：设计 D5 将其解读为未供给。
	return &VMListItem{VM: *vm, Status: model.VMStateCreating}, nil
}
