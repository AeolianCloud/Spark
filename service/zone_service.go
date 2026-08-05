package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
)

// ZoneRepository 是 ZoneService 依赖的区域数据访问层。
type ZoneRepository interface {
	CreateZone(ctx context.Context, name string) (*model.Zone, error)
	GetZone(ctx context.Context, id int64) (*model.Zone, error)
	ListZones(ctx context.Context) ([]model.Zone, error)
	ListZonesPage(ctx context.Context, limit, offset int) ([]model.Zone, error)
	CountZones(ctx context.Context) (int, error)
}

// NodeRepository 是 ZoneService 依赖的节点数据访问层。
type NodeRepository interface {
	CreateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error)
	GetNode(ctx context.Context, id int64) (*model.PVENode, error)
	ListNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error)
	ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error)
	UpdateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error)
}

// ZoneService 实现区域及其 PVE 节点的业务规则。
type ZoneService struct {
	zoneRepo ZoneRepository
	nodeRepo NodeRepository
}

// NewZoneService 使用给定的仓库创建一个 ZoneService。
func NewZoneService(zoneRepo ZoneRepository, nodeRepo NodeRepository) *ZoneService {
	return &ZoneService{zoneRepo: zoneRepo, nodeRepo: nodeRepo}
}

// KindNodeUnavailable 表示"没有可达的候选节点"。该值位于 errors.go 中共享
// kind 的 iota 范围之外（该范围归其他批次所有），以避免本文件与它们的改动
// 产生耦合。
const KindNodeUnavailable ErrorKind = 100

// nodeUnavailablef 构造一个 KindNodeUnavailable 服务错误。
func nodeUnavailablef(format string, args ...any) *Error {
	return &Error{Kind: KindNodeUnavailable, Message: fmt.Sprintf(format, args...)}
}

// nodeProbeTimeout 限制对单个候选节点的一次可达性探测时长。
const nodeProbeTimeout = 5 * time.Second

// nodeProbeBudget 限制对所有候选节点的整轮可达性扫描时长，避免 N 次探测
// 累加为 N x nodeProbeTimeout。
const nodeProbeBudget = 15 * time.Second

// CreateZone 创建一个区域。名称必填且必须唯一；该检查是尽力而为的扫描
// （zones 表没有唯一约束），两个并发创建仍可能同时通过，v1 接受这一情况。
func (s *ZoneService) CreateZone(ctx context.Context, name string) (*model.Zone, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, badRequestf("zone name is required")
	}
	zones, err := s.zoneRepo.ListZones(ctx)
	if err != nil {
		return nil, fmt.Errorf("create zone: list zones: %w", err)
	}
	for _, z := range zones {
		if z.Name == name {
			return nil, conflictf("zone %q already exists", name)
		}
	}
	zone, err := s.zoneRepo.CreateZone(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("create zone: %w", err)
	}
	return zone, nil
}

// ZoneWithNodes 将区域与其节点配对，用于列表响应。
type ZoneWithNodes struct {
	Zone  model.Zone
	Nodes []model.PVENode
}

// ListZones 返回一页区域及其节点，满足 zones 规范中"返回所有区域及其节点
// 信息"的要求。分页单位为区域：每页容纳 limit 个区域（offset 按 id 顺序跳过
// 前 offset 个区域），每个区域都带完整的、不分页的节点列表——一个区域包含的
// 节点很少，因此仅对区域行分页。total 是区域总数，与分页无关。
func (s *ZoneService) ListZones(ctx context.Context, limit, offset int) ([]ZoneWithNodes, int, error) {
	zones, err := s.zoneRepo.ListZonesPage(ctx, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list zones: %w", err)
	}
	out := make([]ZoneWithNodes, 0, len(zones))
	for _, z := range zones {
		nodes, err := s.nodeRepo.ListNodesByZone(ctx, z.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("list zones: nodes of zone %d: %w", z.ID, err)
		}
		out = append(out, ZoneWithNodes{Zone: z, Nodes: nodes})
	}
	total, err := s.zoneRepo.CountZones(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list zones: count: %w", err)
	}
	return out, total, nil
}

