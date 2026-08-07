package service

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
	"spark/repository"
)

// ImportVMRequest 是 VMService.ImportVM 的已校验输入。
type ImportVMRequest struct {
	ZoneID   int64  `json:"zone_id"`    // 必填
	NodeID   int64  `json:"node_id"`    // 必填
	PVEVmid  int64  `json:"pve_vmid"`   // 必填，PVE 侧 VMID
	IPPoolID int64  `json:"ip_pool_id"` // 可选，0 表示自动选池
	Name     string `json:"name"`       // 可选，空则取 PVE 配置名
}

// UnmanagedVM 是节点 PVE 上未被托管的一个 VM 候选（GET /vms/unmanaged 用）。
type UnmanagedVM struct {
	VMID   int64
	Name   string
	Status string
	CPU    int
	MemMB  int64
	DiskGB int64
}

// importDiskKeyRe 匹配 PVE VM 配置中的全部磁盘键（scsi/ide/sata/virtio +
// 数字，以及 efidisk/tpmstate）。这些键的值携带磁盘 size 字段，导入时
// 遍历求和得到 disk_gb。
var importDiskKeyRe = regexp.MustCompile(`^(scsi|ide|sata|virtio|efidisk|tpmstate)\d+$`)

// maxImportedNameLen 限制导入 VM 名称的最大长度（字符数）。PVE 配置名
// 不受本地约束限制（PVE 自身不设上限），超长时按字符（rune）截断而非
// 拒绝——名称仅用于展示与标识，截断保证落库值不超出 OpenAPI 契约 name
// maxLength: 128。
const maxImportedNameLen = 128

// staticIPFromConfig 从 VM 配置的 ipconfig0 中解析静态 IPv4 地址：
// ipconfig0 形如 "ip=10.0.0.5/24,gw=10.0.0.1"（逗号分隔的键值对），
// 取 ip= 前缀的值（去引号）后用 netip.ParsePrefix 解析，返回前缀中的地址。
// DHCP（"ip=dhcp"）或解析失败时返回无效的 netip.Addr（IsValid()==false），
// 调用方据此回退到池分配。
func staticIPFromConfig(cfg pve.VMConfig) netip.Addr {
	for _, kv := range strings.Split(cfg.String("ipconfig0"), ",") {
		v, ok := strings.CutPrefix(kv, "ip=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		if v == "" || strings.EqualFold(v, "dhcp") {
			return netip.Addr{}
		}
		prefix, err := netip.ParsePrefix(v)
		if err != nil {
			return netip.Addr{}
		}
		return prefix.Addr()
	}
	return netip.Addr{}
}

// vmConfigSpec 从 PVE VM 配置中提取导入规格（ImportVM 与 ListUnmanagedVMs
// 共用）：名称（缺失为 ""，由调用方决定是否回退）、核数（PVE qm 缺省即
// 1 核，因此缺省值 0 按 1 处理）、内存 MiB（直接取实际值，缺省 0 即可，
// 运行状态与配置无关）以及全部磁盘键（importDiskKeyRe）的 size 字段求和
// 得到的 GiB。单个磁盘解析失败会跳过该键（磁盘字符串可能缺 size 字段），
// 缺失键按 0 计，disk_gb 可能偏小——resize 只允许增大，不影响正确性
// （设计文档 Risks）。
func vmConfigSpec(cfg pve.VMConfig) (name string, cpu int, memMB, diskGB int64) {
	name = cfg.String("name")
	if c, err := cfg.Cores(); err == nil {
		cpu = c
	}
	if cpu == 0 {
		cpu = 1 // PVE qm 缺省 1 核
	}
	if m, err := cfg.MemoryMB(); err == nil {
		memMB = m
	}
	for key := range cfg {
		if !importDiskKeyRe.MatchString(key) {
			continue
		}
		// 使用 String() 去掉 JSON 字符串值两端的引号后再解析 size。
		if gb, err := parseDiskSizeGB(cfg.String(key)); err == nil {
			diskGB += gb
		}
	}
	return name, cpu, memMB, diskGB
}

