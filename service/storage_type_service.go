package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
	"spark/repository"
)

// StorageTypeNodeRepository 是 StorageTypeService 扫描时依赖的节点数据
// 访问层（只读：列出 zone 的启用节点）。
type StorageTypeNodeRepository interface {
	ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error)
}

// StorageTypeZoneRepository 是 StorageTypeService 校验 zone 存在性所需的
// 只读数据访问层（repository.ZoneRepository 满足该接口，测试可注入替身）。
// 扫描（SyncZone）对不存在的 zone 直接返回 not_found，避免把"区域已删除"
// 误报成 503 node_unavailable。
type StorageTypeZoneRepository interface {
	GetZone(ctx context.Context, id int64) (*model.Zone, error)
}

// StorageTypeRepository 是 StorageTypeService 依赖的存储类型数据访问层；
// repository.StorageTypeRepository 满足该接口，测试可注入替身。
type StorageTypeRepository interface {
	// UpsertByZonePveStorage 以 (zone_id, pve_storage) 为匹配键新建或刷新
	// 一行；nodes 为该存储的节点挂载快照（空切片 = 不限制节点，设计 D8）。
	UpsertByZonePveStorage(ctx context.Context, zoneID int64, pveStorage, stype, content string, nodes []string) (*model.StorageType, bool, error)
	UpdateMeta(ctx context.Context, id int64, name *string, enabled *bool) (*model.StorageType, error)
	ListPage(ctx context.Context, zoneID *int64, limit, offset int) ([]model.StorageType, error)
	Count(ctx context.Context, zoneID *int64) (int, error)
	Get(ctx context.Context, id int64) (*model.StorageType, error)
	Delete(ctx context.Context, id int64) error
}

// ScanSummary 是一次存储扫描（SyncZone）的同步摘要。
type ScanSummary struct {
	// Created：PVE 存在而本地缺失、本次新建的存储数。
	Created int `json:"created"`
	// Updated：两边都存在、仅刷新 type/content 快照的存储数。
	Updated int `json:"updated"`
	// Deleted：PVE 已消失、本次删除的存储数。
	Deleted int `json:"deleted"`
	// Skipped：PVE 已消失但被本地 VM 引用（或并发已删）而跳过删除的存储数。
	Skipped int `json:"skipped"`
}

// maxStorageTypeNameLen 是存储类型业务名（name）的最大长度（rune 数）。
// 与 OpenAPI 契约（StorageTypeUpdateRequest.name.maxLength）保持一致。
const maxStorageTypeNameLen = 255

// StorageTypeService 实现存储类型的业务规则（提案 auto-scan-pve-storage）：
// 存储类型由扫描（SyncZone）从 PVE 权威同步产生，按 zone 归属；管理员仅
// 维护 name（业务名）与 enabled（启用开关），pve_storage/type/content/nodes
// 是扫描快照（nodes 为节点挂载快照，空 = 不限制节点，设计 D8），不可手改。
type StorageTypeService struct {
	repo     StorageTypeRepository
	nodeRepo StorageTypeNodeRepository
	zoneRepo StorageTypeZoneRepository
	// newClient 为节点构建 PVE 客户端（host/port/API 用户/token）；可注入
	// （SetClientFactory），以便测试将扫描指向假服务器。
	newClient func(host string, port int, apiUser, apiTokenSecret string) *pve.Client
	// selectNode 在 zone 的启用节点中挑选首个可达节点；可注入用于测试，
	// 生产默认使用 SelectReachableNode（扫描语义 D1：集群级 GET /storage
	// 只依赖一个可达节点）。
	selectNode func(ctx context.Context, nodes []model.PVENode) (model.PVENode, error)
}

// NewStorageTypeService 使用 repo、nodeRepo 与 zoneRepo 创建一个
// StorageTypeService。nodeRepo 提供 zone 的启用节点，供 SyncZone 挑选可达
// 节点执行集群级存储扫描；zoneRepo 供 SyncZone 校验 zone 存在性。
func NewStorageTypeService(repo StorageTypeRepository, nodeRepo StorageTypeNodeRepository, zoneRepo StorageTypeZoneRepository) *StorageTypeService {
	s := &StorageTypeService{
		repo:     repo,
		nodeRepo: nodeRepo,
		zoneRepo: zoneRepo,
		newClient: func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient(host, apiUser, apiTokenSecret, pve.WithPort(port))
		},
	}
	// 可达性探测与其他节点交互使用同一客户端工厂语义（SelectReachableNode
	// 内部自带 nodeProbeTimeout 超时），保证扫描的"选节点"与"调接口"一致。
	s.selectNode = SelectReachableNode
	return s
}

// SetClientFactory 替换用于扫描节点交互的 PVE 客户端工厂（测试、反向代理
// 场景），与 VMService.SetClientFactory 的注入模式一致。
func (s *StorageTypeService) SetClientFactory(fn func(host string, port int, apiUser, apiTokenSecret string) *pve.Client) {
	if fn != nil {
		s.newClient = fn
	}
}

