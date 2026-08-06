package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"spark/crypto"
	"spark/model"
	"spark/pve"
	"spark/repository"
)

// VM 生命周期领域附加的服务错误种类。这些值位于 errors.go 共享 iota 范围
// 之外（该范围归其他批次所有），以避免本文件与它们的改动产生耦合。
const (
	// KindVMNotReady：VM 尚没有对应的 PVE 实体（供给未完成或 PVE VM 已消失）；
	// 生命周期操作被拒绝。
	KindVMNotReady ErrorKind = 102
	// KindDiskShrinkNotAllowed：请求的磁盘大小小于当前大小。
	KindDiskShrinkNotAllowed ErrorKind = 103
	// KindImageNotAvailable：镜像未出现在请求区域的每个启用节点上。
	KindImageNotAvailable ErrorKind = 104
)

func vmNotReadyf(format string, args ...any) *Error {
	return &Error{Kind: KindVMNotReady, Message: fmt.Sprintf(format, args...)}
}

func diskShrinkNotAllowedf(format string, args ...any) *Error {
	return &Error{Kind: KindDiskShrinkNotAllowed, Message: fmt.Sprintf(format, args...)}
}

func imageNotAvailablef(format string, args ...any) *Error {
	return &Error{Kind: KindImageNotAvailable, Message: fmt.Sprintf(format, args...)}
}

const (
	// vmClaimRetries 限制创建事务内条件式 IP 抢占的重试循环次数
	// （repository.ErrAllocationRetry）。
	vmClaimRetries = 5
	// vmProvisionTimeout 限制整条分离式供给链：NextVMID + create + WaitTask
	// （默认 10 分钟）+ resize + 配置读取。
	vmProvisionTimeout = 12 * time.Minute
	// maxProvisionErrorLen 限制存储在 vms 中的 provision_error 值长度，避免
	// 冗长的 PVE dump 撑大该行。
	maxProvisionErrorLen = 1000
	// vmNamePattern 是 PVE qm 的名称规则：必须匹配
	// ^[A-Za-z0-9_][A-Za-z0-9_.\-]*$（首字符为字母、数字或下划线，之后
	// 可以是字母、数字、下划线、点和短横线）。
	vmNamePattern = `^[A-Za-z0-9_][A-Za-z0-9_.\-]*$`
)

var vmNameRegex = regexp.MustCompile(vmNamePattern)

// TxBeginner 开启一个数据库事务；*pgxpool.Pool 满足该接口。VM 服务将 IP
// 分配的事务编排保留在服务层（按 migration 0002 头部约定），因此除了各仓库
// 之外，它还需要一个事务入口。
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// VMRepository 是 VMService 依赖的 vms 数据访问层。
type VMRepository interface {
	CreateVMTx(ctx context.Context, tx pgx.Tx, vm model.VM) (*model.VM, error)
	GetVM(ctx context.Context, id int64) (*repository.VMWithIP, error)
	ListVMs(ctx context.Context) ([]repository.VMWithIP, error)
	ListVMsPage(ctx context.Context, limit, offset int) ([]repository.VMWithIP, error)
	CountVMs(ctx context.Context) (int, error)
	SetVMIPIDTx(ctx context.Context, tx pgx.Tx, id, ipID int64) error
	UpdateVMPVEVMID(ctx context.Context, id, vmid, diskGB int64) error
	SetProvisionError(ctx context.Context, id int64, message string) error
	UpdateSpec(ctx context.Context, id int64, newCPU int, newMemMB, newDiskGB int64, oldCPU int, oldMemMB, oldDiskGB int64) error
	DeleteVMTx(ctx context.Context, tx pgx.Tx, id int64) error
}

// VMZoneRepository 是 VMService 依赖的区域数据访问层。
type VMZoneRepository interface {
	GetZone(ctx context.Context, id int64) (*model.Zone, error)
	ListZones(ctx context.Context) ([]model.Zone, error)
}

// VMNodeRepository 是 VMService 依赖的节点数据访问层。
type VMNodeRepository interface {
	GetNode(ctx context.Context, id int64) (*model.PVENode, error)
	ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error)
}

// VMIPPoolRepository 是 VMService 依赖的 IP 池数据访问层。
type VMIPPoolRepository interface {
	ListPoolsByZone(ctx context.Context, zoneID int64) ([]model.IPPool, error)
	GetPoolNodes(ctx context.Context, poolID int64) ([]model.PVENode, error)
	ClaimFreeIP(ctx context.Context, tx pgx.Tx, poolID int64, vmID *int64) (model.IP, error)
	ReleaseIPByVMTx(ctx context.Context, tx pgx.Tx, vmID int64) error
}

