package service

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
	"spark/repository"
)

// ImportVMRequest 是 VMService.ImportVM（认领）的已校验输入。
type ImportVMRequest struct {
	ZoneID  int64  `json:"zone_id"`  // 必填
	NodeID  int64  `json:"node_id"`  // 必填
	PVEVmid int64  `json:"pve_vmid"` // 必填，PVE 侧 VMID
	Name    string `json:"name"`     // 可选，空则取 PVE 配置名
	// IP 可选：从区域 IP 池按地址分配占用；不传则 ip_id 保持 NULL，虚拟机
	// 的网络由 PVE 侧配置决定（设计 D6，spec：认领虚拟机的 IP 策略）。
	IP string `json:"ip"`
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

// vmConfigSpec 从 PVE VM 配置中提取认领规格：名称（缺失为 ""，由调用方
// 决定是否回退）、核数（PVE qm 缺省即 1 核，因此缺省值 0 按 1 处理）、
// 内存 MiB（直接取实际值，缺省 0 即可，运行状态与配置无关）以及全部磁盘
// 键（importDiskKeyRe）的 size 字段求和得到的 GiB。单个磁盘解析失败会
// 跳过该键（磁盘字符串可能缺 size 字段），缺失键按 0 计，disk_gb 可能
// 偏小——resize 只允许增大，不影响正确性（设计文档 Risks）。
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

// ImportVM 将节点 PVE 上已有的 VM（pve_vmid）认领为托管 VM（spec：认领
// 虚拟机）：校验区域/节点/幂等性，读取 PVE 配置提取规格，按认领请求可选
// 分配 IP（设计 D6），最后在单个事务内落库（与 CreateVM 相同的 FK 环约定：
// INSERT vms -> 领取 IP -> 回填 ip_id）。认领即托管：pve_vmid 非零、
// provision_error 为空，source 置为 claimed（设计 D3），现有生命周期与透传
// 查询路径直接生效（设计 D5），不需要 provisionVM 供给 goroutine。
func (s *VMService) ImportVM(ctx context.Context, req ImportVMRequest) (*repository.VMWithIP, error) {
	// 0. 请求级总预算：ListVMs（≤30s）+ GetVMConfig（≤30s）最坏 60s+，
	// 包一层 importVMBudget 预算（与 ListVMs 的 listVMsTimeout 相同的部分
	// 失败语义）：预算耗尽时 PVE 调用以 context 错误失败，映射为
	// node_unavailable。
	ctx, cancel := context.WithTimeout(ctx, importVMBudget)
	defer cancel()
	// 1. 输入校验。
	if req.ZoneID <= 0 || req.NodeID <= 0 || req.PVEVmid <= 0 {
		return nil, badRequestf("zone_id, node_id and pve_vmid must be positive integers")
	}
	if req.IP != "" {
		if _, err := netip.ParseAddr(req.IP); err != nil {
			return nil, badRequestf("invalid ip address %q", req.IP)
		}
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
	// 5. 确认 VM 存在于节点列表并读取 PVE 配置。错误消息绝不携带凭据或内部
	// 细节（sanitizePVEError 去掉内部 base URL/host:port 与 API 路径），
	// 与 vm_query.go GetVM 的 node_unavailable 风格一致。先 ListVMs 再
	// GetVMConfig：VM 已从 PVE 删除时配置端点返回 PVE 404，先读配置会把
	// "VM 已删除"误判为节点不可用（503）；先查列表才能把"VM 不存在"正确
	// 映射为 404 vm_not_found_on_node。
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	vms, err := client.ListVMs(ctx, nodeName(*node))
	if err != nil {
		return nil, nodeUnavailablef("node %q unavailable: %s", nodeName(*node), sanitizePVEError(err))
	}
	st, found := findVM(vms, req.PVEVmid)
	if !found {
		// 节点可达但 VM 已被删除。
		return nil, vmNotFoundOnNodef("vm %d not found on node %q", req.PVEVmid, nodeName(*node))
	}
	if st.Template == 1 {
		// PVE 模板是供克隆使用的基础镜像而非运行实体，不可认领。
		return nil, badRequestf("cannot import pve template vm %d", req.PVEVmid)
	}
	cfg, err := client.GetVMConfig(ctx, nodeName(*node), req.PVEVmid)
	if err != nil {
		return nil, nodeUnavailablef("node %q unavailable: %s", nodeName(*node), sanitizePVEError(err))
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

	// 7. 事务落库（与 CreateVM 相同的 FK 环约定，migration 0002 头部）：
	// 先以 ip_id 为 NULL 插入 vms 行（source=claimed），再按请求可选领取
	// IP（vmID 取 INSERT 返回的 created.ID），最后回填 vms.ip_id；任何失败
	// 整体回滚，绝不产生脏记录。
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
		Source:            model.VMSourceClaimed,
	})
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			// 部分唯一索引 (node_id, pve_vmid) WHERE pve_vmid > 0 兜底：
			// 幂等检查与 INSERT 之间的并发导入，与步骤 4 相同的 409 语义。
			return nil, vmAlreadyManagedf("vm already managed: node %d pve_vmid %d", req.NodeID, req.PVEVmid)
		}
		return nil, fmt.Errorf("import vm: insert: %w", err)
	}

	claimed, err := s.claimImportIP(ctx, tx, req, *node, created.ID)
	if err != nil {
		return nil, err
	}
	if claimed.ID != 0 {
		if err := s.vmRepo.SetVMIPIDTx(ctx, tx, created.ID, claimed.ID); err != nil {
			return nil, fmt.Errorf("import vm: link ip: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("import vm: commit: %w", err)
	}

	if claimed.ID != 0 {
		ipID := claimed.ID
		created.IPID = &ipID
	}
	// 8. 返回。认领后的 VM 不需要 provisionVM goroutine（PVE 侧已存在）。
	return &repository.VMWithIP{VM: *created, IP: claimed.IP}, nil
}

// claimImportIP 在认领事务内按请求分配 IP（设计 D6，spec：认领虚拟机的
// IP 策略）：
//
//   - 请求未携带 ip：不分配任何地址（ip_id 保持 NULL，虚拟机网络由 PVE
//     侧配置决定），返回零值 model.IP；
//   - 请求携带 ip：按区域池顺序（id 升序）找第一个 CIDR 包含该地址且
//     白名单包含该节点的池，按地址精确领取（ClaimIPByAddressTx）；地址
//     被并发占用（ErrAllocationRetry）或不在池内（ErrNoRows）时继续尝试
//     下一个池；没有可用池或全部失败时返回 ip_exhausted。
func (s *VMService) claimImportIP(ctx context.Context, tx pgx.Tx, req ImportVMRequest,
	node model.PVENode, vmID int64) (model.IP, error) {
	if req.IP == "" {
		return model.IP{}, nil
	}
	// 入口（ImportVM 步骤 1）已校验 req.IP 为合法地址，这里仅做类型转换，
	// 解析不可能失败。
	addr, _ := netip.ParseAddr(req.IP)
	pools, err := s.ipPoolRepo.ListPoolsByZone(ctx, req.ZoneID)
	if err != nil {
		return model.IP{}, fmt.Errorf("import vm: list pools of zone %d: %w", req.ZoneID, err)
	}
	for _, pool := range pools {
		prefix, err := netip.ParsePrefix(pool.NetworkCIDR)
		if err != nil || !prefix.Contains(addr) {
			continue // 无效 CIDR 或地址不在池网段内
		}
		allowed, err := s.poolAllowsNode(ctx, pool.ID, node.ID)
		if err != nil {
			return model.IP{}, err
		}
		if !allowed {
			continue
		}
		ip, err := s.ipPoolRepo.ClaimIPByAddressTx(ctx, tx, pool.ID, addr.String(), &vmID)
		if err == nil {
			return ip, nil
		}
		if errors.Is(err, repository.ErrAllocationRetry) || errors.Is(err, pgx.ErrNoRows) {
			// 地址被并发占用或不在该池内：继续尝试下一个 CIDR 同样包含
			// 该地址的池。
			continue
		}
		return model.IP{}, fmt.Errorf("import vm: claim ip %s in pool %d: %w", addr, pool.ID, err)
	}
	return model.IP{}, ipExhaustedf("ip %s is not available in any pool of zone %d", req.IP, req.ZoneID)
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