// CreateNode 在区域中注册一个节点。区域必须存在，名称在区域内必须唯一，
// 且 host/api_user/api_token 均为必填。按设计，API token 以明文存储。
func (s *ZoneService) CreateNode(ctx context.Context, zoneID int64, name, host, apiUser, apiToken string, enabled *bool) (*model.PVENode, error) {
	name = strings.TrimSpace(name)
	host = strings.TrimSpace(host)
	apiUser = strings.TrimSpace(apiUser)
	apiToken = strings.TrimSpace(apiToken)
	switch {
	case name == "":
		return nil, badRequestf("node name is required")
	case host == "":
		return nil, badRequestf("node host is required")
	case apiUser == "":
		return nil, badRequestf("node api_user is required")
	case apiToken == "":
		return nil, badRequestf("node api_token is required")
	}

	if _, err := s.zoneRepo.GetZone(ctx, zoneID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("zone %d not found", zoneID)
		}
		return nil, fmt.Errorf("create node: check zone: %w", err)
	}
	nodes, err := s.nodeRepo.ListNodesByZone(ctx, zoneID)
	if err != nil {
		return nil, fmt.Errorf("create node: list nodes: %w", err)
	}
	for _, n := range nodes {
		if n.Name == name {
			return nil, conflictf("node %q already exists in zone %d", name, zoneID)
		}
	}

	node := model.PVENode{
		ZoneID: zoneID, Name: name, Host: host, APIUser: apiUser,
		APITokenSecret: apiToken, Enabled: true,
	}
	if enabled != nil {
		node.Enabled = *enabled
	}
	created, err := s.nodeRepo.CreateNode(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("create node: %w", err)
	}
	return created, nil
}

// UpdateNode 替换节点的可编辑字段（name/host/api_user/enabled）。
// api_token 可选：空值表示保留已存储的密钥。第二个返回值报告 token 是否被
// 替换，以便处理器在不回显密钥的情况下应答 api_token_set。
func (s *ZoneService) UpdateNode(ctx context.Context, id int64, name, host, apiUser, apiToken string, enabled *bool) (*model.PVENode, bool, error) {
	name = strings.TrimSpace(name)
	host = strings.TrimSpace(host)
	apiUser = strings.TrimSpace(apiUser)
	apiToken = strings.TrimSpace(apiToken)
	switch {
	case name == "":
		return nil, false, badRequestf("node name is required")
	case host == "":
		return nil, false, badRequestf("node host is required")
	case apiUser == "":
		return nil, false, badRequestf("node api_user is required")
	}

	existing, err := s.nodeRepo.GetNode(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, notFoundf("node %d not found", id)
		}
		return nil, false, fmt.Errorf("update node: get: %w", err)
	}
	if name != existing.Name {
		nodes, err := s.nodeRepo.ListNodesByZone(ctx, existing.ZoneID)
		if err != nil {
			return nil, false, fmt.Errorf("update node: list nodes: %w", err)
		}
		for _, n := range nodes {
			if n.Name == name && n.ID != id {
				return nil, false, conflictf("node %q already exists in zone %d", name, existing.ZoneID)
			}
		}
	}

	node := *existing
	node.Name, node.Host, node.APIUser = name, host, apiUser
	tokenChanged := apiToken != ""
	if tokenChanged {
		node.APITokenSecret = apiToken
	}
	if enabled != nil {
		node.Enabled = *enabled
	}
	updated, err := s.nodeRepo.UpdateNode(ctx, node)
	if err != nil {
		return nil, false, fmt.Errorf("update node: %w", err)
	}
	return updated, tokenChanged, nil
}

// ListNodesByZone 返回区域的节点列表；区域必须存在。
func (s *ZoneService) ListNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	if _, err := s.zoneRepo.GetZone(ctx, zoneID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("zone %d not found", zoneID)
		}
		return nil, fmt.Errorf("list nodes: check zone: %w", err)
	}
	nodes, err := s.nodeRepo.ListNodesByZone(ctx, zoneID)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	return nodes, nil
}

// SelectReachableNode 按顺序探测候选节点，返回第一个 PVE API 可达的节点
// （每次探测受 nodeProbeTimeout 限制；整轮扫描受 nodeProbeBudget 限制，避免
// 多个死节点累积成长时间阻塞）。失败的节点（不可达、TLS、超时或 token 无效）
// 会被跳过，失败以 debug 级别记录，携带 PVE 客户端报告的原始错误——该错误
// 从不包含凭据（参见 pve.NewClient 的脱敏处理）。当没有任何节点可达时，返回
// KindNodeUnavailable 错误。VM 创建（任务 7.1）使用该函数选择部署节点。
func SelectReachableNode(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
	return selectReachableNode(ctx, nodes, func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient(host, apiUser, apiTokenSecret, pve.WithTimeout(nodeProbeTimeout))
	})
}

// selectReachableNode 是注入了客户端工厂的 SelectReachableNode，以便测试
// 将探测指向假服务器。
func selectReachableNode(ctx context.Context, nodes []model.PVENode, newClient func(host, apiUser, apiTokenSecret string) *pve.Client) (model.PVENode, error) {
	probeCtx, cancel := context.WithTimeout(ctx, nodeProbeBudget)
	defer cancel()

	for _, n := range nodes {
		client := newClient(n.Host, n.APIUser, n.APITokenSecret)
		if err := client.Ping(probeCtx); err != nil {
			slog.Debug("node unreachable, skipping candidate",
				"node", n.Name,
				"host", n.Host,
				"reason", err,
			)
			continue
		}
		return n, nil
	}
	return model.PVENode{}, nodeUnavailablef("no reachable node among %d candidate(s)", len(nodes))
}