// ImportVM 将节点 PVE 上已有的 VM（pve_vmid）纳管为托管 VM：校验区域/
// 节点/幂等性，读取 PVE 配置提取规格，优先复用 PVE 静态 IP（匹配池精确
// 领取），无静态 IP 或匹配失败时回退从区域池分配，最后在单个事务内落库
// （与 CreateVM 相同的 FK 环约定：INSERT vms -> 领取 IP -> 回填 ip_id）。
// 导入即托管：pve_vmid 非零、provision_error 为空，现有生命周期与透传
// 查询路径直接生效（设计 D5），不需要 provisionVM 供给 goroutine。
func (s *VMService) ImportVM(ctx context.Context, req ImportVMRequest) (*repository.VMWithIP, error) {
	// 0. 请求级总预算：ListVMs（≤30s）+ GetVMConfig（≤30s）最坏 60s+，
	// 包一层 importVMBudget 预算（与 ListVMs 的 listVMsTimeout 相同的部分
	// 失败语义）：预算耗尽时 PVE 调用以 context 错误失败，映射为
	// node_unavailable。ListUnmanagedVMs 不受影响（它有独立的
	// listVMsTimeout 预算）。
	ctx, cancel := context.WithTimeout(ctx, importVMBudget)
	defer cancel()
	// 1. 输入校验。
	if req.ZoneID <= 0 || req.NodeID <= 0 || req.PVEVmid <= 0 {
		return nil, badRequestf("zone_id, node_id and pve_vmid must be positive integers")
	}
	// 2. 区域存在性检查。
	if _, err := s.zoneRepo.GetZone(ctx, req.ZoneID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("zone %d not found", req.ZoneID)
		}
		return nil, fmt.Errorf("import vm: check zone: %w", err)
	}
	// 3. 节点存在性/归属/启用检查。
	node, err := s.nodeRepo.GetNode(ctx, req.NodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("node %d not found", req.NodeID)
		}
		return nil, fmt.Errorf("import vm: get node: %w", err)
	}
	if node.ZoneID != req.ZoneID {
		return nil, badRequestf("node %d does not belong to zone %d", req.NodeID, req.ZoneID)
	}
	if !node.Enabled {
		return nil, nodeUnavailablef("node %q is disabled", nodeName(*node))
	}
	// 4. 幂等检查：该节点上的 pve_vmid 已被托管则拒绝。
	existing, err := s.vmRepo.GetVMByNodeVMID(ctx, req.NodeID, req.PVEVmid)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("import vm: idempotency check: %w", err)
		}
	} else if existing != nil {
		return nil, vmAlreadyManagedf("vm already managed: node %d pve_vmid %d", req.NodeID, req.PVEVmid)
	}
	// 5. 确认 VM 存在于节点列表并读取 PVE 配置。错误消息绝不携带凭据
	// （pve 客户端自身脱敏），与 vm_query.go GetVM 的 node_unavailable
	// 风格一致。先 ListVMs 再 GetVMConfig：VM 已从 PVE 删除时配置端点
	// 返回 PVE 404，先读配置会把"VM 已删除"误判为节点不可用（503）；
	// 先查列表才能把"VM 不存在"正确映射为 404 vm_not_found_on_node。
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	vms, err := client.ListVMs(ctx, nodeName(*node))
	if err != nil {
		return nil, nodeUnavailablef("node %q unavailable: %v", nodeName(*node), err)
	}
	st, found := findVM(vms, req.PVEVmid)
	if !found {
		// 节点可达但 VM 已被删除。
		return nil, vmNotFoundOnNodef("vm %d not found on node %q", req.PVEVmid, nodeName(*node))
	}
	if st.Template == 1 {
		// PVE 模板是供克隆使用的基础镜像而非运行实体，不可导入。
		return nil, badRequestf("cannot import pve template vm %d", req.PVEVmid)
	}
	cfg, err := client.GetVMConfig(ctx, nodeName(*node), req.PVEVmid)
	if err != nil {
		return nil, nodeUnavailablef("node %q unavailable: %v", nodeName(*node), err)
	}
	// 6. 从配置提取规格（名称回退到请求值）：请求名须匹配 vmNamePattern
	// 且不超过 maxImportedNameLen 个字符（与 CreateVM 相同的校验，对齐契约
	// name maxLength: 128）；PVE 配置名超长时按字符（rune）截断到
	// maxImportedNameLen 而非拒绝。
	name, cpu, memMB, diskGB := vmConfigSpec(cfg)
	if req.Name != "" {
		if !vmNameRegex.MatchString(req.Name) {
			return nil, badRequestf("vm name must match %s", vmNamePattern)
		}
		if len([]rune(req.Name)) > maxImportedNameLen {
			return nil, badRequestf("vm name must be at most %d characters", maxImportedNameLen)
		}
		name = req.Name
	} else if len([]rune(name)) > maxImportedNameLen {
		r := []rune(name)
		name = string(r[:maxImportedNameLen])
	}
	if name == "" {
		return nil, badRequestf("vm name is required")
	}
	// 7. IP 策略决策：解析 PVE 静态 IP（配置已在步骤 5 读取）。
	staticIP := staticIPFromConfig(cfg)

	// 8. 事务落库（与 CreateVM 相同的 FK 环约定，migration 0002 头部）：
	// 先以 ip_id 为 NULL 插入 vms 行，再领取 IP（vmID 取 INSERT 返回的
	// created.ID），最后回填 vms.ip_id；任何失败整体回滚，绝不产生脏记录。
	tx, err := s.beginner.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("import vm: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, err := s.vmRepo.ImportVMTx(ctx, tx, model.VM{
		UUID:              uuid.NewString(),
		Name:              name,
		ZoneID:            req.ZoneID,
		NodeID:            req.NodeID,
		PVEVmid:           req.PVEVmid,
		ImageID:           nil,
		StorageTypeID:     nil,
		CPU:               cpu,
		MemMB:             memMB,
		DiskGB:            diskGB,
		IPID:              nil,
		PasswordEncrypted: "",
		ProvisionError:    "",
	})
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			// 部分唯一索引 (node_id, pve_vmid) WHERE pve_vmid > 0 兜底：
			// 幂等检查与 INSERT 之间的并发导入，与步骤 4 相同的 409 语义。
			return nil, vmAlreadyManagedf("vm already managed: node %d pve_vmid %d", req.NodeID, req.PVEVmid)
		}
		return nil, fmt.Errorf("import vm: insert: %w", err)
	}

	claimed, err := s.claimImportIP(ctx, tx, req, *node, staticIP, created.ID)
	if err != nil {
		return nil, err
	}
	if err := s.vmRepo.SetVMIPIDTx(ctx, tx, created.ID, claimed.ID); err != nil {
		return nil, fmt.Errorf("import vm: link ip: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("import vm: commit: %w", err)
	}

	ipID := claimed.ID
	created.IPID = &ipID
	// 9. 返回。导入后的 VM 不需要 provisionVM goroutine（PVE 侧已存在）。
	return &repository.VMWithIP{VM: *created, IP: claimed.IP}, nil
}

