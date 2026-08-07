package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// NodeStatusData 是 GET /nodes/{node}/status 的响应负载：节点在线状态、
// CPU/内存/根分区的实时用量、在线时长、版本与负载。字段按 PVE 版本双格式
// 兼容：PVE 7/8 返回 mem/maxmem/cpus/maxcpu/version/status/node，
// PVE 9 改为 cpuinfo/memory/pveversion 对象并移除旧字段。PVE 未返回的
// 数值字段一律取零值，绝不因字段缺失而整体报错（容错设计，见 openspec
// 设计 D2）。
type NodeStatusData struct {
	// Node 是 PVE 节点名（PVE 7/8 有；PVE 9 已移除该字段，回退为空串）。
	Node string `json:"node"`
	// Status 是在线状态（如 "online"；PVE 7/8 有，PVE 9 已移除，由
	// service 层在请求成功时补 "online"）。
	Status string `json:"status"`
	// CPU 是 0-1 之间的 CPU 使用率小数（PVE 7/8/9 都有）。
	CPU float64 `json:"cpu"`
	// CPUInfo 是 CPU 信息对象（PVE 9 新增；PVE 7/8 缺失时为 nil，核数
	// 回退 CPUs/MaxCPU）。
	CPUInfo *CPUInfo `json:"cpuinfo"`
	// CPUs 是 CPU 核数（PVE 7/8；PVE 9 缺失时为 0，核数优先取 CPUInfo.Cpus）。
	CPUs int `json:"cpus"`
	// MaxCPU 是最大 CPU 核数（PVE 7/8；PVE 9 缺失时为 0）。
	MaxCPU int `json:"maxcpu"`
	// Memory 是内存用量对象（PVE 9 新增；PVE 7/8 缺失时为 nil，用量
	// 回退 MaxMem/Mem）。
	Memory *MemoryInfo `json:"memory"`
	// Mem 是已用内存字节数（PVE 7/8；PVE 9 缺失时为 0）。
	Mem int64 `json:"mem"`
	// MaxMem 是内存总量字节数（PVE 7/8；PVE 9 缺失时为 0）。
	MaxMem int64 `json:"maxmem"`
	// Rootfs 是根分区用量信息（PVE 8.2+ 为对象、PVE 7 为裸数字，双格式
	// 兼容见 RootfsInfo；PVE 9 仍为对象格式）。
	Rootfs RootfsInfo `json:"rootfs"`
	// MaxRootfs 是根分区总容量字节数（PVE 7 的总量字段；PVE 8/9 缺失时
	// 零值容错，总量回退 Rootfs.Total）。
	MaxRootfs int64 `json:"maxrootfs"`
	// Uptime 是在线时长（秒，PVE 7/8/9 都有）。
	Uptime int64 `json:"uptime"`
	// Version 是 PVE 版本号（PVE 7/8；PVE 9 已移除，回退 PveVersion）。
	Version string `json:"version"`
	// PveVersion 是 PVE 版本号（PVE 9 新增，替代 version 字段；
	// PVE 7/8 缺失时回退 Version）。
	PveVersion string `json:"pveversion"`
	// KVersion 是内核版本（PVE 7/8/9 都有）。
	KVersion string `json:"kversion"`
	// Loadavg 是负载均值数组（PVE 原文透传，不强行数值化，避免精度损失）。
	Loadavg []string `json:"loadavg"`
}

// CPUInfo 是 PVE 9 的 cpuinfo 对象：物理 CPU 数量与核数信息。
type CPUInfo struct {
	// Cpus 是逻辑 CPU 数量。
	Cpus int `json:"cpus"`
	// Cores 是每颗物理 CPU 的核数。
	Cores int `json:"cores"`
	// Sockets 是物理 CPU 插槽数。
	Sockets int `json:"sockets"`
	// Model 是 CPU 型号。
	Model string `json:"model"`
}

// MemoryInfo 是 PVE 9 的 memory 对象：内存总量与用量。
type MemoryInfo struct {
	// Total 是内存总量字节数。
	Total int64 `json:"total"`
	// Used 是已用内存字节数。
	Used int64 `json:"used"`
	// Free 是空闲内存字节数。
	Free int64 `json:"free"`
	// Available 是可用内存字节数。
	Available int64 `json:"available"`
}

// RootfsInfo 是根分区用量信息的双格式兼容容器：PVE 8 的 rootfs 是对象
// {"total":X,"used":Y,"avail":Z,"percent":P}，PVE 7 的 rootfs 是裸数字
// （已用字节数）。通过自定义 UnmarshalJSON 兼容两种格式；两种都解析失败
// 时全零值，绝不报错（容错设计，见 openspec 设计 D2）。
type RootfsInfo struct {
	// Total 是根分区总容量字节数（PVE 8 的 total；PVE 7 裸数字格式时为 0，
	// 总量由 MaxRootfs 提供）。
	Total int64 `json:"total"`
	// Used 是根分区已用字节数（PVE 8 的 used 或 PVE 7 的裸数字）。
	Used int64 `json:"used"`
}

