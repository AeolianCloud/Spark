package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// KindVMNotFoundOnNode：节点 PVE 可达，但请求的 pve_vmid 不在该节点上
	//（导入已有 VM 或操作 external VM 时的资源不存在，区别于 zone/node
	// 自身的 not_found）。
	KindVMNotFoundOnNode ErrorKind = 105
	// KindVMAlreadyManaged：该节点上的 pve_vmid 已被托管，重复导入被拒绝
	//（区别于一般资源冲突的 KindConflict）。
	KindVMAlreadyManaged ErrorKind = 106
	// KindInvalidVMRef：路径 id 无法解析为数字本地行 id 或 ext-{nodeID}-{vmid}
	// 合成标识。
	KindInvalidVMRef ErrorKind = 107
	// KindOperationLogFailed：PVE 已受理操作，但操作记录写入失败（设计 D5
	// 的审计完整性优先：返回 500，前端提示可刷新确认）。
	KindOperationLogFailed ErrorKind = 108
	// KindNoAvailableIPPool：创建 VM 时区域没有 IP 池，或指定/遍历的池的
	// 白名单∩启用节点候选集合为空（区域池配置缺失）；区别于 KindNodeUnavailable
	// （有池有候选但可达性探测失败）。池配置是创建 VM 的前置条件，即使镜像
	// 也未下载，也优先暴露该错误。
	KindNoAvailableIPPool ErrorKind = 110
	// KindStorageNotAvailableInZone：区域存在池候选节点，但所选存储的节点
	// 挂载快照（nodes）非空且与全部池候选节点无交集——所选存储没有挂载在
	// 任何可调度的节点上（bad_request 类，与 KindImageNotAvailable 同构；
	// 区分于 KindNoAvailableIPPool 的池配置缺失）。
	KindStorageNotAvailableInZone ErrorKind = 111
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

// vmNotFoundOnNodef 构造一个 KindVMNotFoundOnNode 服务错误。
func vmNotFoundOnNodef(format string, args ...any) *Error {
	return &Error{Kind: KindVMNotFoundOnNode, Message: fmt.Sprintf(format, args...)}
}

// vmAlreadyManagedf 构造一个 KindVMAlreadyManaged 服务错误。
func vmAlreadyManagedf(format string, args ...any) *Error {
	return &Error{Kind: KindVMAlreadyManaged, Message: fmt.Sprintf(format, args...)}
}

// invalidVMReff 构造一个 KindInvalidVMRef 服务错误。
func invalidVMReff(format string, args ...any) *Error {
	return &Error{Kind: KindInvalidVMRef, Message: fmt.Sprintf(format, args...)}
}

// operationLogFailedf 构造一个 KindOperationLogFailed 服务错误。
func operationLogFailedf(format string, args ...any) *Error {
	return &Error{Kind: KindOperationLogFailed, Message: fmt.Sprintf(format, args...)}
}

// noAvailableIPPoolf 构造一个 KindNoAvailableIPPool 服务错误；消息由调用点
// 区分两种子因：「区域无池」（"no available ip pool in zone %d"）与「池
// 白名单∩启用节点为空」（"ip pool %d has no candidate nodes in zone %d"）。
func noAvailableIPPoolf(format string, args ...any) *Error {
	return &Error{Kind: KindNoAvailableIPPool, Message: fmt.Sprintf(format, args...)}
}