// claimImportIP 在导入事务内为 VM 领取 IP，执行设计 D3 的策略：
//
//   - 用户指定了池（req.IPPoolID > 0）：校验池存在、属于该区域且白名单
//     包含该节点（不满足 -> bad_request），然后在该池内随机领取；
//   - 否则若 PVE 配置携带静态 IP：按区域池顺序（id 升序）找第一个 CIDR
//     包含该地址且白名单包含该节点的池，按地址精确领取（复用静态 IP）；
//     地址被并发占用（ErrAllocationRetry）或不在池内（ErrNoRows）时回退；
//   - 回退路径：自动选择区域内第一个白名单包含该节点的池随机领取；
//     没有可用池或池耗尽（pgx.ErrNoRows）时返回 ip_exhausted。
func (s *VMService) claimImportIP(ctx context.Context, tx pgx.Tx, req ImportVMRequest,
	node model.PVENode, staticIP netip.Addr, vmID int64) (model.IP, error) {
	if req.IPPoolID > 0 {
		pool, err := s.ipPoolRepo.GetPool(ctx, req.IPPoolID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return model.IP{}, badRequestf("ip pool %d not found", req.IPPoolID)
			}
			return model.IP{}, fmt.Errorf("import vm: get pool %d: %w", req.IPPoolID, err)
		}
		if pool.ZoneID != req.ZoneID {
			return model.IP{}, badRequestf("ip pool %d does not belong to zone %d", req.IPPoolID, req.ZoneID)
		}
		allowed, err := s.poolAllowsNode(ctx, pool.ID, node.ID)
		if err != nil {
			return model.IP{}, err
		}
		if !allowed {
			return model.IP{}, badRequestf("ip pool %d does not allow node %d", req.IPPoolID, req.NodeID)
		}
		return s.claimFreeInPool(ctx, tx, pool.ID, vmID)
	}

	if staticIP.IsValid() {
		pools, err := s.ipPoolRepo.ListPoolsByZone(ctx, req.ZoneID)
		if err != nil {
			return model.IP{}, fmt.Errorf("import vm: list pools of zone %d: %w", req.ZoneID, err)
		}
		for _, pool := range pools {
			prefix, err := netip.ParsePrefix(pool.NetworkCIDR)
			if err != nil || !prefix.Contains(staticIP) {
				continue // 无效 CIDR 或静态 IP 不在池网段内
			}
			allowed, err := s.poolAllowsNode(ctx, pool.ID, node.ID)
			if err != nil {
				return model.IP{}, err
			}
			if !allowed {
				continue
			}
			ip, err := s.ipPoolRepo.ClaimIPByAddressTx(ctx, tx, pool.ID, staticIP.String(), &vmID)
			if err == nil {
				return ip, nil
			}
			if errors.Is(err, repository.ErrAllocationRetry) || errors.Is(err, pgx.ErrNoRows) {
				// 地址被并发占用或不在该池内：继续尝试下一个 CIDR 同样包含
				// 该地址的池（best-effort 提升静态 IP 复用命中率，设计 D3）；
				// 所有池都失败后才回退到随机分配。
				continue
			}
			return model.IP{}, fmt.Errorf("import vm: claim static ip %s in pool %d: %w", staticIP, pool.ID, err)
		}
	}

	// 回退路径：自动选择区域内第一个白名单包含该节点的池。
	pool, err := s.selectImportPool(ctx, req.ZoneID, node.ID)
	if err != nil {
		return model.IP{}, err
	}
	return s.claimFreeInPool(ctx, tx, pool.ID, vmID)
}