// VMImageRepository 是 VMService 依赖的镜像数据访问层。
type VMImageRepository interface {
	Get(ctx context.Context, id int64) (*model.Image, error)
	EnabledNodeNamesByZone(ctx context.Context, zoneID int64) ([]string, error)
}

// VMStorageTypeRepository 是 VMService 依赖的存储类型数据访问层。
type VMStorageTypeRepository interface {
	Get(ctx context.Context, id int64) (*model.StorageType, error)
}

// CreateVMRequest 是 VMService.CreateVM 的已校验输入；字段名与 D6 API 的
// 形态完全一致（POST /vms 请求体）。
type CreateVMRequest struct {
	Name          string `json:"name"`
	CPU           int    `json:"cpu"`
	MemMB         int64  `json:"mem_mb"`
	DiskGB        int64  `json:"disk_gb"`
	ImageID       int64  `json:"image_id"`
	StorageTypeID int64  `json:"storage_type_id"`
	ZoneID        int64  `json:"zone_id"`
	Password      string `json:"password"`
}

// validateCreateVMRequest 强制创建校验中与存在性无关的部分：名称、正数规格
// 以及非空密码。存在性检查（区域、镜像、存储类型、镜像在区域内的可用性）在
// CreateVM 中先于本函数执行，与文档化的校验顺序一致。
func validateCreateVMRequest(req CreateVMRequest) error {
	switch {
	case strings.TrimSpace(req.Name) == "":
		return badRequestf("vm name is required")
	case !vmNameRegex.MatchString(req.Name):
		return badRequestf("vm name must match %s", vmNamePattern)
	case req.Password == "":
		return badRequestf("password is required")
	case req.CPU <= 0:
		return badRequestf("cpu must be > 0")
	case req.MemMB <= 0:
		return badRequestf("mem_mb must be > 0")
	case req.DiskGB <= 0:
		return badRequestf("disk_gb must be > 0")
	}
	return nil
}

// VMService 实现 VM 生命周期的业务规则：创建（原子 IP 分配与分离式 PVE
// 供给链）、启动/停止/重启、销毁（含 IP 释放）以及规格变更。
type VMService struct {
	beginner    TxBeginner
	vmRepo      VMRepository
	ipPoolRepo  VMIPPoolRepository
	zoneRepo    VMZoneRepository
	nodeRepo    VMNodeRepository
	imageRepo   VMImageRepository
	storageRepo VMStorageTypeRepository
	cipher      *crypto.Cipher
	// newClient 为节点构建 PVE 客户端（host/port/API 用户/token）；可注入，
	// 以便测试将供给链和生命周期调用指向假服务器。
	newClient func(host string, port int, apiUser, apiTokenSecret string) *pve.Client
	// selectNode 在池候选节点中挑选部署节点；可注入用于测试，生产默认使用与
	// 服务在其他所有节点交互时相同的 newClient 工厂来探测可达性（因此
	// SetClientFactory 同样会重定向这些探测）。
	selectNode func(ctx context.Context, nodes []model.PVENode) (model.PVENode, error)
}

// NewVMService 使用给定的仓库和加密密码器创建一个 VMService（密码器用于在
// 存储前加密 cloud-init 密码）。
func NewVMService(beginner TxBeginner, vmRepo VMRepository, ipPoolRepo VMIPPoolRepository,
	zoneRepo VMZoneRepository, nodeRepo VMNodeRepository, imageRepo VMImageRepository,
	storageRepo VMStorageTypeRepository, cipher *crypto.Cipher) *VMService {
	s := &VMService{
		beginner:    beginner,
		vmRepo:      vmRepo,
		ipPoolRepo:  ipPoolRepo,
		zoneRepo:    zoneRepo,
		nodeRepo:    nodeRepo,
		imageRepo:   imageRepo,
		storageRepo: storageRepo,
		cipher:      cipher,
		newClient: func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient(host, apiUser, apiTokenSecret, pve.WithPort(port))
		},
	}
	// 可达性探测必须与其他所有节点交互使用相同的客户端工厂，因此覆盖
	// newClient（SetClientFactory、测试、反向代理）也会一并重定向这些探测。
	// 该字段在构造之后才赋值，因为它闭包捕获了 s 自身。
	s.selectNode = func(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
		return selectReachableNode(ctx, nodes, s.newClient)
	}
	return s
}