// storageNotAvailablef 构造一个 KindStorageNotAvailableInZone 服务错误：
// 所选存储的节点挂载快照非空，但没有任何池候选节点挂载了它——磁盘发往
// 未挂载该存储的节点必然失败，创建被拒绝（bad_request 类）。
func storageNotAvailablef(format string, args ...any) *Error {
	return &Error{Kind: KindStorageNotAvailableInZone, Message: fmt.Sprintf(format, args...)}
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
	// maxOperationErrorLen 限制 vm_operations.error_message 的最大长度（字符
	// 数），与迁移 0008 的 VARCHAR(1000) 列约束一致：落库前截断保证永不触发
	// Postgres 的字符串超长错误（SQLSTATE 22001）。
	maxOperationErrorLen = 1000
	// importVMBudget 限制 ImportVM 整个导入流程（ListVMs + GetVMConfig +
	// 事务落库）的请求级总预算：与 ListVMs 的 listVMsTimeout 相同的部分
	// 失败语义——预算耗尽时 PVE 调用以 context 错误失败，映射为
	// node_unavailable。
	importVMBudget = 30 * time.Second
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

// Identity 是已鉴权身份在服务层的表示（设计 D4/D5）：Role 为身份域
// （admin/user），ID 为身份在对应表（admins/users）中的 ID。api 层负责从
// gin.Context（middleware.IdentityKey）读取并转换为本类型。
type Identity struct {
	Role string
	ID   int64
}

// IsAdmin 报告身份是否为管理员（分流与归属校验的入口）。fail-closed（M1）：
// nil 身份一律按非管理员处理（最小权限语义），绝不放行为管理员；生产环境
// 所有业务路由均经 requireAuth 注入身份，nil 只出现在测试与内部直调路径。
func (i *Identity) IsAdmin() bool {
	return i != nil && i.Role == RoleAdmin
}

// UserLookupRepository 是 VMService 校验可选归属用户（vms.user_id，设计
// D3/D6）所需的最小查询接口：创建/认领时 user_id 非空须指向存在且启用的
// 用户。repository.AuthRepository 满足该接口。
type UserLookupRepository interface {
	GetUserByID(ctx context.Context, id int64) (*model.User, error)
}

// VMRepository 是 VMService 依赖的 vms 数据访问层。
type VMRepository interface {
	CreateVMTx(ctx context.Context, tx pgx.Tx, vm model.VM) (*model.VM, error)
	// ImportVMTx 在调用方的事务内插入一条 pve_vmid 非零的已导入 VM 行
	// （image_id/storage_type_id/password_encrypted 为 NULL）。
	ImportVMTx(ctx context.Context, tx pgx.Tx, vm model.VM) (*model.VM, error)
	// GetVMByNodeVMID 返回节点上指定 pve_vmid 的已托管 VM（导入幂等检查）；
	// 无该行时返回 pgx.ErrNoRows。
	GetVMByNodeVMID(ctx context.Context, nodeID, vmid int64) (*model.VM, error)
	GetVM(ctx context.Context, id int64) (*repository.VMWithIP, error)
	// ListVMs 返回本地全部 VM 行（含 IP）：列表合并需要与每节点 PVE 全量
	// 摘要做差集（设计 D1/D3），分页在合并排序后由服务层统一执行。
	ListVMs(ctx context.Context) ([]repository.VMWithIP, error)
	// ListVMsByUser 返回归属于指定用户的本地 VM 行（含 IP，设计 D5）：用户
	// 视角的列表分流（external 条目对用户天然排除）。
	ListVMsByUser(ctx context.Context, userID int64) ([]repository.VMWithIP, error)
	SetVMIPIDTx(ctx context.Context, tx pgx.Tx, id, ipID int64) error
	UpdateVMPVEVMID(ctx context.Context, id, vmid, diskGB int64) error
	SetProvisionError(ctx context.Context, id int64, message string) error
	UpdateSpec(ctx context.Context, id int64, newCPU int, newMemMB, newDiskGB int64, oldCPU int, oldMemMB, oldDiskGB int64) error
	DeleteVMTx(ctx context.Context, tx pgx.Tx, id int64) error
}

// VMOperationRepository 是 VMService 依赖的 vm_operations 数据访问层
// （生命周期操作的审计记录，设计 D5）。
type VMOperationRepository interface {
	CreateOperation(ctx context.Context, op model.VMOperation) (*model.VMOperation, error)
	// ListOperations 按时间倒序分页返回指定 (node_id, pve_vmid) 的操作记录
	// 及匹配总数。
	ListOperations(ctx context.Context, nodeID, vmid int64, limit, offset int) ([]model.VMOperation, int, error)
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
	// ListNodesByIDs 返回指定 id 集合中的节点（不存在的 id 被忽略），供
	// 列表合并把被禁用/已移除节点的 id 翻译为节点名（警告的 Node 字段）。
	ListNodesByIDs(ctx context.Context, ids []int64) ([]model.PVENode, error)
}

// VMIPPoolRepository 是 VMService 依赖的 IP 池数据访问层。
type VMIPPoolRepository interface {
	GetPool(ctx context.Context, id int64) (*model.IPPool, error)
	ListPoolsByZone(ctx context.Context, zoneID int64) ([]model.IPPool, error)
	GetPoolNodes(ctx context.Context, poolID int64) ([]model.PVENode, error)
	ClaimFreeIP(ctx context.Context, tx pgx.Tx, poolID int64, vmID *int64) (model.IP, error)
	// ClaimIPByAddressTx 在调用方的事务内按地址精确领取空闲地址（导入时
	// 优先复用 PVE 静态 IP）；地址不在池内时返回 pgx.ErrNoRows，被并发
	// 抢占时返回 repository.ErrAllocationRetry。
	ClaimIPByAddressTx(ctx context.Context, tx pgx.Tx, poolID int64, ipAddr string, vmID *int64) (model.IP, error)
	ReleaseIPByVMTx(ctx context.Context, tx pgx.Tx, vmID int64) error
}

// VMImageRepository 是 VMService 依赖的镜像数据访问层。
type VMImageRepository interface {
	Get(ctx context.Context, id int64) (*model.Image, error)
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
	// UserID 可选归属用户（vms.user_id，设计 D3）：nil 表示无主 VM；
	// 非 nil 时 CreateVM 校验用户存在且启用（禁用用户不得获得新资源）。
	UserID *int64 `json:"user_id,omitempty"`
	// PoolID 可选 IP 池（设计 2）：nil 表示缺省自动遍历区域全部池；
	// 非 nil 时限定只在该池调度，CreateVM 校验其存在且属于该区域。
	PoolID *int64 `json:"pool_id,omitempty"`
}

// validateCreateVMRequest 强制创建校验中与存在性无关的部分：名称、正数规格
// 以及非空密码。存在性检查在 CreateVM 中按顺序执行（镜像/存储/区域先查，
// 镜像可用性由节点选择阶段决定），与文档化的校验顺序一致。
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
// 供给链）、启动/停止/重启、销毁（含 IP 释放）以及规格变更。生命周期操作
// 对本地行与外部 VM（ext- 合成标识，设计 D2）一视同仁，操作受理后写入
// 审计记录（设计 D5）。
type VMService struct {
	beginner    TxBeginner
	vmRepo      VMRepository
	opRepo      VMOperationRepository
	userRepo    UserLookupRepository
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
// 存储前加密 cloud-init 密码）。userRepo 用于校验创建/认领时的可选归属用户
// （vms.user_id，设计 D3）。
func NewVMService(beginner TxBeginner, vmRepo VMRepository, opRepo VMOperationRepository,
	ipPoolRepo VMIPPoolRepository, zoneRepo VMZoneRepository, nodeRepo VMNodeRepository,
	imageRepo VMImageRepository, storageRepo VMStorageTypeRepository,
	userRepo UserLookupRepository, cipher *crypto.Cipher) *VMService {
	s := &VMService{
		beginner:    beginner,
		vmRepo:      vmRepo,
		opRepo:      opRepo,
		userRepo:    userRepo,
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
// identity 用于归属约束（H1，设计 D5）：user 令牌只能把 user_id 指定为自身
// 或留空（留空默认归属自身）；admin 可任意指定或留空（无主）。
//
// 供给 goroutine 不得借用调用方的 context（HTTP 处理器返回时该 context 会
// 被取消），因此它在受 vmProvisionTimeout 限制的分离式后台 context 下运行。
func (s *VMService) CreateVM(ctx context.Context, identity *Identity, req CreateVMRequest) (*repository.VMWithIP, error) {
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
	// 3b. 跨 zone 归属校验（与 ip pool 的 zone 归属语义一致）：存储类型
	// 必须属于请求区域，否则按资源不存在 404 处理——一个 zone 对应一个
	// PVE 集群，防止把其他集群的存储塞进本区域的 VM。归属校验先于可用性
	// 两道闸执行（与 ip pool 的"存在→归属"惯例一致）：跨 zone 时优先
	// 404 而非 400。
	if storageType.ZoneID != req.ZoneID {
		return nil, notFoundf("storage type %d not found in zone %d", req.StorageTypeID, req.ZoneID)
	}
	// 3c. 存储可用性两道闸（设计 D5）：所选存储必须处于启用状态且支持
	// images 内容类型（content 快照为 NULL/空时视为不支持）；校验发生在
	// 异步供给链之前，错误直接返回调用方。
	if !storageType.Enabled {
		return nil, badRequestf("storage type is disabled")
	}
	if !storageTypeSupportsImages(storageType.Content) {
		return nil, badRequestf("storage type cannot store VM disks")
	}
	// 4. 校验密码与规格。
	if err := validateCreateVMRequest(req); err != nil {
		return nil, err
	}
	// 5. 归属用户解析与校验（设计 D3/D5 + H1）：user 令牌只能把 user_id
	// 指定为自身或留空（留空默认归属自身）；admin 可任意指定或留空（无主）。
	// 解析后的归属用户须存在且启用（禁用用户无法登录，也不应获得新资源）。
	uid, err := resolveVMCreationUser(identity, req.UserID)
	if err != nil {
		return nil, err
	}
	if err := s.validateUserForVM(ctx, uid); err != nil {
		return nil, err
	}

	// 6. 可选池校验（设计 2）：指定 pool_id 时必须为正整数、存在且属于该
	// 区域（指定不属于本区域的池按资源不存在 404 处理）；通过后限定只在
	// 该池调度，缺省保持自动遍历区域全部池。
	if req.PoolID != nil {
		if *req.PoolID <= 0 {
			return nil, badRequestf("pool_id must be a positive integer")
		}
		pool, err := s.ipPoolRepo.GetPool(ctx, *req.PoolID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, notFoundf("ip pool %d not found in zone %d", *req.PoolID, req.ZoneID)
			}
			return nil, fmt.Errorf("create vm: get ip pool: %w", err)
		}
		if pool.ZoneID != req.ZoneID {
			return nil, notFoundf("ip pool %d not found in zone %d", *req.PoolID, req.ZoneID)
		}
	}

	// 节点与池的选择（D4，镜像感知调度 5.6 + 存储挂载过滤 D8）：按 id 顺序
	// 遍历区域的池；对每个池，其白名单节点与区域启用节点求交集后，先按所选
	// 存储的节点挂载快照过滤（nodes 非空时仅保留挂载了该存储的节点），再按
	// 镜像存在性过滤（仅保留 local/import 存储上存在该镜像的节点），最后从
	// 过滤结果中挑选第一个可达节点；没有可用候选的池会被跳过，继续尝试下一
	// 个池。
	pool, node, volid, err := s.selectPoolAndNode(ctx, req.ZoneID, image, storageType, req.PoolID)
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
		ImageID:           &req.ImageID,
		StorageTypeID:     &req.StorageTypeID,
		CPU:               req.CPU,
		MemMB:             req.MemMB,
		DiskGB:            req.DiskGB,
		PasswordEncrypted: passwordEncrypted,
		UserID:            uid,
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
	go s.provisionVM(vm, node, image, volid, storageType, pool, req.Password, claimed.IP)

	return &repository.VMWithIP{VM: vm, IP: claimed.IP}, nil
}

// storageTypeSupportsImages 判断存储类型的内容快照是否包含 images 内容
// 类型：content 按逗号拆分后逐项匹配（容忍空白）；content 为 NULL（nil，
// 尚未扫描的存量行）或空串时视为不支持——不能确定能放磁盘映像就不放行。
func storageTypeSupportsImages(content *string) bool {
	if content == nil {
		return false
	}
	for _, c := range strings.Split(*content, ",") {
		if strings.TrimSpace(c) == "images" {
			return true
		}
	}
	return false
}

// resolveVMCreationUser 解析创建/认领 VM 的归属用户（vms.user_id，设计
// D3/D5 + H1）：非管理员（user 令牌或未注入身份，M1 fail-closed）只能把
// user_id 指定为自身或留空——留空默认归属自身（user 创建/认领的 VM 必须
// 属于自己）；admin 可任意指定或留空（留空为无主 VM）。user 令牌指定他人
// user_id -> 403 forbidden：杜绝跨用户资源注入与 user_id 枚举（S2），后续
// validateUserForVM 的 404/400 分支只有 admin 才可能触发。nil 身份无 ID 可
// 归属，返回 0 交给 validateUserForVM 以 not_found 拒绝（身份缺失不得被放
// 行为管理员创建无主 VM）。
func resolveVMCreationUser(identity *Identity, userID *int64) (*int64, error) {
	if identity != nil && identity.IsAdmin() {
		return userID, nil
	}
	if userID != nil && (identity == nil || *userID != identity.ID) {
		return nil, forbiddenf("user cannot create vm for user %d", *userID)
	}
	if identity == nil {
		zero := int64(0)
		return &zero, nil
	}
	id := identity.ID
	return &id, nil
}

// validateUserForVM 校验可选归属用户（vms.user_id，设计 D3/D6）：nil 表示
// 无主 VM（放行）；用户不存在 -> not_found；用户被禁用 -> bad_request
// （禁用用户不得再获得任何资源，与 D5 的"禁用即拒登"语义一致）。
func (s *VMService) validateUserForVM(ctx context.Context, userID *int64) error {
	if userID == nil {
		return nil
	}
	user, err := s.userRepo.GetUserByID(ctx, *userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return notFoundf("user %d not found", *userID)
		}
		return fmt.Errorf("check user %d: %w", *userID, err)
	}
	if user.Status != model.UserStatusEnabled {
		return badRequestf("user %d is disabled", *userID)
	}
	return nil
}

// checkVMOwnership 校验用户身份对本地 VM 行的归属（设计 D5）：无主 VM
// （user_id 为 NULL）或归属他人的 VM 对用户一律 403 forbidden（无主 VM 在
// 用户视角视同不存在，与列表"仅返回归属自己的"语义一致）；归属自身放行。
// 管理员放行；nil 身份（M1 fail-closed）没有 ID 可比，一律 403 拒绝，绝不
// fail-open。
func checkVMOwnership(identity *Identity, vm *model.VM) error {
	if identity.IsAdmin() {
		return nil
	}
	if identity == nil {
		return forbiddenf("identity required to access vm %d", vm.ID)
	}
	if vm.UserID == nil || *vm.UserID != identity.ID {
		return forbiddenf("user cannot access vm %d", vm.ID)
	}
	return nil
}

// authorizeVMOperation 校验身份对生命周期目标的操作权限（设计 D5），在解析
// 目标后、调用 PVE 前执行：管理员放行；nil 身份（M1 fail-closed）按用户
// 语义处理（同 user，ID 为 0，目标一律拒绝）；用户要求目标 VM 归属自身——
// 数字 id 按本地行校验（行不存在 -> 404，与 vmAndNode 语义一致）；
// ext- 标识指向本地托管行时按该行归属校验（与 G1 的本地路由语义一致），
// 否则（纯 external，无归属）一律 403。
func (s *VMService) authorizeVMOperation(ctx context.Context, identity *Identity, t vmTarget) error {
	if identity.IsAdmin() {
		return nil
	}
	if t.external {
		local, err := s.vmRepo.GetVMByNodeVMID(ctx, t.nodeID, t.vmid)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("authorize operation on external vm %d on node %d: %w", t.vmid, t.nodeID, err)
		}
		if local == nil {
			return forbiddenf("user cannot operate external vm %s", externalVMID(t.nodeID, t.vmid))
		}
		return checkVMOwnership(identity, local)
	}
	vm, err := s.vmRepo.GetVM(ctx, t.localID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return notFoundf("vm %d not found", t.localID)
		}
		return fmt.Errorf("authorize operation on vm %d: %w", t.localID, err)
	}
	return checkVMOwnership(identity, &vm.VM)
}