// poolAllowsNode 报告池白名单（ip_pool_nodes）是否包含给定节点。
func (s *VMService) poolAllowsNode(ctx context.Context, poolID, nodeID int64) (bool, error) {
	nodes, err := s.ipPoolRepo.GetPoolNodes(ctx, poolID)
	if err != nil {
		return false, fmt.Errorf("import vm: pool %d nodes: %w", poolID, err)
	}
	for _, n := range nodes {
		if n.ID == nodeID {
			return true, nil
		}
	}
	return false, nil
}

// selectImportPool 返回区域内第一个白名单包含给定节点的池（按 id 升序）。
// 没有可用池时返回 ip_exhausted：导入必须给 VM 分配地址，无池可分配即
// 视为地址资源不足（设计 D3）。
func (s *VMService) selectImportPool(ctx context.Context, zoneID, nodeID int64) (model.IPPool, error) {
	pools, err := s.ipPoolRepo.ListPoolsByZone(ctx, zoneID)
	if err != nil {
		return model.IPPool{}, fmt.Errorf("import vm: list pools of zone %d: %w", zoneID, err)
	}
	for _, pool := range pools {
		allowed, err := s.poolAllowsNode(ctx, pool.ID, nodeID)
		if err != nil {
			return model.IPPool{}, err
		}
		if allowed {
			return pool, nil
		}
	}
	return model.IPPool{}, ipExhaustedf("zone %d has no ip pool that allows node %d", zoneID, nodeID)
}

