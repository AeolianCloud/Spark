package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"spark/pve"
	"spark/service"
)

// NodeStatusHandler 提供 GET /nodes/:id/status 路由（openspec node-monitor
// 设计 D1/D4）：本地节点配置（平铺）+ PVE 实时状态（嵌套 status 对象）。
type NodeStatusHandler struct {
	svc *service.NodeStatusService
}

// NewNodeStatusHandler 创建一个由 svc 支撑的 NodeStatusHandler。
func NewNodeStatusHandler(svc *service.NodeStatusService) *NodeStatusHandler {
	return &NodeStatusHandler{svc: svc}
}

// RegisterNodeStatusRoutes 挂载节点状态路由。nodesGroup 是 /nodes 分组
// （GET /nodes/:id/status），与 RegisterZonesRoutes 的 PUT /nodes/:id
// 同分组前缀共存；由 router 传入独立分组实例调用。
func RegisterNodeStatusRoutes(nodesGroup *gin.RouterGroup, svc *service.NodeStatusService) {
	h := NewNodeStatusHandler(svc)
	nodesGroup.GET("/:id/status", Handler(h.GetNodeStatus))
}

// nodeStatusResponse 是 GET /nodes/:id/status 的响应负载：nodeResponse 的
// 全部配置字段平铺 + 嵌套 status 对象（运行时数据），前端详情页可复用列表
// 页的配置字段类型定义。
type nodeStatusResponse struct {
	ID          int64           `json:"id"`
	ZoneID      int64           `json:"zone_id"`
	Name        string          `json:"name"`
	PveName     string          `json:"pve_name"`
	Host        string          `json:"host"`
	Port        int             `json:"port"`
	APIUser     string          `json:"api_user"`
	APITokenSet bool            `json:"api_token_set"`
	Enabled     bool            `json:"enabled"`
	CreatedAt   time.Time       `json:"created_at"`
	Status      nodeStatusField `json:"status"`
}

// nodeStatusField 是嵌套的 PVE 实时状态对象。
type nodeStatusField struct {
	// Status 是在线状态（如 "online"；PVE 9 无该字段时由 service 补默认值）。
	Status string `json:"status"`
	// UptimeSeconds 是在线时长（秒）。
	UptimeSeconds int64 `json:"uptime_seconds"`
	// PveVersion 是 PVE 版本号。
	PveVersion string `json:"pve_version"`
	// KernelVersion 是内核版本。
	KernelVersion string `json:"kernel_version"`
	// CPU 是 CPU 用量信息（核数/使用率/负载）。
	CPU nodeCPUStatus `json:"cpu"`
	// Memory 是内存用量信息（总量/已用/使用率 0-1 小数）。
	Memory nodeMemoryStatus `json:"memory"`
	// Disk 是根分区用量信息（总量/已用/使用率）。
	Disk nodeDiskStatus `json:"disk"`
	// Network 是网络接口列表（仅接口信息，PVE 9 无接口级流量）。
	Network []nodeNetStatus `json:"network"`
	// NetIO 是节点级网络吞吐（bytes/s，来自 rrddata 最后一点）。
	NetIO *pve.NodeIO `json:"net_io"`
}

// nodeCPUStatus 是 CPU 用量字段。
type nodeCPUStatus struct {
	// Cores 是 CPU 核数（PVE 的 cpus，缺失时回退 maxcpu）。
	Cores int `json:"cores"`
	// Usage 是 CPU 使用率（0-1 小数，PVE 的 cpu 字段直接透传）。
	Usage float64 `json:"usage"`
	// Loadavg 是负载均值数组（PVE 原文透传）。
	Loadavg []string `json:"loadavg"`
}

// nodeMemoryStatus 是内存用量字段。
type nodeMemoryStatus struct {
	// Total 是内存总量字节数。
	Total int64 `json:"total"`
	// Used 是已用内存字节数。
	Used int64 `json:"used"`
	// Usage 是内存使用率（0-1 小数；total 为 0 时取 0）。
	Usage float64 `json:"usage"`
}

// nodeDiskStatus 是根分区用量字段。
type nodeDiskStatus struct {
	// Total 是根分区总容量字节数。
	Total int64 `json:"total"`
	// Used 是根分区已用字节数。
	Used int64 `json:"used"`
	// Usage 是根分区使用率（0-1 小数；total 为 0 时取 0）。
	Usage float64 `json:"usage"`
}

// nodeNetStatus 是单个网络接口的展示字段：来自 /network 的结构化接口信息。
// PVE 9 的 netstat 只返回 VM 设备计数器，接口级流量无来源，因此不再提供
// rx/tx 字段（节点级吞吐见 nodeStatusField.NetIO）。
type nodeNetStatus struct {
	// Iface 是接口名。
	Iface string `json:"iface"`
	// Type 是接口类型（如 bridge、eth）。
	Type string `json:"type"`
	// Address 是接口 IP 地址。
	Address string `json:"address"`
	// Active 是否启用；PVE 的 active 是布尔或数字 1/0（PveBool 双格式
	// 兼容，JSON 输出为 boolean），PVE 未返回时为 null。
	Active *pve.PveBool `json:"active"`
}