// applyOperator 把身份写入操作记录的操作者字段（设计 D5/D8）：
// operator_type 为身份域（admin/user），operator_id 为对应表 ID；
// 身份缺失（nil，未注入身份的路径）时操作者字段保持空（旧记录形态）。
func applyOperator(op *model.VMOperation, identity *Identity) {
	if identity == nil {
		return
	}
	op.OperatorType = identity.Role
	id := identity.ID
	op.OperatorID = &id
}

// selectPoolAndNode 按 id 顺序遍历区域的 IP 池（D4，镜像感知调度 5.6 +
// 存储挂载过滤 D8）。对每个池，将白名单节点（ip_pool_nodes，按节点 id）与
// 区域启用节点求交集，先按所选存储的节点挂载快照过滤（storageType.Nodes
// 非空时仅保留 PVE 节点名在其中、即挂载了该存储的候选；nodes 为空 = 不
// 限制节点，不过滤），再按镜像存在性过滤：仅保留 local 存储 import 目录中
// 存在该镜像（以 DownloadURL 的 basename 匹配）的节点及其卷 ID，过滤结果
// 再走 selectNode 可达性探测，第一个可达节点胜出；没有带镜像候选的池会被
// 跳过，继续尝试下一个池。
//
// storageType 必须是 CreateVM 已校验（zone 归属/enabled/images 内容能力）
// 的所选存储类型；nil 视为"不过滤节点"（防御性处理，正常路径不会发生）。
// 挂载过滤同时作用于镜像存在性扫描（scanNodeImageVolIDs 的输入节点）：
// 未挂载该存储的节点永远不可能被选中，提前剔除可减少无效的镜像扫描调用。
//
// poolID 非空时（调用方已校验池存在且属于该区域）只遍历该池，不再列举
// 区域全部池；poolID 为空时保持缺省的全池自动遍历。两种形态下镜像扫描
// （volIDs）都会执行：既用于候选过滤，也用于失败分支的镜像/可达性区分。
//
// 返回的 volid 是所选节点上该镜像的卷 ID（如 "local:import/xxx.qcow2"），
// 由供给链用于 scsi0 的 import-from（任务 5.7），保证非空。
//
// 失败分支按优先级区分四种语义（设计 3 + D8）：
// a. 池候选集合整体为空（区域无池、指定池的白名单∩启用节点为空、或全部
//
//	池的候选均为空）-> KindNoAvailableIPPool；该分支优先于存储挂载与镜像
//	检查——池配置是创建 VM 的前置条件，即使镜像也未下载也先暴露池缺失。
//	消息区分「区域无池」与「池无候选节点」两种子因（不再新增子错误码）。
//
// b. 池有候选但存储挂载过滤后全为空（所选存储 nodes 快照非空，且没有任一
//
//	候选节点挂载它）-> KindStorageNotAvailableInZone（设计 D8）：磁盘发往
//	未挂载该存储的节点必然失败，先于镜像检查暴露。
//
// c. 存储过滤后仍有候选但区域内没有任何启用节点确认存在该镜像 -> 扫描
//
//	失败（节点不可达）时 KindNodeUnavailable，全部扫描成功却无镜像时
//	KindImageNotAvailable。
//
// d. 存储过滤后候选带镜像，但可达性探测全部失败 -> KindNodeUnavailable。
func (s *VMService) selectPoolAndNode(ctx context.Context, zoneID int64, image *model.Image, storageType *model.StorageType, poolID *int64) (model.IPPool, model.PVENode, string, error) {
	enabledNodes, err := s.nodeRepo.ListEnabledNodesByZone(ctx, zoneID)
	if err != nil {
		return model.IPPool{}, model.PVENode{}, "", fmt.Errorf("select node: list enabled nodes: %w", err)
	}
	// 存储挂载集合（设计 D8）：nodes 快照非空时，只有挂载了所选存储的节点
	// 参与镜像存在性扫描与调度；快照为空（不限制节点）返回 nil = 不过滤。
	mounted := storageMountedNodes(storageType)
	// 可观测性（11.5）：快照节点名在 zone 启用节点中无匹配时记 warning——
	// 可能原因：节点 PveName 未回填（PveName 空时以业务名 Name 匹配）、
	// 快照过期或节点被禁用。调度以快照为准，无匹配的挂载节点永远不可能被
	// 选中，warning 携带快照节点名与 zone 便于管理员定位配置问题。
	if mounted != nil {
		if unmatched := storageSnapshotUnmatched(mounted, enabledNodes); len(unmatched) > 0 {
			slog.Warn("storage mount snapshot has node names not among enabled nodes of zone",
				"zone_id", zoneID, "storage", storageType.PVEStorage,
				"snapshot_nodes", strings.Join(unmatched, ","))
		}
	}
	var pools []model.IPPool
	if poolID != nil {
		// 指定池：存在性与归属校验由 CreateVM 完成，这里重新读取完整池
		// 对象（返回给调用方时须携带 NetworkCIDR 等供给链依赖的字段），
		// 遍历逻辑与全池形态共用。
		pool, err := s.ipPoolRepo.GetPool(ctx, *poolID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return model.IPPool{}, model.PVENode{}, "", notFoundf("ip pool %d not found in zone %d", *poolID, zoneID)
			}
			return model.IPPool{}, model.PVENode{}, "", fmt.Errorf("select node: pool %d: %w", *poolID, err)
		}
		// 纵深防御：调用方（CreateVM）已校验池属于该区域，这里再校验一次，
		// 防止未来绕过 CreateVM 的调用路径把跨区池带入调度（404 语义与
		// CreateVM 保持一致）。
		if pool.ZoneID != zoneID {
			return model.IPPool{}, model.PVENode{}, "", notFoundf("ip pool %d not found in zone %d", *poolID, zoneID)
		}
		pools = []model.IPPool{*pool}
	} else {
		pools, err = s.ipPoolRepo.ListPoolsByZone(ctx, zoneID)
		if err != nil {
			return model.IPPool{}, model.PVENode{}, "", fmt.Errorf("select node: list pools: %w", err)
		}
	}

	// 镜像存在性过滤：并行扫描挂载了所选存储的启用节点（存储挂载过滤先行，
	// 未挂载节点的镜像存在性无关紧要——它们不可能被选中）的 local/import
	// 存储，建立节点 -> 镜像卷 ID 映射。扫描失败的节点（不可达）视为"未
	// 知"，不记录存在性，也不参与后续选择——它们只会影响错误区分（见下方）。
	// 镜像名复用 image_service 的 imageFileName（同包共享 helper，与
	// image_service 的匹配语义同源）：url.Parse 后取 Path 的 basename，
	// URL 带查询串时不会把查询串带进文件名，保证与扫描匹配（D2）一致。
	scanNodes := enabledNodes
	if mounted != nil {
		scanNodes = make([]model.PVENode, 0, len(enabledNodes))
		for _, n := range enabledNodes {
			if _, ok := mounted[nodeName(n)]; ok {
				scanNodes = append(scanNodes, n)
			}
		}
	}
	imageName := imageFileName(image.DownloadURL)
	volIDs, scanFailures, err := s.scanNodeImageVolIDs(ctx, scanNodes, imageName)
	if err != nil {
		return model.IPPool{}, model.PVENode{}, "", err
	}

	// hasCandidatePool 记录是否出现过候选非空的池；hasStorageCandidatePool
	// 记录存储挂载过滤后仍非空的池；poolTriedWithImage 记录是否出现过候选带
	// 镜像的池。三者按失败分支的优先级区分「无可用池」（a）、「存储未挂载」
	// （b）与「有池但镜像/可达性失败」（c/d）。
	hasCandidatePool := false
	hasStorageCandidatePool := false
	poolTriedWithImage := false
	for _, pool := range pools {
		poolNodes, err := s.ipPoolRepo.GetPoolNodes(ctx, pool.ID)
		if err != nil {
			return model.IPPool{}, model.PVENode{}, "", fmt.Errorf("select node: pool %d nodes: %w", pool.ID, err)
		}
		candidates := poolCandidates(poolNodes, enabledNodes)
		if len(candidates) == 0 {
			continue
		}
		hasCandidatePool = true
		// 存储挂载过滤（设计 D8）：剔除 PVE 节点名（nodeName，PveName
		// fallback Name）不在存储 nodes 快照中的候选；快照为空（不限制）
		// 时不过滤。
		withStorage := candidates
		if mounted != nil {
			withStorage = make([]model.PVENode, 0, len(candidates))
			for _, n := range candidates {
				if _, ok := mounted[nodeName(n)]; ok {
					withStorage = append(withStorage, n)
				}
			}
		}
		if len(withStorage) == 0 {
			continue // 该池的候选均未挂载所选存储：跳过该池，继续下一个
		}
		hasStorageCandidatePool = true
		withImage := make([]model.PVENode, 0, len(withStorage))
		for _, n := range withStorage {
			if _, ok := volIDs[n.ID]; ok {
				withImage = append(withImage, n)
			}
		}
		if len(withImage) == 0 {
			continue // 池的候选节点均无该镜像：跳过该池，继续下一个
		}
		poolTriedWithImage = true
		node, err := s.selectNode(ctx, withImage)
		if err == nil {
			return pool, node, volIDs[node.ID], nil
		}
		// KindNodeUnavailable：保留最后的错误并尝试下一个池。
	}
	// a. 池候选集合整体为空（优先级最高）：指定池时消息含池 id 与区域，
	// 未指定（区域无池或全部池均无候选）时消息含区域。
	if !hasCandidatePool {
		if poolID != nil {
			return model.IPPool{}, model.PVENode{}, "", noAvailableIPPoolf("ip pool %d has no candidate nodes in zone %d", *poolID, zoneID)
		}
		return model.IPPool{}, model.PVENode{}, "", noAvailableIPPoolf("no available ip pool in zone %d", zoneID)
	}
	// b. 池有候选但存储挂载过滤后为空（设计 D8）：所选存储（nodes 快照非空）
	// 没有挂载在任何候选节点上，磁盘无处可放——先于镜像检查暴露。
	if !hasStorageCandidatePool {
		return model.IPPool{}, model.PVENode{}, "", storageNotAvailablef("storage type %q is not available on any candidate node in zone %d", storageType.PVEStorage, zoneID)
	}
	// c. 存储过滤后仍有候选但没有任何启用节点确认存在该镜像。若存在扫描失败
	// 的节点（不可达，无法确认镜像是否存在），优先呈现节点不可达；全部扫描
	// 成功却无镜像才是镜像不可用。
	if !poolTriedWithImage && len(volIDs) == 0 {
		if len(scanFailures) > 0 {
			return model.IPPool{}, model.PVENode{}, "", nodeUnavailablef("no reachable node with image %q in zone %d", image.Name, zoneID)
		}
		return model.IPPool{}, model.PVENode{}, "", imageNotAvailablef("image %q is not available on any enabled node of zone %d", image.Name, zoneID)
	}
	// d. 池有候选且镜像存在，但可达性探测全部失败（真实的依赖故障）。
	return model.IPPool{}, model.PVENode{}, "", nodeUnavailablef("no reachable node with image %q in zone %d", image.Name, zoneID)
}

