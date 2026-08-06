package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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
	// probeNodes 探测 PVE 集群节点名列表（GET /nodes 的 node 字段）；可注入
	// 用于测试，生产默认使用 probeClusterNodeNames（与 selectReachableNode
	// 相同的客户端构建模式，探测受 nodeProbeTimeout 限制）。
	probeNodes func(ctx context.Context, host string, port int, apiUser, apiTokenSecret string) ([]string, error)
}

// NewZoneService 使用给定的仓库创建一个 ZoneService。
func NewZoneService(zoneRepo ZoneRepository, nodeRepo NodeRepository) *ZoneService {
	s := &ZoneService{zoneRepo: zoneRepo, nodeRepo: nodeRepo}
	// 探测函数在构造之后才赋值（与 VMService 的 selectNode 模式一致），测试
	// 可整体替换该字段以将探测指向假服务器。
	s.probeNodes = probeClusterNodeNames
	return s
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

// defaultNodePort 是 PVE API 的默认端口；节点 host 未携带 :port 后缀时使用。
// 与 pve 包的 defaultPort 保持一致，改动需同步两侧。
const defaultNodePort = 8006

// parseNodeHost 解析节点 host 字符串，返回去除 scheme 后的纯主机地址与 API
// 端口。规则（与 pve.WithPort 的端口范围一致，但校验更严格）：
//   - 先去除首尾空白与 http:// / https:// 前缀（大小写不敏感）；
//   - 尾斜杠（如 host:8007/）会被拒绝——pve 客户端对尾斜杠宽容，这里不做
//     自动修正，避免用户以为输入被接受；输入只允许 主机[:端口] 形式；
//   - 以 / 开头（如 /host:8007，或 https:///host:8007 剥掉 scheme 后仍以
//     / 开头）是裸路径而非主机地址，同样返回 badRequest；
//   - 含多个冒号（如 IPv6 字面量）无法可靠剥离端口，返回 badRequest；
//   - 无冒号时端口取默认值 defaultNodePort；
//   - 有冒号时，后缀必须是纯数字且位于 1-65535 之间——:0 与非法后缀
//     （如 :abc、:99999）同样返回 badRequest（pve.WithPort 把 0 视为
//     "忽略"，这里则显式拒绝）；host 只保留冒号前的部分；
//   - 解析后主机地址为空（如输入仅为 scheme 或 :port）返回 badRequest。
func parseNodeHost(host string) (string, int, error) {
	host = strings.TrimSpace(host)
	// scheme 前缀大小写不敏感（如 HTTPS://）；用 EqualFold 只比较前缀，
	// 避免 ToLower 影响 host 其余部分（主机名大小写不敏感，无需转换）。
	if len(host) >= 8 && strings.EqualFold(host[:8], "https://") {
		host = host[8:]
	} else if len(host) >= 7 && strings.EqualFold(host[:7], "http://") {
		host = host[7:]
	}
	if strings.Count(host, ":") > 1 {
		return "", 0, badRequestf("node host %q contains multiple colons (IPv6 literals are not supported)", host)
	}
	if !strings.Contains(host, ":") {
		if host == "" {
			return "", 0, badRequestf("node host is required")
		}
		// 尾斜杠（如 host/）与裸路径（如 /host）同样拒绝——只允许纯 主机 形式，
		// 与带端口分支的行为保持一致，也避免用户以为 host/ 被接受。
		if strings.HasPrefix(host, "/") || strings.HasSuffix(host, "/") {
			return "", 0, badRequestf("invalid node host %q (must be host[:port])", host)
		}
		return host, defaultNodePort, nil
	}
	pureHost, portStr, _ := strings.Cut(host, ":")
	// ParseUint（10 进制、16 bit）精确表达"纯数字"语义：拒绝符号前缀与空串，
	// 溢出（> 65535）由 ErrRange 覆盖；随后单独拒绝 0（端口必须 >= 1）。
	port64, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port64 < 1 {
		return "", 0, badRequestf("invalid port %q in node host %q (must be 1-65535)", portStr, host)
	}
	if pureHost == "" {
		return "", 0, badRequestf("node host is required")
	}
	// 以 / 开头（如 "/host:8007"、剥掉 scheme 后仍是 "/host:8007" 的
	// https:///host:8007）是裸路径而非主机地址，直接拒绝，避免路径被当作
	// 主机落库；与注释承诺的 主机[:端口] 输入形式保持一致。
	if strings.HasPrefix(pureHost, "/") {
		return "", 0, badRequestf("invalid node host %q (must be host[:port])", host)
	}
	return pureHost, int(port64), nil
}

// probeClusterNodeNames 调用 PVE 的 GET /nodes 探测集群节点名列表，是
// ZoneService.probeNodes 的生产默认实现。它复用 SelectReachableNode 的客户端
// 构建模式（含 nodeProbeTimeout 探测超时）；返回的错误来自 pve 客户端的脱敏
// 处理，绝不包含凭据。
func probeClusterNodeNames(ctx context.Context, host string, port int, apiUser, apiTokenSecret string) ([]string, error) {
	client := pve.NewClient(host, apiUser, apiTokenSecret,
		pve.WithPort(port), pve.WithTimeout(nodeProbeTimeout))
	nodes, err := client.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Node)
	}
	return names, nil
}