// SyncZone 对指定 zone 执行一次存储扫描（设计 D1/D3/D6/D8）：先校验 zone
// 存在（不存在返回 not_found，避免误报 503 且不泄露候选节点数），从 zone
// 的启用节点中选首个可达节点，调用集群级 GET /storage 获取 PVE 存储清单，
// 然后以 (zone_id, pve_storage) 为匹配键逐条 upsert（新建默认启用、name
// 为空；已存在仅刷新 type/content/nodes 快照——nodes 为节点挂载快照，空
// 表示不限制节点，保留管理员的 name/enabled），再删除本地记录中 PVE 已
// 消失的行（被 VM 引用或并发已删的行跳过并计入 skipped）。
//
// 全部节点不可达时不产生任何同步（不落库、不删除），直接返回
// node_unavailable 错误——避免半个 zone 的存储被误删。同步采用"先逐条
// upsert、后逐条删除"的幂等方式：upsert 可重复执行，删除失败只跳过该行，
// 不影响其余存储；下一次扫描会自然收敛。
//
// 并发语义：本方法无整体事务，是逐条幂等收敛的同步——节点注册自动触发与
// 管理员手动触发可能并发执行，此时摘要计数可能失真（同一行被两次扫描
// 分别计入），但 upsert/delete 各自幂等，最终数据一致，无脏写。
func (s *StorageTypeService) SyncZone(ctx context.Context, zoneID int64) (ScanSummary, error) {
	if _, err := s.zoneRepo.GetZone(ctx, zoneID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ScanSummary{}, notFoundf("zone %d not found", zoneID)
		}
		return ScanSummary{}, fmt.Errorf("sync storage: check zone %d: %w", zoneID, err)
	}
	nodes, err := s.nodeRepo.ListEnabledNodesByZone(ctx, zoneID)
	if err != nil {
		return ScanSummary{}, fmt.Errorf("sync storage: list enabled nodes of zone %d: %w", zoneID, err)
	}
	node, err := s.selectNode(ctx, nodes)
	if err != nil {
		return ScanSummary{}, err
	}
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	storages, err := client.ListStorage(ctx)
	if err != nil {
		return ScanSummary{}, fmt.Errorf("sync storage: list storage on node %q: %w", nodeName(node), err)
	}

	var summary ScanSummary
	pveNames := make(map[string]struct{}, len(storages))
	for _, ps := range storages {
		if ps.Storage == "" {
			// 防御：无存储名的条目无法对齐，跳过（不产生任何记录）。
			slog.Warn("sync storage: skipping storage with empty name", "zone_id", zoneID)
			continue
		}
		_, created, err := s.repo.UpsertByZonePveStorage(ctx, zoneID, ps.Storage, ps.Type, ps.Content, ps.Nodes)
		if err != nil {
			return ScanSummary{}, fmt.Errorf("sync storage: upsert %q: %w", ps.Storage, err)
		}
		pveNames[ps.Storage] = struct{}{}
		if created {
			summary.Created++
		} else {
			summary.Updated++
		}
	}

	locals, err := s.repo.ListPage(ctx, &zoneID, -1, 0)
	if err != nil {
		return ScanSummary{}, fmt.Errorf("sync storage: list local storage of zone %d: %w", zoneID, err)
	}
	for _, st := range locals {
		if _, ok := pveNames[st.PVEStorage]; ok {
			continue
		}
		err := s.repo.Delete(ctx, st.ID)
		switch {
		case err == nil:
			summary.Deleted++
		case errors.Is(err, repository.ErrInUse), errors.Is(err, pgx.ErrNoRows):
			// 被本地 VM 引用（ErrInUse）或并发已被删除（ErrNoRows）的消失
			// 存储跳过删除；VM 清理后再次扫描即删除。
			summary.Skipped++
			slog.Info("sync storage: skipped deleting storage still referenced by vms",
				"zone_id", zoneID, "storage", st.PVEStorage, "id", st.ID)
		default:
			return ScanSummary{}, fmt.Errorf("sync storage: delete %q (id %d): %w", st.PVEStorage, st.ID, err)
		}
	}
	return summary, nil
}

// List 返回按 id 排序的一页存储类型（zoneID 非 nil 时仅返回该 zone）；
// total 是存储类型总数，与分页无关。
func (s *StorageTypeService) List(ctx context.Context, zoneID *int64, limit, offset int) ([]model.StorageType, int, error) {
	types, err := s.repo.ListPage(ctx, zoneID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list storage types: %w", err)
	}
	total, err := s.repo.Count(ctx, zoneID)
	if err != nil {
		return nil, 0, fmt.Errorf("list storage types: count: %w", err)
	}
	return types, total, nil
}

// Get 返回指定 id 的存储类型，或返回 not_found 错误。
func (s *StorageTypeService) Get(ctx context.Context, id int64) (*model.StorageType, error) {
	st, err := s.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("storage type %d not found", id)
		}
		return nil, fmt.Errorf("get storage type: %w", err)
	}
	return st, nil
}

// Update 更新已有存储类型的管理员元数据（设计 D3/D4）：仅允许 name（业务
// 名，可空/可置空）与 enabled（启用开关）；pve_storage 是扫描权威字段，
// 不可修改。name 为 nil 表示不更新，非 nil 时 trim 后应用（空串表示置空为
// NULL，trim 后长度超过 maxStorageTypeNameLen 拒绝）；enabled 为 nil 表示
// 不更新。
func (s *StorageTypeService) Update(ctx context.Context, id int64, name *string, enabled *bool) (*model.StorageType, error) {
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if utf8.RuneCountInString(trimmed) > maxStorageTypeNameLen {
			return nil, badRequestf("storage type name must be at most %d characters", maxStorageTypeNameLen)
		}
		name = &trimmed
	}
	st, err := s.repo.UpdateMeta(ctx, id, name, enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("storage type %d not found", id)
		}
		return nil, fmt.Errorf("update storage type: %w", err)
	}
	return st, nil
}

// Delete 删除一个存储类型。当仍有 VM 引用它时，删除会被以冲突错误拒绝，
// 从而保证现有 VM 的映射保持不变。
func (s *StorageTypeService) Delete(ctx context.Context, id int64) error {
	err := s.repo.Delete(ctx, id)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return notFoundf("storage type %d not found", id)
	case errors.Is(err, repository.ErrInUse):
		return conflictf("storage type %d is still referenced by VMs", id)
	default:
		return fmt.Errorf("delete storage type: %w", err)
	}
}