// storageMountedNodes 返回所选存储挂载的 PVE 节点名集合（与 nodeName 的
// 语义一致：PveName fallback Name），供调度按挂载过滤候选节点（设计 D8）。
// storageType 为 nil 或 Nodes 为空（快照语义：不限制节点、所有节点可用）
// 时返回 nil，调用方视为"不过滤任何节点"。
func storageMountedNodes(st *model.StorageType) map[string]struct{} {
	if st == nil || len(st.Nodes) == 0 {
		return nil
	}
	mounted := make(map[string]struct{}, len(st.Nodes))
	for _, n := range st.Nodes {
		if n = strings.TrimSpace(n); n != "" {
			mounted[n] = struct{}{}
		}
	}
	return mounted
}

// storageSnapshotUnmatched 返回挂载快照节点名（mounted）中未出现在 zone
// 启用节点（enabledNodes，按 nodeName 即 PveName fallback Name）中的名单，
// 供调度侧可观测性日志定位 PveName 未回填等配置问题（11.5）。
func storageSnapshotUnmatched(mounted map[string]struct{}, enabledNodes []model.PVENode) []string {
	enabled := make(map[string]struct{}, len(enabledNodes))
	for _, n := range enabledNodes {
		enabled[nodeName(n)] = struct{}{}
	}
	var unmatched []string
	for name := range mounted {
		if _, ok := enabled[name]; !ok {
			unmatched = append(unmatched, name)
		}
	}
	sort.Strings(unmatched)
	return unmatched
}