// claimFreeInPool 在事务内随机领取池中的空闲地址，重试
// repository.ErrAllocationRetry 竞态（与 CreateVM 相同的 vmClaimRetries
// 循环）；池耗尽（pgx.ErrNoRows）或重试次数用尽返回 ip_exhausted。
func (s *VMService) claimFreeInPool(ctx context.Context, tx pgx.Tx, poolID, vmID int64) (model.IP, error) {
	for attempt := 0; attempt < vmClaimRetries; attempt++ {
		ip, err := s.ipPoolRepo.ClaimFreeIP(ctx, tx, poolID, &vmID)
		if err == nil {
			return ip, nil
		}
		if errors.Is(err, repository.ErrAllocationRetry) {
			continue // 在同一事务内挑选另一个随机候选地址
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return model.IP{}, ipExhaustedf("pool %d has no free ip", poolID)
		}
		return model.IP{}, fmt.Errorf("import vm: claim ip in pool %d: %w", poolID, err)
	}
	return model.IP{}, ipExhaustedf("pool %d has no free ip after %d attempts", poolID, vmClaimRetries)
}

// ListUnmanagedVMs 返回节点 PVE 上尚未被托管（本地 vms 无对应
// node_id+pve_vmid 行）的 VM 候选，按 VMID 升序排序。候选的规格取
// ListVMs 摘要字段（MaxMem/MaxDisk 字节换算）；已停止的 VM 会省略大多
// 数字段（零值），此时对 MaxMem<=0 的候选再调 GetVMConfig 补全规格
// （复用 vmConfigSpec）；补全失败（配置读取错误）的候选被跳过。PVE
// 模板（template==1）不作为候选。
func (s *VMService) ListUnmanagedVMs(ctx context.Context, nodeID int64) ([]UnmanagedVM, error) {
	// 整个查询（列表 + 逐候选配置补全）共享同一请求级总预算：与 ListVMs
	// 相同的部分失败语义，预算耗尽表现为节点调用失败（node_unavailable）。
	ctx, cancel := context.WithTimeout(ctx, listVMsTimeout)
	defer cancel()
	node, err := s.nodeRepo.GetNode(ctx, nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("node %d not found", nodeID)
		}
		return nil, fmt.Errorf("list unmanaged vms: get node %d: %w", nodeID, err)
	}
	if !node.Enabled {
		return nil, nodeUnavailablef("node %q is disabled", nodeName(*node))
	}
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	vms, err := client.ListVMs(ctx, nodeName(*node))
	if err != nil {
		return nil, nodeUnavailablef("node %q unavailable: %v", nodeName(*node), err)
	}
	// 本地已托管 vmid 集合：ListVMs 全量后按 node_id 筛选。
	local, err := s.vmRepo.ListVMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list unmanaged vms: local metadata: %w", err)
	}
	managed := make(map[int64]struct{})
	for _, v := range local {
		if v.VM.NodeID == nodeID {
			managed[v.VM.PVEVmid] = struct{}{}
		}
	}
	out := make([]UnmanagedVM, 0, len(vms))
	for _, st := range vms {
		if st.Template == 1 {
			continue // PVE 模板不可导入：不出现在候选列表
		}
		if _, ok := managed[st.VMID]; ok {
			continue // 已被托管：不出现在候选列表
		}
		u := UnmanagedVM{
			VMID:   st.VMID,
			Name:   st.Name,
			Status: st.Status,
			CPU:    int(st.Cpus),
			MemMB:  st.MaxMem >> 20,  // 字节 -> MiB
			DiskGB: st.MaxDisk >> 30, // 字节 -> GiB
		}
		if u.MemMB <= 0 {
			// 已停止的 VM 摘要大多为零值：读配置补全规格；失败则跳过
			// 该候选（不向用户展示零规格条目）。
			cfg, err := client.GetVMConfig(ctx, nodeName(*node), st.VMID)
			if err != nil {
				continue
			}
			name, cpu, memMB, diskGB := vmConfigSpec(cfg)
			u.Name, u.CPU, u.MemMB, u.DiskGB = name, cpu, memMB, diskGB
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out, nil
}
