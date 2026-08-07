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

// VMListItem 是透传列表/详情的合并行（任务 8.1/8.2 + 全部 PVE 虚拟机可见，
// 设计 D1/D2/D3）：本地元数据（创建时请求的规格值，设计 D1）加上 VM
// 存在于 PVE 时的实时状态。当 VM 没有对应的 PVE 实体（或它已消失，设计
// D5）时 Status 为 "creating"，供给失败时为 "failed"；这两种情况下 Live
// 均为 nil。
//
// ExternalID 非空时该条目是 PVE 上存在而本地无记录的外部虚拟机（source
// 为 external，不落库，设计 D3）：VM 字段携带 PVE 摘要的规格/名称（本地
// 行字段如 UUID/created_at 无意义，均为零值），id 以合成标识
// "ext-{nodeID}-{vmid}" 呈现（设计 D2）。
type VMListItem struct {
	VM     repository.VMWithIP
	Status string
	Live   *LiveVMStatus
	// ExternalID 是外部条目的合成标识（ext-{nodeID}-{vmid}）；本地条目为空。
	ExternalID string
}

// extIDPrefix 是外部 VM 合成 id 的前缀（设计 D2）。
const extIDPrefix = "ext-"

// externalVMID 构造外部条目的合成标识 ext-{nodeID}-{vmid}（设计 D2）。
func externalVMID(nodeID, vmid int64) string {
	return fmt.Sprintf("%s%d-%d", extIDPrefix, nodeID, vmid)
}

// NodeWarning 是附加到列表响应的部分失败通知：节点的实时查询失败（不可达、
// TLS、认证失败等），其 VM 从列表中省略（任务 8.3）。Node 是节点的业务名
// （本地行的节点被禁用/移除时同样输出节点名，绝不输出内部 id）。Error 可
// 安全展示：service 层经 sanitizePVEError 脱敏（去掉内部 base URL/host:port
// 与 API 路径），只保留 PVE 返回的错误消息或传输层原因摘要。
type NodeWarning struct {
	Node  string
	Error string
}

// nodeQueryResult 为 mergeVMListItems 携带一个节点的 PVE 列表结果：实时 VM
// 列表，或替代它的失败。ZoneID 供 external 条目回填 zone_id。
type nodeQueryResult struct {
	Name   string
	ZoneID int64
	VMs    []pve.VMStatus
	Err    error
}