// scanNodeImageVolIDs 并行扫描启用节点的 local 存储 import 目录，返回
// 存在该镜像（以 imageName 的 basename 匹配）的节点到其卷 ID 的映射，以及
// 扫描失败的节点名（节点不可达，无法确认镜像是否存在）。
func (s *VMService) scanNodeImageVolIDs(ctx context.Context, nodes []model.PVENode, imageName string) (map[int64]string, []string, error) {
	type scanResult struct {
		nodeID int64
		name   string
		volID  string
		found  bool
		err    error
	}
	results := make([]scanResult, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n model.PVENode) {
			defer wg.Done()
			volID, found, err := s.nodeImageVolID(ctx, n, imageName)
			results[i] = scanResult{nodeID: n.ID, name: nodeName(n), volID: volID, found: found, err: err}
		}(i, n)
	}
	wg.Wait()

	volIDs := make(map[int64]string, len(nodes))
	var failures []string
	for _, r := range results {
		if r.err != nil {
			failures = append(failures, r.name)
			continue
		}
		if r.found {
			volIDs[r.nodeID] = r.volID
		}
	}
	return volIDs, failures, nil
}

// nodeImageVolID 扫描单个节点 local 存储的 import 目录，返回与镜像文件名
// （imageFileName 语义，与 image_service 的匹配同源）匹配的卷 ID；节点上
// 不存在该镜像时返回 found=false。
func (s *VMService) nodeImageVolID(ctx context.Context, node model.PVENode, imageName string) (string, bool, error) {
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	contents, err := client.ListStorageContent(ctx, nodeName(node), "local", "import")
	if err != nil {
		return "", false, err
	}
	for _, c := range contents {
		if matchesImageName(c.Name, imageName) {
			return c.VolID, true, nil
		}
	}
	return "", false, nil
}

