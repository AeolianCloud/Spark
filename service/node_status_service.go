package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
)

// NodeStatusRepository 是 NodeStatusService 依赖的节点数据访问层
// （GetNode 返回节点凭据与 PVE 节点名）。
type NodeStatusRepository interface {
	GetNode(ctx context.Context, id int64) (*model.PVENode, error)
}

// NodeStatusService 实现节点实时状态聚合（openspec node-monitor 设计 D3）：
// 先以本地 pve_nodes 记录解析节点凭据，再并发拉取 PVE 的 status/network
//（核心组）并串行拉取 rrddata（增强字段）聚合成 NodeStatusResult。状态为
// PVE 实时透传，不落库。
type NodeStatusService struct {
	nodeRepo NodeStatusRepository
	// newClient 为节点构建 PVE 客户端（host/port/API 用户/token）；可注入，
	// 以便测试将拉取指向假服务器（与 ImageService 同模式）。
	newClient func(host string, port int, apiUser, apiTokenSecret string) *pve.Client
}

// NewNodeStatusService 使用节点仓库创建一个 NodeStatusService。默认客户端
// 工厂针对 https://{host}:{port}/api2/json（port 取节点持久化的端口）构建
// 客户端；测试可通过 SetClientFactory 覆盖。
func NewNodeStatusService(nodeRepo NodeStatusRepository) *NodeStatusService {
	return &NodeStatusService{
		nodeRepo: nodeRepo,
		newClient: func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient(host, apiUser, apiTokenSecret, pve.WithPort(port))
		},
	}
}

// SetClientFactory 替换用于节点状态拉取的 PVE 客户端工厂（测试注入假服务器
// 用，与 ImageService.SetClientFactory 同模式）。
func (s *NodeStatusService) SetClientFactory(fn func(host string, port int, apiUser, apiTokenSecret string) *pve.Client) {
	if fn != nil {
		s.newClient = fn
	}
}

// NodeStatusResult 是 GetStatus 的聚合结果。
type NodeStatusResult struct {
	// Node 是本地 pve_nodes 记录（配置字段，handler 平铺到响应顶层）。
	Node *model.PVENode
	// Status 是 PVE 的 GET /nodes/{node}/status 实时状态；PVE 9 不返回
	// status 字段时由服务层补 "online"（请求成功即在线）。
	Status *pve.NodeStatusData
	// Network 是 PVE 的 GET /nodes/{node}/network 结构化接口列表。
	Network []pve.NetIface
	// NetIO 是节点级网络吞吐（bytes/s，来自 GET /nodes/{node}/rrddata
	// 的最后一个采样点，PVE 9 的 netstat 只返回 VM 设备计数器不可用）。
	NetIO *pve.NodeIO
}

// GetStatus 返回指定节点（本地 pve_nodes.id）的实时状态。节点不存在时
// 返回 not_found；核心组（status/network）任一失败整体降级为
// KindNodeUnavailable，增强字段 rrddata（NetIO）失败仅降级为零值、不
// 整体降级（设计决策，见 openspec node-monitor 设计 D3：rrddata 需要
// Sys.Audit 权限，权限不足或临时失败不应拖垮整个状态查询）。错误消息经
// sanitizePVEError 脱敏并按 maxNodeStatusErrorLen 截断，不泄露 PVE
// 内部细节（设计 D1/D3）。
//
// 并发结构：核心组（status+network）并发执行，任一失败即通过
// context.WithCancel 取消另一 in-flight 请求并整体返回 503，不再等待
// 最慢请求超时（最长 30s）；rrddata 在核心组成功后单独串行调用（增强
// 字段，失败仅降级 NetIO，不影响响应速度大局）。缓冲通道容量 = goroutine
// 最大发送数，保证任何时序下 goroutine 都不会阻塞在发送上，任一失败
// 路径无 goroutine 泄漏。
func (s *NodeStatusService) GetStatus(ctx context.Context, nodeID int64) (*NodeStatusResult, error) {
	node, err := s.nodeRepo.GetNode(ctx, nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("node %d not found", nodeID)
		}
		return nil, fmt.Errorf("get node %d: %w", nodeID, err)
	}
	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	pveNode := nodeName(*node)

	type coreResult struct {
		status  *pve.NodeStatusData
		network []pve.NetIface
		err     error
	}
	// 核心组共享可取消的 context：任一失败时 defer cancel 立即终止另一
	// in-flight 请求（m4：503 响应不等最慢请求）。
	coreCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// 通道缓冲容量 = 2 个 goroutine 的最大发送数：即使提前返回未消费，
	// 其余 goroutine 的发送也不会阻塞。
	coreCh := make(chan coreResult, 2)
	go func() {
		st, err := client.NodeStatus(coreCtx, pveNode)
		coreCh <- coreResult{status: st, err: err}
	}()
	go func() {
		nw, err := client.NodeNetwork(coreCtx, pveNode)
		coreCh <- coreResult{network: nw, err: err}
	}()

	res := &NodeStatusResult{Node: node}
	for i := 0; i < 2; i++ {
		r := <-coreCh
		if r.err != nil {
			// 核心组任一失败整体降级：defer cancel 立即中止另一
			// in-flight 请求，503 无需等待其完成或超时。
			// 消息先脱敏（sanitizePVEError）再按 rune 截断：PVE 错误体
			// 最大可达 1MiB（pve 包 maxResponseSize），不截断会原样进入
			// 503 响应体（安全红线：对外错误消息不得携带内部细节）。
			return nil, nodeUnavailablef("node %q unavailable: %s", pveNode,
				truncateNodeStatusError(sanitizePVEError(r.err)))
		}
		if r.status != nil {
			// PVE 9 的 status 端点不再返回 status 字段（请求成功即在线），
			// 此处补默认值，保证 handler 层恒有非空状态。
			if r.status.Status == "" {
				r.status.Status = "online"
			}
			res.Status = r.status
		}
		if r.network != nil {
			res.Network = r.network
		}
	}

	// 增强字段：rrddata 单独串行调用（需 Sys.Audit 权限），失败仅把
	// NetIO 降级为零值（非 nil，契约要求 net_io 恒为非空对象），照常
	// 返回 status/network（设计决策 D3：权限不足或临时失败不拖垮整个
	// 状态查询）。
	io, err := client.NodeNetIO(ctx, pveNode)
	if err != nil {
		res.NetIO = &pve.NodeIO{}
	} else {
		res.NetIO = io
	}
	return res, nil
}

// maxNodeStatusErrorLen 限制节点状态降级错误消息的最大字符数（rune）。
// PVE 错误体最大可达 1MiB（pve 包 maxResponseSize），脱敏后若不截断，
// 超长错误体可能原样进入 503 响应体（违反"对外错误消息不得暴露内部
// 细节"红线，且放大响应体）；500 字符足以承载脱敏摘要与节点名。
const maxNodeStatusErrorLen = 500

// truncateNodeStatusError 按 maxNodeStatusErrorLen 以 rune 边界截断错误
// 消息（先截断后构造 nodeUnavailablef，与 vm_service.sanitizeOperationError
// 的"先脱敏后截断、按 rune 切割"风格一致）：多字节 UTF-8 字符绝不会被切
// 成非法序列。消息短于上限时原样返回。
func truncateNodeStatusError(msg string) string {
	r := []rune(msg)
	if len(r) > maxNodeStatusErrorLen {
		return string(r[:maxNodeStatusErrorLen])
	}
	return msg
}