// usageRatio 计算 used/total 的 0-1 使用率；total <= 0 时返回 0（防御
// 除零，PVE 未返回总量时前端展示 0 而非非法值）。
func usageRatio(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total)
}

// GetNodeStatus 处理 GET /nodes/:id/status：解析 id → 服务聚合 → 拼装
// nodeStatusResponse。错误走 mapServiceErrorExtended（节点不存在 → 404
// not_found；PVE 不可达 → 503 node_unavailable，消息已脱敏）。
func (h *NodeStatusHandler) GetNodeStatus(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	result, err := h.svc.GetStatus(c.Request.Context(), id)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	c.JSON(http.StatusOK, toNodeStatusResponse(result))
	return nil
}

// toNodeStatusResponse 把服务聚合结果转换为响应负载：配置字段平铺
// （api_token_set 报告是否存有 token，与 nodeResponse 语义一致），
// status 对象由 PVE 实时数据组装。各数值字段按 PVE 7/8/9 双格式回退：
// PVE 9 优先取对象字段（cpuinfo/memory/pveversion），PVE 7/8 回退旧字段。
func toNodeStatusResponse(res *service.NodeStatusResult) nodeStatusResponse {
	out := nodeStatusResponse{
		ID: res.Node.ID, ZoneID: res.Node.ZoneID, Name: res.Node.Name,
		PveName: res.Node.PveName, Host: res.Node.Host, Port: res.Node.Port,
		APIUser: res.Node.APIUser, APITokenSet: res.Node.APITokenSecret != "",
		Enabled: res.Node.Enabled, CreatedAt: res.Node.CreatedAt,
	}
	if res.Status == nil {
		// 死代码防御：service 的 GetStatus 在成功路径恒返回非空 Status
		//（聚合失败整体报错，不产出 nil），仅在 service 契约被破坏
		//（如未来重构引入 nil 成功返回）时可达；保持零值响应不报错，
		// 防御性保留。NetIO 同样补零值对象，保证契约 net_io 恒为非空
		// 对象（避免 JSON null，m2）。
		out.Status.Network = []nodeNetStatus{}
		out.Status.NetIO = &pve.NodeIO{}
		return out
	}
	st := res.Status
	// Cores 回退链：PVE 9 的 cpuinfo.cpus → PVE 7/8 的 cpus → maxcpu。
	cores := 0
	if st.CPUInfo != nil {
		cores = st.CPUInfo.Cpus
	}
	if cores == 0 {
		cores = st.CPUs
	}
	if cores == 0 {
		cores = st.MaxCPU
	}
	loadavg := st.Loadavg
	if loadavg == nil {
		loadavg = []string{}
	}
	// Disk：Total 优先取 PVE 7 的 maxrootfs（总量字段），为 0（PVE 8/9
	// 缺失该字段）时回退 rootfs.total；Used 取 rootfs.used（PVE 8/9 对象）
	// 或裸数字（PVE 7），双格式兼容。
	rootfsTotal := st.MaxRootfs
	if rootfsTotal == 0 {
		rootfsTotal = st.Rootfs.Total
	}
	// Memory：Total/Used 优先取 PVE 9 的 memory 对象，为 nil（PVE 7/8）
	// 时回退 maxmem/mem。
	memTotal, memUsed := st.MaxMem, st.Mem
	if st.Memory != nil {
		memTotal, memUsed = st.Memory.Total, st.Memory.Used
	}
	// PveVersion 回退链：PVE 9 的 pveversion → PVE 7/8 的 version。
	pveVersion := st.PveVersion
	if pveVersion == "" {
		pveVersion = st.Version
	}
	out.Status = nodeStatusField{
		// Status 由 service 保证非空（PVE 9 无该字段时补 "online"）。
		Status: st.Status, UptimeSeconds: st.Uptime,
		PveVersion: pveVersion, KernelVersion: st.KVersion,
		CPU: nodeCPUStatus{Cores: cores, Usage: st.CPU, Loadavg: loadavg},
		Memory: nodeMemoryStatus{
			Total: memTotal, Used: memUsed, Usage: usageRatio(memUsed, memTotal),
		},
		Disk: nodeDiskStatus{
			Total: rootfsTotal, Used: st.Rootfs.Used, Usage: usageRatio(st.Rootfs.Used, rootfsTotal),
		},
		NetIO: res.NetIO,
	}
	network := make([]nodeNetStatus, 0, len(res.Network))
	for _, iface := range res.Network {
		network = append(network, nodeNetStatus{
			Iface: iface.Iface, Type: iface.Type, Address: iface.Address, Active: iface.Active,
		})
	}
	out.Status.Network = network
	return out
}