// matchesImageName 判断存储内容条目是否对应给定镜像：比较文件名 basename
// 与镜像文件名（已由 imageFileName 按 URL Path 规范化），相等即匹配。
// 与 image_service 的匹配语义同源（镜像名侧共享 imageFileName helper，
// 保证创建 VM 的节点选择与扫描/下载匹配同一个文件名），容忍 PVE 返回
// 完整路径的情形。
func matchesImageName(contentName, imageName string) bool {
	return path.Base(contentName) == imageName
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
func (s *VMService) provisionVM(vm model.VM, node model.PVENode, image *model.Image, imageVolID string,
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
	if err := s.provision(ctx, vm, node, image, imageVolID, storageType, pool, plainPassword, ipAddr); err != nil {
		slog.Error("vm provisioning failed",
			"vm_id", vm.ID,
			"node", node.Name,
			"pve_node", nodeName(node),
			"error", err,
		)
	}
}

// provision 执行单步创建链（设计 D5）：先 NextVMID，然后一次 CreateVM 调用
// 携带 scsi0 的 import-from 磁盘（源为节点选择阶段返回的镜像卷 ID
// imageVolID）、cloud-init 数据盘（ide2）、vmbr0 网络以及 cloud-init 注入
// （ciuser/cipassword/ipconfig0/nameserver）；再对 qmcreate 任务执行
// WaitTask；当导入镜像小于请求大小时将磁盘扩展到请求大小；最后更新
// pve_vmid/disk_gb 元数据。每次失败都通过 SetProvisionError 以脱敏消息
// 持久化（明文 cloud-init 密码绝不会进入数据库或日志）。
func (s *VMService) provision(ctx context.Context, vm model.VM, node model.PVENode,
	image *model.Image, imageVolID string, storageType *model.StorageType, pool model.IPPool,
	plainPassword, ipAddr string) error {
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)

	vmid, err := client.NextVMID(ctx)
	if err != nil {
		return s.failProvision(ctx, vm.ID, 0, "next vmid", err, plainPassword)
	}

	// 镜像卷 ID 由节点选择阶段（selectPoolAndNode）保证非空；这里保留防御性
	// 检查，防止绕过选择阶段的调用路径在空卷 ID 下产出损坏的磁盘串。
	if imageVolID == "" {
		return s.failProvision(ctx, vm.ID, 0, "image volid",
			fmt.Errorf("image %q has no volume id for node %q", image.Name, nodeName(node)), plainPassword)
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
		Scsi0:      pve.DiskImportString(storageType.PVEStorage, imageVolID),
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

// sanitizePVEError 把 PVE 客户端错误转换为对外可展示的脱敏摘要。原始错误
// 可能携带内部细节——PVE 请求路径（如 "/nodes/pve1/qemu"）与网络层失败的
// 内部 base URL/host:port（如 "Get https://10.0.0.7:8006/api2/json/...:
// dial tcp 10.0.0.7:8006: connect: connection refused"）——违反"对外错误
// 消息不得暴露内部细节"的红线。摘要只保留：
//
//	*pve.UpstreamError -> PVE 返回的 errors 对象（或响应体）消息；
//	传输层错误 -> 错误链最末的原因段（如 "connection refused"）。
//
// 返回前统一经 truncatePVEErrorMsg 按 rune 截断（maxPVEErrorLen）：PVE
// 错误体最大可达 1MiB，脱敏后若不截断，超长错误体会进入详情 503、列表
// warnings 与节点状态降级等一切对外消息（红线：对外错误消息不得暴露内部
// 细节，且放大响应体）。
func sanitizePVEError(err error) string {
	var msg string
	var upErr *pve.UpstreamError
	if errors.As(err, &upErr) {
		if len(upErr.Errors) > 0 {
			keys := make([]string, 0, len(upErr.Errors))
			for k := range upErr.Errors {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s: %s", k, upErr.Errors[k]))
			}
			msg = strings.Join(parts, ", ")
		} else if msg = strings.TrimSpace(upErr.Body); msg == "" {
			msg = "empty response"
		}
	} else {
		// 网络层错误（节点不可达/TLS/超时）：取错误链最末一个冒号之后的段
		//（"connect: connection refused" 的最后段是 "connection refused"），
		// 剥离其中的内部地址与 URL。
		msg = err.Error()
		if i := strings.LastIndex(msg, ":"); i >= 0 {
			msg = strings.TrimSpace(msg[i+1:])
		}
		if msg == "" {
			msg = "unreachable"
		}
	}
	return truncatePVEErrorMsg(msg)
}

// sanitizeOperationError 生成失败操作审计记录（vm_operations.error_message）
// 的落库值：保留服务层自己的包装上下文（如 "start vm 1"），把错误链中的
// PVE 部分替换为脱敏摘要（sanitizePVEError），并按 maxOperationErrorLen
// 截断。先脱敏后截断（与 sanitizeProvisionError 相同的顺序），按 rune 边界
// 切割，多字节 UTF-8 字符绝不会被切成非法序列（VARCHAR 列会拒绝它们，
// Postgres 22001）。
func sanitizeOperationError(err error) string {
	msg := err.Error()
	var upErr *pve.UpstreamError
	if errors.As(err, &upErr) {
		// 定位 PVE 响应错误在完整消息中的起始位置：它之前是服务层上下文
		//（如 "start vm 1: "），保留；PVE 段替换为摘要。
		marker := upErr.Error()
		if i := strings.Index(msg, marker); i >= 0 {
			msg = msg[:i] + sanitizePVEError(upErr)
		} else {
			msg = sanitizePVEError(upErr)
		}
	} else if i := strings.Index(msg, "pve: "); i >= 0 {
		// 网络层失败同样带 "pve: METHOD /path: " 前缀：保留其之前的服务层
		// 上下文，PVE 段替换为传输层原因摘要。
		msg = msg[:i] + sanitizePVEError(errors.New(msg[i:]))
	}
	r := []rune(msg)
	if len(r) > maxOperationErrorLen {
		msg = string(r[:maxOperationErrorLen])
	}
	return msg
}

// vmTarget 是生命周期操作的目标 VM（设计 D2/D4）：数字本地行 id 或
// external 合成标识 ext-{nodeID}-{vmid}。
type vmTarget struct {
	localID int64
	nodeID  int64
	vmid    int64
	// external 为 true 时目标是 PVE 上存在、本地无记录的外部虚拟机。
	external bool
}

// vmRefNumberRe 匹配合成标识中数字组成部分的规范形态：正整数，无符号、
// 无前导零（"ext-01-005"、"ext-+1-+5" 这类歧义写法一律按非法标识拒绝）。
var vmRefNumberRe = regexp.MustCompile(`^[1-9][0-9]*$`)

// parseVMRef 解析路径 id 参数（设计 D2）：纯数字 -> 本地行 id；前缀
// ext- -> 外部合成标识 ext-{nodeID}-{vmid}（nodeID 是本地 DB 的
// pve_nodes.id）；其余格式 -> KindInvalidVMRef。
func parseVMRef(id string) (vmTarget, error) {
	if strings.HasPrefix(id, extIDPrefix) {
		rest := strings.TrimPrefix(id, extIDPrefix)
		parts := strings.Split(rest, "-")
		if len(parts) != 2 || !vmRefNumberRe.MatchString(parts[0]) || !vmRefNumberRe.MatchString(parts[1]) {
			return vmTarget{}, invalidVMReff("invalid external vm id %q, want %sext-{nodeID}-{vmid}", id, extIDPrefix)
		}
		nodeID, err1 := strconv.ParseInt(parts[0], 10, 64)
		vmid, err2 := strconv.ParseInt(parts[1], 10, 64)
		if err1 != nil || err2 != nil {
			return vmTarget{}, invalidVMReff("invalid external vm id %q, want %sext-{nodeID}-{vmid}", id, extIDPrefix)
		}
		return vmTarget{nodeID: nodeID, vmid: vmid, external: true}, nil
	}
	if !vmRefNumberRe.MatchString(id) {
		return vmTarget{}, invalidVMReff("invalid vm id %q: must be a positive integer or %sext-{nodeID}-{vmid}", id, extIDPrefix)
	}
	localID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return vmTarget{}, invalidVMReff("invalid vm id %q: must be a positive integer or %sext-{nodeID}-{vmid}", id, extIDPrefix)
	}
	return vmTarget{localID: localID}, nil
}

// vmAndNode 加载本地 VM（缺失的行映射为 not_found）及其节点。pve_vmid 仍为
// 零的 VM 尚未完成供给，会产生 KindVMNotReady。
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

// resolveVMTarget 把解析后的目标解析为可直接调用 PVE 的连接信息（节点 +
// PVE VMID，设计 D4）：
//
//	本地行   -> 现有 vmAndNode 路径（pve_vmid == 0 拒绝为 vm_not_ready）
//	ext- 标识 -> 按 nodeID 反查节点（缺失 -> not_found，禁用 -> node_unavailable），
//	           并校验 pve_vmid 在该节点 PVE 上存在（缺失 -> vm_not_found_on_node，
//	           节点查询失败 -> node_unavailable；PVE 模板同样拒绝）。
//
// external 分支返回的 localID 非零表示该 (nodeID, pve_vmid) 已有本地托管行
// （PVE 是真相源，列表以差集判定，ext- 标识可能指向已托管 VM）：调用方应
// 把操作路由到本地行流程，保证销毁的本地清理（IP 释放与行删除）与操作错误
// 映射（本地行 -> vm_not_ready）的一致性。
func (s *VMService) resolveVMTarget(ctx context.Context, t vmTarget) (*model.PVENode, int64, int64, error) {
	if !t.external {
		vm, node, err := s.vmAndNode(ctx, t.localID)
		if err != nil {
			return nil, 0, 0, err
		}
		return node, vm.VM.PVEVmid, 0, nil
	}
	node, err := s.nodeRepo.GetNode(ctx, t.nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, 0, notFoundf("node %d not found", t.nodeID)
		}
		return nil, 0, 0, fmt.Errorf("get node %d: %w", t.nodeID, err)
	}
	if !node.Enabled {
		return nil, 0, 0, nodeUnavailablef("node %q is disabled", nodeName(*node))
	}
	// 本地托管检查：ext- 标识指向的 (nodeID, pve_vmid) 可能已有本地行
	//（列表以 PVE 全量摘要与本地记录做差集，两者可能并存）。命中时由调用方
	// 路由到本地流程，绝不走 external 直调路径（否则 destroy 会绕过 IP 释放
	// 与行删除）。
	local, err := s.vmRepo.GetVMByNodeVMID(ctx, t.nodeID, t.vmid)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, 0, fmt.Errorf("check managed vm %d on node %d: %w", t.vmid, t.nodeID, err)
	}
	if local != nil {
		return node, t.vmid, local.ID, nil
	}
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	vms, err := client.ListVMs(ctx, nodeName(*node))
	if err != nil {
		return nil, 0, 0, nodeUnavailablef("node %q unavailable: %s", nodeName(*node), sanitizePVEError(err))
	}
	st, found := findVM(vms, t.vmid)
	if !found {
		return nil, 0, 0, vmNotFoundOnNodef("vm %d not found on node %q", t.vmid, nodeName(*node))
	}
	if st.Template == 1 {
		// PVE 模板是供克隆使用的基础镜像而非运行实体，不可操作（与列表
		// 不并入、认领拒绝的语义一致）。
		return nil, 0, 0, badRequestf("cannot operate on pve template vm %d", t.vmid)
	}
	return node, t.vmid, 0, nil
}