// matchPveNodeName 探测 PVE 集群节点名并校验业务名与其一致（任务 4.1/4.2
// 共享）：探测结果中存在与 name 相同的集群节点名时返回该名（PveName = 业务
// 名）；无匹配时返回 nodeUnavailable 错误并列出探测到的集群真实节点名（提示
// 用户改用真实名，不含凭据）；探测本身失败（不可达/TLS/401 等）同样以
// nodeUnavailable 语义拒绝登记/更新，绝不静默落库。探测受 nodeProbeTimeout
// 限制。
func (s *ZoneService) matchPveNodeName(ctx context.Context, name, host string, port int, apiUser, apiToken string) (string, error) {
	clusterNames, err := s.probeNodes(ctx, host, port, apiUser, apiToken)
	if err != nil {
		return "", nodeUnavailablef("probe pve cluster node names of node %q failed: %v", name, err)
	}
	for _, cn := range clusterNames {
		if cn == name {
			return name, nil
		}
	}
	return "", nodeUnavailablef("node name %q does not match any pve cluster node name (cluster node(s): %s)",
		name, strings.Join(clusterNames, ", "))
}

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

	// 解析 host 中可选的 :port 后缀：host 只保留纯主机地址，端口写入
	// node.Port（未携带端口时取默认 8006）。解析失败（非法端口、IPv6 字面量
	// 等）以 badRequest 拒绝，不落库。
	pureHost, port, err := parseNodeHost(host)
	if err != nil {
		return nil, err
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

	// 探测 PVE 集群节点名并校验业务名与集群真实名一致（任务 4.1）：探测或
	// 匹配失败都以 nodeUnavailable 语义拒绝登记、不落库；错误消息提示集群
	// 真实节点名且不含凭据。
	pveName, err := s.matchPveNodeName(ctx, name, pureHost, port, apiUser, apiToken)
	if err != nil {
		return nil, err
	}

	node := model.PVENode{
		ZoneID: zoneID, Name: name, PveName: pveName, Host: pureHost, Port: port, APIUser: apiUser,
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

// UpdateNode 替换节点的可编辑字段（name/pve_name/host/api_user/enabled）。
// api_token 可选：空值表示保留已存储的密钥。第二个返回值报告 token 是否被
// 替换，以便处理器在不回显密钥的情况下应答 api_token_set。
//
// pve_name 的更新规则（任务 4.2）：请求显式指定 pve_name（非空）时直接采用，
// 跳过探测匹配（逃生通道）；否则当 name 或 host 变化时，走与 CreateNode
// 相同的探测匹配；name/host 均未变化时保留原 PveName。
func (s *ZoneService) UpdateNode(ctx context.Context, id int64, name, pveName, host, apiUser, apiToken string, enabled *bool) (*model.PVENode, bool, error) {
	name = strings.TrimSpace(name)
	pveName = strings.TrimSpace(pveName)
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

	// 与 CreateNode 相同的 host 解析规则：host 保存纯地址，端口写入
	// node.Port；解析失败以 badRequest 拒绝，不落库。
	pureHost, port, err := parseNodeHost(host)
	if err != nil {
		return nil, false, err
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
	node.Name, node.Host, node.Port, node.APIUser = name, pureHost, port, apiUser
	switch {
	case pveName != "":
		// 请求显式指定 pve_name：直接采用，跳过探测匹配（逃生通道，见设计 D2）。
		node.PveName = pveName
	case name != existing.Name || pureHost != existing.Host || port != existing.Port:
		// name/host 变化：与 CreateNode 相同的探测匹配（无匹配时错误消息会
		// 列出集群真实节点名）。探测使用更新后的凭据；api_token 为空时表示
		// 保留已存储的密钥，因此用原 token 探测。
		probeToken := apiToken
		if probeToken == "" {
			probeToken = existing.APITokenSecret
		}
		matched, err := s.matchPveNodeName(ctx, name, pureHost, port, apiUser, probeToken)
		if err != nil {
			return nil, false, err
		}
		node.PveName = matched
	}
	// 否则（name/host 未变化且未显式指定）保留原 PveName。
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
	return selectReachableNode(ctx, nodes, func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient(host, apiUser, apiTokenSecret,
			pve.WithPort(port), pve.WithTimeout(nodeProbeTimeout))
	})
}

// selectReachableNode 是注入了客户端工厂的 SelectReachableNode，以便测试
// 将探测指向假服务器。
func selectReachableNode(ctx context.Context, nodes []model.PVENode, newClient func(host string, port int, apiUser, apiTokenSecret string) *pve.Client) (model.PVENode, error) {
	probeCtx, cancel := context.WithTimeout(ctx, nodeProbeBudget)
	defer cancel()

	for _, n := range nodes {
		// 探测必须连接节点持久化的 API 端口（任务 4.4）。
		client := newClient(n.Host, n.Port, n.APIUser, n.APITokenSecret)
		if err := client.Ping(probeCtx); err != nil {
			slog.Debug("node unreachable, skipping candidate",
				"node", n.Name,
				"pve_node", nodeName(n),
				"host", n.Host,
				"reason", err,
			)
			continue
		}
		return n, nil
	}
	return model.PVENode{}, nodeUnavailablef("no reachable node among %d candidate(s)", len(nodes))
}