// mergeVMListItems 是透传列表（任务 8.1 + 全部 PVE 虚拟机可见）的纯合并：
// 对每个本地 VM，在其节点的 PVE 列表中查找实时状态并合并；没有 PVE 对应
// 实体的 VM 报告为 creating/failed；仅存在于 PVE 的 VM（本地无行）作为
// external 条目并入（设计 D1/D2/D3，PVE 模板除外）。失败节点的 VM 被省略
// 并收集进警告。节点不在被查询的启用节点之列（被禁用或已移除）的 VM 同样
// 被省略并给出警告，警告的 Node 字段取 nodeNames 映射的节点名（缺失时
// 兜底为 id 字符串）。合并结果按 (node_id, pve_vmid) 升序稳定排序，保证
// 翻页稳定（设计 D2）；警告按节点名排序，因此输出是确定性的。
//
// 它是输入的纯函数，因此合并语义无需访问 PVE 或数据库即可进行单元测试。
func mergeVMListItems(local []repository.VMWithIP, nodes map[int64]nodeQueryResult, nodeNames map[int64]string) ([]VMListItem, []NodeWarning) {
	items := make([]VMListItem, 0, len(local))
	// disabled 收集节点不在被查询启用节点之列的本地 VM 的警告，以节点名为
	// 键，使它们与查询失败的警告合并到同一次确定性排序中。
	disabled := make(map[string]string)
	// indexes 为每个节点缓存一个 vmid -> status 查找映射，每个节点以 O(P) 构建
	// 一次，使该节点每个 VM 的合并为 O(1)，而不是每行都对 PVE 列表做线性扫描。
	indexes := make(map[int64]map[int64]pve.VMStatus)
	// managed 记录本地已托管的 (node_id, pve_vmid)，供 external 差集判定；
	// 只有 pve_vmid > 0 的行才能与 PVE 实体对应（0 是"尚未在 PVE 上创建"
	// 的哨兵）。
	managed := make(map[int64]map[int64]struct{})
	for i := range local {
		vm := local[i]
		res, ok := nodes[vm.VM.NodeID]
		if !ok {
			// 本地元数据指向的节点不在被查询的启用节点之列（被禁用或已移除）：
			// 该 VM 被省略并产生一条警告，镜像失败节点的语义。Node 字段输出
			// 节点名（nodeNames 映射），保持与 PVE 失败分支一致的对外形态。
			name := nodeNames[vm.VM.NodeID]
			if name == "" {
				name = strconv.FormatInt(vm.VM.NodeID, 10) // 未知节点兜底
			}
			disabled[name] = fmt.Sprintf("node %q not among enabled nodes", name)
			continue
		}
		if res.Err != nil {
			// 节点的实时查询失败：其 VM 作为部分失败被省略（任务 8.3）；警告在
			// 下方收集。
			continue
		}
		if vm.VM.PVEVmid > 0 {
			if managed[vm.VM.NodeID] == nil {
				managed[vm.VM.NodeID] = make(map[int64]struct{})
			}
			managed[vm.VM.NodeID][vm.VM.PVEVmid] = struct{}{}
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

	// external 条目：遍历启用节点（按节点 id 升序保证确定性），把 PVE 上
	// 存在而本地无对应行（且非模板）的 VM 并入列表（设计 D1/D3）。
	nodeIDs := make([]int64, 0, len(nodes))
	for id := range nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Slice(nodeIDs, func(i, j int) bool { return nodeIDs[i] < nodeIDs[j] })
	for _, nodeID := range nodeIDs {
		res := nodes[nodeID]
		if res.Err != nil {
			continue
		}
		for _, st := range res.VMs {
			if st.Template == 1 {
				// PVE 模板是供克隆使用的基础镜像而非运行实体，不并入列表
				//（与认领路径对模板的拒绝一致）。
				continue
			}
			if _, ok := managed[nodeID][st.VMID]; ok {
				continue // 已有本地行：条目在本地循环中生成
			}
			items = append(items, externalVMListItem(nodeID, res.ZoneID, st))
		}
	}

	// 统一按 (node_id, pve_vmid) 升序稳定排序：external 条目不随 PVE 列表
	// 顺序漂移，翻页稳定（设计 D2）。本地 pve_vmid=0 的 creating/failed 行
	// 排在该节点实体之前；同键行保持输入相对顺序（唯一索引保证本地行键唯一，
	// external 与本地行键互斥）。
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].VM.VM, items[j].VM.VM
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		return a.PVEVmid < b.PVEVmid
	})

	names := make([]string, 0, len(nodes)+len(disabled))
	errs := make(map[string]string, len(nodes)+len(disabled))
	for _, res := range nodes {
		if res.Err == nil {
			continue
		}
		// 脱敏：原始错误可能携带内部 base URL/host:port 与 API 路径，警告
		// 只保留 PVE 返回的错误消息或传输层原因摘要。
		errs[res.Name] = sanitizePVEError(res.Err)
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

// externalVMListItem 从 PVE 摘要构建 external 条目（设计 D2）：合成 id
// ext-{nodeID}-{vmid}，uuid/created_at 等本地行字段保持零值，名称/规格
// 取摘要值（MemMB/MaxDisk 字节换算），实时状态与指标透传。
func externalVMListItem(nodeID, zoneID int64, st pve.VMStatus) VMListItem {
	return VMListItem{
		VM: repository.VMWithIP{VM: model.VM{
			Name:    st.Name,
			ZoneID:  zoneID,
			NodeID:  nodeID,
			PVEVmid: st.VMID,
			CPU:     int(st.Cpus),
			MemMB:   st.MaxMem >> 20,  // 字节 -> MiB
			DiskGB:  st.MaxDisk >> 30, // 字节 -> GiB
			Source:  model.VMSourceExternal,
		}},
		Status: st.Status,
		Live: &LiveVMStatus{
			Status: st.Status, CPUUsage: st.CPU, Mem: st.Mem, MaxMem: st.MaxMem,
			Disk: st.Disk, MaxDisk: st.MaxDisk, Uptime: st.Uptime,
		},
		ExternalID: externalVMID(nodeID, st.VMID),
	}
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
// 节点恰好一次 PVE 列表调用（设计 D1），读取本地全量元数据（含 IP），并将
// 两者合并为三类条目（本地行+PVE、本地行-only、PVE-only external，设计
// D1/D2/D3）。整个查询运行在请求级预算（listVMsTimeout）之内。节点查询并行
// 执行，因此总延迟由最慢的节点决定，而非所有节点延迟之和；列表调用失败的
// 节点——包括共享截止时间触发——贡献一条警告而非其 VM（任务 8.3），绝不会
// 让整个请求失败。
//
// 分页（limit/offset）作用于合并后的完整条目列表：先按 (node_id, pve_vmid)
// 升序稳定排序，再在内存中切片分页（设计 D1）——external 条目因此与本地
// 条目同页混排且翻页稳定。total 是合并后条目总数（含 external、剔除失败/
// 禁用节点的 VM），即 X-Total-Count 的口径；它与本地 vms 行数（CountVMs
// 的旧口径）不再相等，本地行数没有独立的对外语义。
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
				// 部分失败（任务 8.3）：丢弃该节点的 VM 并产生一条警告。原始
				// 错误只在内部传递，对外呈现的警告消息在 mergeVMListItems 中
				// 经 sanitizePVEError 脱敏（去掉内部 base URL/API 路径等）。
				results[i] = nodeQueryResult{Name: n.Name, Err: err}
				return
			}
			results[i] = nodeQueryResult{Name: n.Name, ZoneID: n.ZoneID, VMs: vms}
		}()
	}
	wg.Wait()

	perNode := make(map[int64]nodeQueryResult, len(results))
	for i := range results {
		perNode[nodes[i].ID] = results[i]
	}

	// 本地全量行：合并需要与每节点 PVE 全量摘要做差集（设计 D1/D3），
	// SQL 分页在此不再适用，分页在合并排序后统一执行。
	local, err := s.vmRepo.ListVMs(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("list vms: local metadata: %w", err)
	}

	// 节点名映射：启用节点直接取自上述查询；本地 VM 引用的其他节点（被禁用
	// 或已移除）单独查询名字，供 disabled 警告输出节点名而非内部 id。
	nodeNames := make(map[int64]string, len(nodes))
	for _, n := range nodes {
		nodeNames[n.ID] = n.Name
	}
	missing := make([]int64, 0)
	for i := range local {
		if _, ok := nodeNames[local[i].VM.NodeID]; !ok {
			nodeNames[local[i].VM.NodeID] = "" // 去重标记
			missing = append(missing, local[i].VM.NodeID)
		}
	}
	if len(missing) > 0 {
		extra, err := s.nodeRepo.ListNodesByIDs(ctx, missing)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("list vms: node names: %w", err)
		}
		for _, n := range extra {
			nodeNames[n.ID] = n.Name
		}
	}

	items, warnings := mergeVMListItems(local, perNode, nodeNames)
	total := len(items)
	page := slicePage(items, limit, offset)
	return page, warnings, total, nil
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
		// 消息经 sanitizePVEError 脱敏（去掉内部 base URL/host:port 与 API
		// 路径），只保留 PVE 返回的错误消息或传输层原因摘要。
		return nil, nodeUnavailablef("node %q unavailable: %s", nodeName(*node), sanitizePVEError(err))
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