// mapPVEOpError 将本地 VM 的生命周期操作失败转换为服务错误：PVE 404 表示
// pve_vmid 在节点上已不再指向任何实体（VM 在服务之外被移除），以
// vm_not_ready 呈现；其余失败保持普通错误（由处理器呈现为通用的 500）。
func mapPVEOpError(err error, op string, id int64) error {
	var upErr *pve.UpstreamError
	if errors.As(err, &upErr) && upErr.StatusCode == http.StatusNotFound {
		return vmNotReadyf("vm %d does not exist on the pve node (cannot %s)", id, op)
	}
	return fmt.Errorf("%s vm %d: %w", op, id, err)
}

// mapExternalPVEOpError 将 external VM 的生命周期操作失败转换为服务错误：
// PVE 404（VM 已从节点移除）以 vm_not_found_on_node（资源不存在）呈现，
// 区别于本地行的 vm_not_ready（spec：对不存在的虚拟机执行操作 -> 资源不存在）。
func mapExternalPVEOpError(err error, op string, nodeID, vmid int64) error {
	var upErr *pve.UpstreamError
	if errors.As(err, &upErr) && upErr.StatusCode == http.StatusNotFound {
		return vmNotFoundOnNodef("vm %d not found on node %d (cannot %s)", vmid, nodeID, op)
	}
	return fmt.Errorf("%s external vm %d on node %d: %w", op, vmid, nodeID, err)
}

// recordAcceptedOperation 在 PVE 受理成功后写入审计记录（设计 D5），操作者
// 字段取自身份（operator_type/operator_id，设计 D8）。写失败返回
// KindOperationLogFailed：操作已被 PVE 受理，返回 500 保证审计完整性
// 优先，前端提示可刷新确认。
func (s *VMService) recordAcceptedOperation(ctx context.Context, identity *Identity, action string, nodeID, vmid int64) error {
	op := model.VMOperation{
		NodeID: nodeID, PVEVmid: vmid, Action: action, Result: model.VMOpResultAccepted,
	}
	applyOperator(&op, identity)
	if _, err := s.opRepo.CreateOperation(ctx, op); err != nil {
		return operationLogFailedf("%s vm %d on node %d accepted by pve but operation record write failed: %v",
			action, vmid, nodeID, err)
	}
	return nil
}

// recordFailedOperation 在 PVE 返回错误后写入 result=failed 的审计记录
// （spec：记录失败的操作），操作者字段取自身份（设计 D8）。该写入是尽力而
// 为：审计记录是次要信息，写失败只记日志，绝不掩盖向调用方返回的 PVE 错误。
// error_message 落库前经 sanitizeOperationError 脱敏与截断：绝不落库内部
// base URL/API 路径等内部细节，也不超出迁移 0008 的 VARCHAR(1000) 列约束。
func (s *VMService) recordFailedOperation(ctx context.Context, identity *Identity, action string, nodeID, vmid int64, opErr error) {
	op := model.VMOperation{
		NodeID: nodeID, PVEVmid: vmid, Action: action, Result: model.VMOpResultFailed,
		ErrorMessage: sanitizeOperationError(opErr),
	}
	applyOperator(&op, identity)
	if _, err := s.opRepo.CreateOperation(ctx, op); err != nil {
		slog.Error("could not persist failed operation record",
			"action", action, "node_id", nodeID, "pve_vmid", vmid, "error", err)
	}
}

// Start 启动 VM（POST /nodes/{node}/qemu/{vmid}/status/start），支持本地
// 行与 external 标识（设计 D4）。identity 用于操作前归属校验与操作记录写
// 操作者（设计 D5/D8）。PVE 任务 ID 不对外暴露：调用方没有可轮询
// 它的对象，且 VM 的真实状态反正会通过透传读取（批次 8）。受理成功后同步
// 写入操作记录（设计 D5）。
func (s *VMService) Start(ctx context.Context, id string, identity *Identity) error {
	return s.runLifecycleOp(ctx, id, model.VMOpActionStart, "start", identity,
		func(client *pve.Client, nodeName string, vmid int64) error {
			_, err := client.StartVM(ctx, nodeName, vmid)
			return err
		})
}

// Stop 关闭 VM（POST .../status/stop）。force=false 执行干净的 ACPI 关机；
// PVE 侧的强制停机留给运维自行操作。
func (s *VMService) Stop(ctx context.Context, id string, identity *Identity) error {
	return s.runLifecycleOp(ctx, id, model.VMOpActionStop, "stop", identity,
		func(client *pve.Client, nodeName string, vmid int64) error {
			_, err := client.StopVM(ctx, nodeName, vmid, false)
			return err
		})
}

// Restart 重启 VM（POST .../status/reboot）。
func (s *VMService) Restart(ctx context.Context, id string, identity *Identity) error {
	return s.runLifecycleOp(ctx, id, model.VMOpActionReboot, "restart", identity,
		func(client *pve.Client, nodeName string, vmid int64) error {
			_, err := client.RebootVM(ctx, nodeName, vmid)
			return err
		})
}