// SetClientFactory 替换用于所有节点交互（供给链、生命周期操作、透传查询、
// 可达性探测）的 PVE 客户端工厂。默认工厂针对
// https://{host}:{port}/api2/json（port 取节点持久化的端口）构建客户端；
// 覆盖它可以让调用方将服务指向不同的 base URL（测试、反向代理）。
func (s *VMService) SetClientFactory(fn func(host string, port int, apiUser, apiTokenSecret string) *pve.Client) {
	if fn != nil {
		s.newClient = fn
	}
}

// CreateVM 校验请求、挑选可达节点（D4）、原子分配 IP 并持久化 VM 记录（D3 +
// migration 0002 约定），随后启动分离式供给链（D5）并返回带明文 IP 的 VM。
// 返回的记录具有 "creating" 语义：在供给链成功之前 pve_vmid 保持为零。
//
// 供给 goroutine 不得借用调用方的 context（HTTP 处理器返回时该 context 会
// 被取消），因此它在受 vmProvisionTimeout 限制的分离式后台 context 下运行。
func (s *VMService) CreateVM(ctx context.Context, req CreateVMRequest) (*repository.VMWithIP, error) {
	// 1. 检查区域是否存在。
	if req.ZoneID <= 0 {
		return nil, badRequestf("zone_id must be a positive integer")
	}
	if _, err := s.zoneRepo.GetZone(ctx, req.ZoneID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("zone %d not found", req.ZoneID)
		}
		return nil, fmt.Errorf("create vm: check zone: %w", err)
	}
	// 2. 检查镜像是否存在。
	if req.ImageID <= 0 {
		return nil, badRequestf("image_id must be a positive integer")
	}
	image, err := s.imageRepo.Get(ctx, req.ImageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("image %d not found", req.ImageID)
		}
		return nil, fmt.Errorf("create vm: get image: %w", err)
	}
	// 3. 检查存储类型是否存在。
	if req.StorageTypeID <= 0 {
		return nil, badRequestf("storage_type_id must be a positive integer")
	}
	storageType, err := s.storageRepo.Get(ctx, req.StorageTypeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("storage type %d not found", req.StorageTypeID)
		}
		return nil, fmt.Errorf("create vm: get storage type: %w", err)
	}
	// 4. 镜像在区域每个启用节点上的可用性（复用 6.3 的交集语义：node_images
	// 映射必须为每个启用节点都包含一个键）。
	nodeNames, err := s.imageRepo.EnabledNodeNamesByZone(ctx, req.ZoneID)
	if err != nil {
		return nil, fmt.Errorf("create vm: enabled nodes by zone: %w", err)
	}
	if len(filterImagesAvailableByNodes([]model.Image{*image}, nodeNames)) == 0 {
		return nil, imageNotAvailablef("image %d is not available on every enabled node of zone %d", req.ImageID, req.ZoneID)
	}
	// 5. 校验密码与规格。
	if err := validateCreateVMRequest(req); err != nil {
		return nil, err
	}

	// 节点与池的选择（D4）：按 id 顺序遍历区域的池；对每个池，其白名单节点与
	// 区域启用节点求交集，第一个可达节点胜出；不可达的池会被跳过，继续尝试
	// 下一个池。
	pool, node, err := s.selectPoolAndNode(ctx, req.ZoneID)
	if err != nil {
		return nil, err
	}

	// cloud-init 密码以加密形式存储（crypto.Cipher），绝不持久化、记录或以明文
	// 回显。
	passwordEncrypted, err := s.cipher.Encrypt(req.Password)
	if err != nil {
		return nil, fmt.Errorf("create vm: encrypt password: %w", err)
	}

	// 原子落位（D3 + migration 0002 约定）：单个事务依次执行 INSERT vms
	// （ip_id 为 NULL）-> 抢占 ip -> UPDATE vms.ip_id；任何失败都会连同抢占
	// 一起回滚 vms 行。
	tx, err := s.beginner.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create vm: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, err := s.vmRepo.CreateVMTx(ctx, tx, model.VM{
		UUID:              uuid.NewString(),
		Name:              req.Name,
		ZoneID:            req.ZoneID,
		NodeID:            node.ID,
		ImageID:           req.ImageID,
		StorageTypeID:     req.StorageTypeID,
		CPU:               req.CPU,
		MemMB:             req.MemMB,
		DiskGB:            req.DiskGB,
		PasswordEncrypted: passwordEncrypted,
	})
	if err != nil {
		return nil, fmt.Errorf("create vm: insert: %w", err)
	}

	var claimed model.IP
	for attempt := 0; attempt < vmClaimRetries; attempt++ {
		ip, err := s.ipPoolRepo.ClaimFreeIP(ctx, tx, pool.ID, &created.ID)
		if err == nil {
			claimed = ip
			break
		}
		if errors.Is(err, repository.ErrAllocationRetry) {
			continue // 在同一事务内挑选另一个随机候选地址
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ipExhaustedf("pool %d has no free ip", pool.ID)
		}
		return nil, fmt.Errorf("create vm: claim ip in pool %d: %w", pool.ID, err)
	}
	if claimed.ID == 0 {
		return nil, ipExhaustedf("pool %d has no free ip after %d attempts", pool.ID, vmClaimRetries)
	}

	if err := s.vmRepo.SetVMIPIDTx(ctx, tx, created.ID, claimed.ID); err != nil {
		return nil, fmt.Errorf("create vm: link ip: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("create vm: commit: %w", err)
	}

	ipID := claimed.ID
	created.IPID = &ipID

	// 分离式供给链（D5）：请求已经成功返回，因此供给链的失败会被记录到
	// vms.provision_error 而非返回给调用方。使用 context.Background()（而非
	// 请求的 ctx，后者在处理器返回时被取消），受 vmProvisionTimeout 限制。
	vm := *created
	go s.provisionVM(vm, node, image, storageType, pool, req.Password, claimed.IP)

	return &repository.VMWithIP{VM: vm, IP: claimed.IP}, nil
}