// UnmarshalJSON 兼容 PVE 7/8 两种 rootfs 格式：先用 struct 内联 json tag
// 按对象尝试解析，若 total/used/avail 至少一个非零则采用对象语义（PVE 8）；
// 否则把原始值按裸数字解析为 Used（PVE 7）。两种都解析失败时保持全零值，
// 不返回错误（D2 容错：单个字段解析失败绝不拖垮整体请求）。
func (r *RootfsInfo) UnmarshalJSON(data []byte) error {
	var obj struct {
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
		Avail int64 `json:"avail"`
	}
	if err := json.Unmarshal(data, &obj); err == nil &&
		(obj.Total != 0 || obj.Used != 0 || obj.Avail != 0) {
		r.Total, r.Used = obj.Total, obj.Used
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		r.Used = n
	}
	return nil
}

// NodeStatus 调用 GET /nodes/{node}/status 返回节点的实时运行状态。
func (c *Client) NodeStatus(ctx context.Context, node string) (*NodeStatusData, error) {
	path := fmt.Sprintf("/nodes/%s/status", node)
	raw, err := c.doJSON(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	st, err := decodeData[NodeStatusData](raw)
	if err != nil {
		return nil, fmt.Errorf("pve: node status on %s: %w", node, err)
	}
	return &st, nil
}

// PveBool 是容错布尔类型：兼容 JSON true/false 与数字 1/0 两种形态
// （PVE 部分端点如 /nodes/{node}/network 的 active 字段返回数字 1/0，
// 另一些端点返回布尔）。解析失败时容错为 false，绝不报错。
type PveBool bool

// UnmarshalJSON 解析布尔或数字形态的布尔值："true"/"1"→true，
// "false"/"0"→false，其余值（如字符串 "yes"、对象、null 之外的形态）
// 容错为 false 不返回错误。
func (b *PveBool) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true", "1":
		*b = true
	default:
		// "false"、"0" 及一切无法识别的形态一律按 false 容错。
		*b = false
	}
	return nil
}

// NetIface 是 GET /nodes/{node}/network 返回的单个网络接口条目。
type NetIface struct {
	// Iface 是接口名（如 vmbr0、eth0）。
	Iface string `json:"iface"`
	// Type 是接口类型（如 bridge、eth）。
	Type string `json:"type"`
	// Address 是接口 IP 地址。
	Address string `json:"address"`
	// Active 是否启用；PVE 的 active 可能是布尔或数字 1/0（PveBool 双格式
	// 兼容），用指针容错缺失字段。
	Active *PveBool `json:"active"`
}

// NodeNetwork 调用 GET /nodes/{node}/network 返回节点的结构化网络接口列表。
func (c *Client) NodeNetwork(ctx context.Context, node string) ([]NetIface, error) {
	path := fmt.Sprintf("/nodes/%s/network", node)
	raw, err := c.doJSON(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	ifaces, err := decodeData[[]NetIface](raw)
	if err != nil {
		return nil, fmt.Errorf("pve: node network on %s: %w", node, err)
	}
	return ifaces, nil
}

// NodeIO 是节点级网络吞吐（bytes/s），取自 rrddata 时间序列的最后一个点。
type NodeIO struct {
	// NetIn 是当前接收吞吐（bytes/s）。
	NetIn float64 `json:"net_in"`
	// NetOut 是当前发送吞吐（bytes/s）。
	NetOut float64 `json:"net_out"`
}

// rrdPoint 是 GET /nodes/{node}/rrddata 返回的单个时间序列点。
type rrdPoint struct {
	// NetIn 是该时刻的接收吞吐（bytes/s）。
	NetIn float64 `json:"netin"`
	// NetOut 是该时刻的发送吞吐（bytes/s）。
	NetOut float64 `json:"netout"`
}

// NodeNetIO 调用 GET /nodes/{node}/rrddata?timeframe=hour 返回节点当前
// 网络吞吐。PVE 9 的 netstat 端点只返回 VM 网络设备计数器
// （dev/vmid/in/out），无物理网卡流量，不可用于节点级监控；节点级流量
// 改用 rrddata：解析数组并取最后一个元素的 netin/netout（bytes/s）作为
// 当前吞吐（最近采样点）。数组为空时返回全零（容错不报错）。
func (c *Client) NodeNetIO(ctx context.Context, node string) (*NodeIO, error) {
	path := fmt.Sprintf("/nodes/%s/rrddata", node)
	query := url.Values{"timeframe": {"hour"}}
	raw, err := c.doJSON(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	points, err := decodeData[[]rrdPoint](raw)
	if err != nil {
		return nil, fmt.Errorf("pve: node rrddata on %s: %w", node, err)
	}
	out := &NodeIO{}
	if len(points) > 0 {
		last := points[len(points)-1]
		out.NetIn, out.NetOut = last.NetIn, last.NetOut
	}
	return out, nil
}