// runLifecycleOp 是 start/stop/reboot 三个生命周期操作的统一骨架（设计
// D4/D5）：解析标识 -> 身份归属校验（用户仅能操作归属自己的 VM，ext- 对
// 用户一律 403，设计 D5）-> 解析目标（数字行走现有路径，ext- 标识反查节点
// 并校验 PVE 存在；ext- 标识指向已托管 VM 时路由到本地行路径，保证错误映射
// 与操作记录的一致性）-> 调用 PVE -> 记录操作（成功 accepted / 失败
// failed，失败写入尽力而为；记录携带操作者，设计 D8）。PVE 404 的映射区分
// 目标类型：本地行 -> vm_not_ready，external -> vm_not_found_on_node
// （资源不存在）。
func (s *VMService) runLifecycleOp(ctx context.Context, id, action, verb string, identity *Identity,
	call func(client *pve.Client, node string, vmid int64) error) error {
	t, err := parseVMRef(id)
	if err != nil {
		return err
	}
	if err := s.authorizeVMOperation(ctx, identity, t); err != nil {
		return err
	}
	node, vmid, localID, err := s.resolveVMTarget(ctx, t)
	if err != nil {
		return err
	}
	if t.external && localID > 0 {
		// ext- 标识指向已托管 VM（PVE 为真相源，列表以差集判定）：按本地行
		// 语义执行——PVE 404 映射为 vm_not_ready、操作记录以本地行
		// (node_id, pve_vmid) 写入，与数字 id 的操作完全一致。
		t = vmTarget{localID: localID}
	}
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	if err := call(client, nodeName(*node), vmid); err != nil {
		var mapped error
		if t.external {
			mapped = mapExternalPVEOpError(err, verb, node.ID, vmid)
		} else {
			mapped = mapPVEOpError(err, verb, t.localID)
		}
		s.recordFailedOperation(ctx, identity, action, node.ID, vmid, mapped)
		return mapped
	}
	return s.recordAcceptedOperation(ctx, identity, action, node.ID, vmid)
}

// Destroy 删除 VM：数字 id 走现有本地行流程（PVE 销毁 + 事务内释放 IP 与
// 删除行，migration 0002 约定）；ext- 标识直接销毁 PVE VM（无本地行/IP 可
// 清理，设计 D4），但指向已托管 VM 时（PVE 是真相源，列表以差集判定，两者
// 可能并存）路由到本地销毁流程——否则 PVE VM 被销毁后本地行会滞留、IP 池
// 地址会永久处于 used 状态。操作前先做身份归属校验（用户仅能销毁归属自己
// 的 VM，ext- 对用户一律 403，设计 D5）。两种路径都在受理后同步写入操作
// 记录（设计 D5，携带操作者 D8）。
func (s *VMService) Destroy(ctx context.Context, id string, identity *Identity) error {
	t, err := parseVMRef(id)
	if err != nil {
		return err
	}
	if err := s.authorizeVMOperation(ctx, identity, t); err != nil {
		return err
	}
	if t.external {
		return s.destroyExternal(ctx, identity, t)
	}
	return s.destroyLocal(ctx, identity, t.localID)
}

// destroyExternal 销毁本地无记录的 external VM：解析目标（校验 PVE 存在，
// 命中本地托管行时由 resolveVMTarget 的路由语义转入 destroyLocal），调用
// PVE DestroyVM（purge=true），无本地行/IP 清理，受理后写入操作记录
// （携带操作者，设计 D8）。
func (s *VMService) destroyExternal(ctx context.Context, identity *Identity, t vmTarget) error {
	node, vmid, localID, err := s.resolveVMTarget(ctx, t)
	if err != nil {
		return err
	}
	if localID > 0 {
		// ext- 标识指向已托管 VM：路由到本地销毁流程（含 IP 释放与行删除），
		// 绝不让 PVE 销毁后本地行/IP 滞留。
		return s.destroyLocal(ctx, identity, localID)
	}
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	if _, err := client.DestroyVM(ctx, nodeName(*node), vmid, true); err != nil {
		mapped := mapExternalPVEOpError(err, "destroy", node.ID, vmid)
		s.recordFailedOperation(ctx, identity, model.VMOpActionDestroy, node.ID, vmid, mapped)
		return mapped
	}
	return s.recordAcceptedOperation(ctx, identity, model.VMOpActionDestroy, node.ID, vmid)
}

// destroyLocal 删除本地 VM：先销毁 PVE VM（purge=true，任务在 DestroyVM
// 内部等待完成）；仅当成功后才在单个事务内释放已抢占的 IP 并删除 vms 行
// （migration 0002 约定：先释放后删除）。任何 PVE 失败都会中止销毁并同时
// 保留数据库记录与 IP，以便运维检查或重试——但 PVE 404 除外（VM 已在 PVE
// 侧被移除，例如被运维手动删除），它被视为"已销毁"并继续本地清理。从未
// 到达 PVE 的 VM（pve_vmid == 0）会跳过 PVE 调用，仅清理本地记录。PVE
// 失败时写入 failed 操作记录（尽力而为），本地清理成功后写入 accepted 记录
// （设计 D5，携带操作者 D8）。
func (s *VMService) destroyLocal(ctx context.Context, identity *Identity, id int64) error {
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
				mapped := fmt.Errorf("destroy vm %d on pve: %w (vm record and ip kept)", id, err)
				s.recordFailedOperation(ctx, identity, model.VMOpActionDestroy, vm.VM.NodeID, vm.VM.PVEVmid, mapped)
				return mapped
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
	return s.recordAcceptedOperation(ctx, identity, model.VMOpActionDestroy, vm.VM.NodeID, vm.VM.PVEVmid)
}

// ListOperations 返回指定 VM 的操作记录（按时间倒序分页，设计 D5）。数字
// id 要求本地行存在（spec：查询本地不存在的 VM -> 资源不存在），取其
// node_id/pve_vmid 查询；ext- 标识直接按 node+vmid 查询（不校验 VM 当前
// 是否存在：记录是审计历史，PVE 侧可能已销毁该 VM）。操作前做身份归属校验
// （设计 D5：用户仅能查看归属自己的 VM 的操作记录，ext- 对用户一律 403，
// 除非指向本地托管行且归属自身）。
func (s *VMService) ListOperations(ctx context.Context, id string, identity *Identity, limit, offset int) ([]model.VMOperation, int, error) {
	t, err := parseVMRef(id)
	if err != nil {
		return nil, 0, err
	}
	if err := s.authorizeVMOperation(ctx, identity, t); err != nil {
		return nil, 0, err
	}
	var nodeID, vmid int64
	if t.external {
		nodeID, vmid = t.nodeID, t.vmid
	} else {
		vm, err := s.vmRepo.GetVM(ctx, t.localID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, 0, notFoundf("vm %d not found", t.localID)
			}
			return nil, 0, fmt.Errorf("list operations of vm %d: %w", t.localID, err)
		}
		nodeID, vmid = vm.VM.NodeID, vm.VM.PVEVmid
	}
	ops, total, err := s.opRepo.ListOperations(ctx, nodeID, vmid, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list operations of vm %s: %w", id, err)
	}
	return ops, total, nil
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
// KindConflict 并可重试。操作前做身份归属校验（设计 D5：用户仅能调整归属
// 自己的 VM）。返回的记录携带本次调用实际应用的规格（请求的字段取新值，
// 其余为开始时读取的值）；它不是从数据库重新读取的新数据——需要最新持久化
// 行或实时透传状态的调用方必须自行通过 GetVM 获取。
func (s *VMService) Resize(ctx context.Context, id int64, identity *Identity, cpu *int, memMB, diskGB *int64) (*repository.VMWithIP, error) {
	if err := s.authorizeVMOperation(ctx, identity, vmTarget{localID: id}); err != nil {
		return nil, err
	}
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