// selectPoolAndNode 按 id 顺序遍历区域的 IP 池（D4）。对每个池，将白名单
// 节点（ip_pool_nodes，按节点 id）与区域启用节点求交集，并挑选第一个可达
// 节点；没有可达候选的池会被跳过，继续尝试下一个池。当没有池能产出可达节点
// 时，返回 KindNodeUnavailable 错误（这也涵盖没有池的区域：候选集在构造上
// 即为空）。
func (s *VMService) selectPoolAndNode(ctx context.Context, zoneID int64) (model.IPPool, model.PVENode, error) {
	enabledNodes, err := s.nodeRepo.ListEnabledNodesByZone(ctx, zoneID)
	if err != nil {
		return model.IPPool{}, model.PVENode{}, fmt.Errorf("select node: list enabled nodes: %w", err)
	}
	pools, err := s.ipPoolRepo.ListPoolsByZone(ctx, zoneID)
	if err != nil {
		return model.IPPool{}, model.PVENode{}, fmt.Errorf("select node: list pools: %w", err)
	}
	for _, pool := range pools {
		poolNodes, err := s.ipPoolRepo.GetPoolNodes(ctx, pool.ID)
		if err != nil {
			return model.IPPool{}, model.PVENode{}, fmt.Errorf("select node: pool %d nodes: %w", pool.ID, err)
		}
		candidates := poolCandidates(poolNodes, enabledNodes)
		if len(candidates) == 0 {
			continue
		}
		node, err := s.selectNode(ctx, candidates)
		if err == nil {
			return pool, node, nil
		}
		// KindNodeUnavailable：保留最后的错误并尝试下一个池。
	}
	return model.IPPool{}, model.PVENode{}, nodeUnavailablef("no reachable node for zone %d", zoneID)
}

// poolCandidates 将池的白名单节点与区域启用节点求交集。结果遵循
// GetPoolNodes 返回的节点 id 顺序；v1 接受这种节点 id 顺序而非池的勾选顺序
// （勾选顺序本身未被持久化，无需改动 schema）。
func poolCandidates(poolNodes, enabledNodes []model.PVENode) []model.PVENode {
	enabled := make(map[int64]struct{}, len(enabledNodes))
	for _, n := range enabledNodes {
		enabled[n.ID] = struct{}{}
	}
	candidates := make([]model.PVENode, 0, len(poolNodes))
	for _, n := range poolNodes {
		if _, ok := enabled[n.ID]; ok {
			candidates = append(candidates, n)
		}
	}
	return candidates
}

// provisionVM 运行分离式 PVE 供给链（D5）并将失败记录到
// vms.provision_error。它以 goroutine 形式配合分离式后台 context 被调用；
// 返回的错误仅用于记录日志。
//
// 该 goroutine 绝不能拖垮进程：链中任何位置的 panic 都会在此被恢复并记录
// 为内部供给错误，使 VM 行保持可检查状态（provision_error 已设置，pve_vmid
// 仍为零）。
func (s *VMService) provisionVM(vm model.VM, node model.PVENode, image *model.Image,
	storageType *model.StorageType, pool model.IPPool, plainPassword, ipAddr string) {
	ctx, cancel := context.WithTimeout(context.Background(), vmProvisionTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			msg := sanitizeProvisionError(fmt.Errorf("internal panic during provisioning: %v", r), plainPassword)
			if uerr := s.vmRepo.SetProvisionError(ctx, vm.ID, msg); uerr != nil {
				slog.Error("could not persist provision_error", "vm_id", vm.ID, "error", uerr)
			}
			slog.Error("vm provisioning panicked",
				"vm_id", vm.ID,
				"node", node.Name,
				"pve_node", nodeName(node),
				"error", msg,
			)
		}
	}()
	if err := s.provision(ctx, vm, node, image, storageType, pool, plainPassword, ipAddr); err != nil {
		slog.Error("vm provisioning failed",
			"vm_id", vm.ID,
			"node", node.Name,
			"pve_node", nodeName(node),
			"error", err,
		)
	}
}

// provision 执行单步创建链（设计 D5）：先 NextVMID，然后一次 CreateVM 调用
// 携带 scsi0 的 import-from 磁盘、cloud-init 数据盘（ide2）、vmbr0 网络以及
// cloud-init 注入（ciuser/cipassword/ipconfig0/nameserver）；再对 qmcreate
// 任务执行 WaitTask；当导入镜像小于请求大小时将磁盘扩展到请求大小；最后
// 更新 pve_vmid/disk_gb 元数据。每次失败都通过 SetProvisionError 以脱敏消息
// 持久化（明文 cloud-init 密码绝不会进入数据库或日志）。
func (s *VMService) provision(ctx context.Context, vm model.VM, node model.PVENode,
	image *model.Image, storageType *model.StorageType, pool model.IPPool,
	plainPassword, ipAddr string) error {
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)

	vmid, err := client.NextVMID(ctx)
	if err != nil {
		return s.failProvision(ctx, vm.ID, 0, "next vmid", err, plainPassword)
	}

	imagePath := image.NodeImages[nodeName(node)]
	if imagePath == "" {
		return s.failProvision(ctx, vm.ID, 0, "image path",
			fmt.Errorf("image %q has no storage path for node %q", image.Name, nodeName(node)), plainPassword)
	}

	prefix, err := netip.ParsePrefix(pool.NetworkCIDR)
	if err != nil {
		return s.failProvision(ctx, vm.ID, 0, "pool prefix",
			fmt.Errorf("pool %d has invalid network_cidr %q: %v", pool.ID, pool.NetworkCIDR, err), plainPassword)
	}

	upid, err := client.CreateVM(ctx, nodeName(node), pve.CreateVMParams{
		VMID:       int64(vmid),
		Name:       vm.Name,
		Memory:     vm.MemMB,
		Cores:      vm.CPU,
		Scsi0:      pve.DiskImportString(storageType.PVEStorage, imagePath),
		IDE2:       storageType.PVEStorage + ":cloudinit",
		Net0:       "virtio,bridge=vmbr0",
		BootDisk:   "scsi0",
		ScsiHW:     "virtio-scsi-pci",
		CIUser:     image.DefaultUser,
		CIPassword: plainPassword,
		IPConfig0:  fmt.Sprintf("ip=%s/%d,gw=%s", ipAddr, prefix.Bits(), pool.Gateway),
		Nameserver: pool.DNS,
	})
	if err != nil {
		return s.failProvision(ctx, vm.ID, int64(vmid), "create", err, plainPassword)
	}

	if _, err := client.WaitTask(ctx, nodeName(node), upid, 0, 0); err != nil {
		return s.failProvision(ctx, vm.ID, int64(vmid), "wait create", err, plainPassword)
	}

	// 导入的镜像可能小于请求的大小；此时磁盘会被扩展到 disk_gb。当镜像至少
	// 与请求大小相同时，持久化的是实际大小。
	diskGB := vm.DiskGB
	cfg, err := client.GetVMConfig(ctx, nodeName(node), int64(vmid))
	if err != nil {
		return s.failProvision(ctx, vm.ID, int64(vmid), "read config", err, plainPassword)
	}
	boot := cfg.BootDisk()
	if boot == "" {
		boot = "scsi0"
	}
	if actual, perr := parseDiskSizeGB(cfg.String(boot)); perr == nil {
		if vm.DiskGB > actual {
			upid, err := client.ResizeDisk(ctx, nodeName(node), int64(vmid), boot, vm.DiskGB)
			if err != nil {
				return s.failProvision(ctx, vm.ID, int64(vmid), "resize disk", err, plainPassword)
			}
			if upid != "" {
				if _, err := client.WaitTask(ctx, nodeName(node), upid, 0, 0); err != nil {
					return s.failProvision(ctx, vm.ID, int64(vmid), "wait resize", err, plainPassword)
				}
			}
			diskGB = vm.DiskGB
		} else {
			diskGB = actual
		}
	}

	if err := s.vmRepo.UpdateVMPVEVMID(ctx, vm.ID, int64(vmid), diskGB); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 供给期间 VM 行被销毁；PVE VM 成为节点上的孤儿，需要人工清理。
			return fmt.Errorf("provision vm %d: row deleted during provisioning (orphaned pve vmid %d)", vm.ID, vmid)
		}
		return fmt.Errorf("provision vm %d: persist vmid: %w", vm.ID, err)
	}
	return nil
}

// parseDiskSizeGB 将 PVE 磁盘字符串的 size 字段
// （"local-lvm:vm-100-disk-0,size=10G"）转换为整 GiB。
func parseDiskSizeGB(diskString string) (int64, error) {
	size := ""
	for _, part := range strings.Split(diskString, ",") {
		if v, ok := strings.CutPrefix(part, "size="); ok {
			size = v
			break
		}
	}
	if size == "" {
		return 0, fmt.Errorf("no size in disk string %q", diskString)
	}
	bytes, err := pve.ParseSize(size)
	if err != nil {
		return 0, err
	}
	gb := bytes / (1 << 30)
	if gb == 0 && bytes > 0 {
		gb = 1
	}
	return gb, nil
}

// failProvision 将供给失败以脱敏消息持久化到 vms.provision_error，并返回供
// 日志使用的脱敏错误。按设计，失败时 IP 保持已分配状态（设计文档 Risks：
// 不自动释放，由运维人工回收脏地址）。
//
// vmid 是 NextVMID 分配的 PVE VMID（当链在得知 VMID 之前就失败时为 0）。
// 一旦存在 VMID，消息会内嵌它（以及 create 之后步骤的 "create succeeded"
// 标记），以便运维定位并清理节点上半成品 VM。
//
// 先拼接步骤前缀，再对整条消息做脱敏（先脱敏后截断），因此冗长的 PVE 错误
// 永远不会把存储值推过 maxProvisionErrorLen，前缀本身也绝不会被截掉——与
// provisionVM 中 recover 分支的长度规则一致。
func (s *VMService) failProvision(ctx context.Context, vmID, vmid int64, step string, err error, plainPassword string) error {
	var msg string
	switch {
	case vmid == 0:
		msg = fmt.Sprintf("%s: %s", step, err)
	case step == "create":
		msg = fmt.Sprintf("create (vmid=%d) failed: %s", vmid, err)
	default:
		msg = fmt.Sprintf("create succeeded (vmid=%d) but %s failed: %s", vmid, step, err)
	}
	msg = sanitizeProvisionError(errors.New(msg), plainPassword)
	if uerr := s.vmRepo.SetProvisionError(ctx, vmID, msg); uerr != nil {
		slog.Error("could not persist provision_error", "vm_id", vmID, "error", uerr)
	}
	return fmt.Errorf("provision vm %d: %s", vmID, msg)
}

// sanitizeProvisionError 从错误消息中脱敏掉给定机密（cloud-init 密码）的
// 每一次出现，并限制其长度，因此 vms.provision_error 和日志绝不会携带密码
// 或无界的 PVE dump。脱敏先于截断执行，因此跨越长度边界的机密绝不会被半
// 存储；截断按 rune 边界切割，多字节 UTF-8 字符绝不会被切成非法序列
// （vms.provision_error 列会拒绝它们，Postgres 22021）。
func sanitizeProvisionError(err error, secrets ...string) string {
	msg := err.Error()
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, "[redacted]")
	}
	r := []rune(msg)
	if len(r) > maxProvisionErrorLen {
		msg = string(r[:maxProvisionErrorLen])
	}
	return msg
}

// vmAndNode 加载 VM（缺失的行映射为 not_found）及其节点。pve_vmid 仍为零
// 的 VM 尚未完成供给，会产生 KindVMNotReady。
func (s *VMService) vmAndNode(ctx context.Context, id int64) (*repository.VMWithIP, *model.PVENode, error) {
	vm, err := s.vmRepo.GetVM(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, notFoundf("vm %d not found", id)
		}
		return nil, nil, fmt.Errorf("get vm %d: %w", id, err)
	}
	if vm.VM.PVEVmid == 0 {
		return nil, nil, vmNotReadyf("vm %d is not provisioned yet", id)
	}
	node, err := s.nodeRepo.GetNode(ctx, vm.VM.NodeID)
	if err != nil {
		return nil, nil, fmt.Errorf("get node %d of vm %d: %w", vm.VM.NodeID, id, err)
	}
	return vm, node, nil
}

// mapPVEOpError 将生命周期操作失败转换为服务错误：PVE 404 表示 pve_vmid
// 在节点上已不再指向任何实体（VM 在服务之外被移除），以 vm_not_ready 呈现；
// 其余失败保持普通错误（由处理器呈现为通用的 500）。
func mapPVEOpError(err error, op string, id int64) error {
	var upErr *pve.UpstreamError
	if errors.As(err, &upErr) && upErr.StatusCode == http.StatusNotFound {
		return vmNotReadyf("vm %d does not exist on the pve node (cannot %s)", id, op)
	}
	return fmt.Errorf("%s vm %d: %w", op, id, err)
}

// Start 启动 VM（POST /nodes/{node}/qemu/{vmid}/status/start）。PVE 任务 ID
// 不对外暴露：调用方没有可轮询它的对象，且 VM 的真实状态反正会通过透传读取
// （批次 8）。
func (s *VMService) Start(ctx context.Context, id int64) error {
	vm, node, err := s.vmAndNode(ctx, id)
	if err != nil {
		return err
	}
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	if _, err := client.StartVM(ctx, nodeName(*node), vm.VM.PVEVmid); err != nil {
		return mapPVEOpError(err, "start", id)
	}
	return nil
}

// Stop 关闭 VM（POST .../status/stop）。force=false 执行干净的 ACPI 关机；
// PVE 侧的强制停机留给运维自行操作。
func (s *VMService) Stop(ctx context.Context, id int64) error {
	vm, node, err := s.vmAndNode(ctx, id)
	if err != nil {
		return err
	}
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	if _, err := client.StopVM(ctx, nodeName(*node), vm.VM.PVEVmid, false); err != nil {
		return mapPVEOpError(err, "stop", id)
	}
	return nil
}

// Restart 重启 VM（POST .../status/reboot）。
func (s *VMService) Restart(ctx context.Context, id int64) error {
	vm, node, err := s.vmAndNode(ctx, id)
	if err != nil {
		return err
	}
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	if _, err := client.RebootVM(ctx, nodeName(*node), vm.VM.PVEVmid); err != nil {
		return mapPVEOpError(err, "restart", id)
	}
	return nil
}

// Destroy 删除 VM：先销毁 PVE VM（purge=true，任务在 DestroyVM 内部等待
// 完成）；仅当成功后才在单个事务内释放已抢占的 IP 并删除 vms 行（migration
// 0002 约定：先释放后删除）。任何 PVE 失败都会中止销毁并同时保留数据库记录
// 与 IP，以便运维检查或重试——但 PVE 404 除外（VM 已在 PVE 侧被移除，例如
// 被运维手动删除），它被视为"已销毁"并继续本地清理。从未到达 PVE 的 VM
// （pve_vmid == 0）会跳过 PVE 调用，仅清理本地记录。
func (s *VMService) Destroy(ctx context.Context, id int64) error {
	vm, err := s.vmRepo.GetVM(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return notFoundf("vm %d not found", id)
		}
		return fmt.Errorf("destroy vm %d: get: %w", id, err)
	}
	if vm.VM.PVEVmid > 0 {
		node, err := s.nodeRepo.GetNode(ctx, vm.VM.NodeID)
		if err != nil {
			return fmt.Errorf("destroy vm %d: get node: %w", id, err)
		}
		client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
		if _, err := client.DestroyVM(ctx, nodeName(*node), vm.VM.PVEVmid, true); err != nil {
			var upErr *pve.UpstreamError
			if errors.As(err, &upErr) && upErr.StatusCode == http.StatusNotFound {
				// PVE VM 已不存在（在服务之外被移除）；下面的本地清理仍会执行。
			} else {
				return fmt.Errorf("destroy vm %d on pve: %w (vm record and ip kept)", id, err)
			}
		}
	}

	tx, err := s.beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("destroy vm %d: begin tx: %w", id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.ipPoolRepo.ReleaseIPByVMTx(ctx, tx, id); err != nil {
		return fmt.Errorf("destroy vm %d: release ip: %w", id, err)
	}
	if err := s.vmRepo.DeleteVMTx(ctx, tx, id); err != nil {
		return fmt.Errorf("destroy vm %d: delete row: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("destroy vm %d: commit: %w", id, err)
	}
	return nil
}

// validateResizeSpec 对照 VM 当前值校验调整请求：至少一个字段必须存在且
// 为正数；磁盘只能增大——更小的 disk_gb 会以 KindDiskShrinkNotAllowed 被
// 拒绝。相等的 disk_gb 被允许并视为无操作（不会为其发起 resize 调用），从而
// 使总是发送完整规格的调用方获得幂等的调整行为。cpu 和 mem 可增可减。
func validateResizeSpec(cpu *int, memMB, diskGB *int64, current model.VM) error {
	if cpu == nil && memMB == nil && diskGB == nil {
		return badRequestf("at least one of cpu, mem_mb, disk_gb is required")
	}
	if cpu != nil && *cpu <= 0 {
		return badRequestf("cpu must be > 0")
	}
	if memMB != nil && *memMB <= 0 {
		return badRequestf("mem_mb must be > 0")
	}
	if diskGB != nil && *diskGB <= 0 {
		return badRequestf("disk_gb must be > 0")
	}
	if diskGB != nil && *diskGB < current.DiskGB {
		return diskShrinkNotAllowedf("disk size cannot be reduced from %dG to %dG", current.DiskGB, *diskGB)
	}
	return nil
}

// Resize 调整 VM 规格：cpu 和 mem 通过 SetVMConfig 变更（在 PVE 7/8/9 上
// 同步生效），更大的磁盘通过 ResizeDisk 调整。变更先应用到 PVE，之后才
// 持久化到 vms 行。持久化步骤以开始时读取的规格作为乐观锁（UpdateSpec 在
// WHERE 子句中重新检查它）：当期间有并发的调整先提交，调用方会得到
// KindConflict 并可重试。返回的记录携带本次调用实际应用的规格（请求的字段
// 取新值，其余为开始时读取的值）；它不是从数据库重新读取的新数据——需要
// 最新持久化行或实时透传状态的调用方必须自行通过 GetVM 获取。
func (s *VMService) Resize(ctx context.Context, id int64, cpu *int, memMB, diskGB *int64) (*repository.VMWithIP, error) {
	vm, node, err := s.vmAndNode(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validateResizeSpec(cpu, memMB, diskGB, vm.VM); err != nil {
		return nil, err
	}

	// PVE 侧变更成功之后需要持久化的规格：请求的字段取新值，其余保持读取时
	// 的值。此处读取的值同时充当 UpdateSpec 的乐观锁基线。
	next := vm.VM
	if cpu != nil {
		next.CPU = *cpu
	}
	if memMB != nil {
		next.MemMB = *memMB
	}
	if diskGB != nil {
		next.DiskGB = *diskGB
	}

	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)

	changed := false
	if (cpu != nil && *cpu != vm.VM.CPU) || (memMB != nil && *memMB != vm.VM.MemMB) {
		params := pve.VMConfigParams{}
		if cpu != nil {
			c := *cpu
			params.Cores = &c
		}
		if memMB != nil {
			m := *memMB
			params.MemoryMB = &m
		}
		if _, err := client.SetVMConfig(ctx, nodeName(*node), vm.VM.PVEVmid, params); err != nil {
			return nil, fmt.Errorf("resize vm %d: set config: %w", id, err)
		}
		changed = true
	}

	if diskGB != nil && *diskGB > vm.VM.DiskGB {
		cfg, err := client.GetVMConfig(ctx, nodeName(*node), vm.VM.PVEVmid)
		if err != nil {
			return nil, fmt.Errorf("resize vm %d: read config: %w", id, err)
		}
		boot := cfg.BootDisk()
		if boot == "" {
			boot = "scsi0"
		}
		upid, err := client.ResizeDisk(ctx, nodeName(*node), vm.VM.PVEVmid, boot, *diskGB)
		if err != nil {
			return nil, fmt.Errorf("resize vm %d: resize disk: %w", id, err)
		}
		if upid != "" {
			if _, err := client.WaitTask(ctx, nodeName(*node), upid, 0, 0); err != nil {
				return nil, fmt.Errorf("resize vm %d: wait resize: %w", id, err)
			}
		}
		changed = true
	}

	if !changed {
		return vm, nil // 无操作（例如 disk_gb 与当前值相等）
	}

	if err := s.vmRepo.UpdateSpec(ctx, id, next.CPU, next.MemMB, next.DiskGB,
		vm.VM.CPU, vm.VM.MemMB, vm.VM.DiskGB); err != nil {
		if errors.Is(err, repository.ErrSpecConflict) {
			return nil, conflictf("规格已被并发修改，请重试")
		}
		return nil, fmt.Errorf("resize vm %d: persist spec: %w", id, err)
	}
	// 已持久化的规格与 next 相同（UpdateSpec 以它为参数成功），因此直接返回
	// 预构建的记录而不重新读取该行：需要实时透传状态的处理器反正会通过 GetVM
	// 重新读取，而省掉额外查询可使 PATCH 路径在 PVE 侧变更后保持单次数据库
	// 往返。
	return &repository.VMWithIP{VM: next, IP: vm.IP}, nil
}
