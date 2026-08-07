package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// VMStatus 是 GET /nodes/{node}/qemu 返回的一项。内存和磁盘值以字节为
// 单位；CPU 是当前使用的配置核数占比，Cpus 是配置中最大可用 CPU 数量
// （sockets×cores，并以宿主机核数为上限）。已停止的 VM 会省略大多数字段，
// 解码为零值。Template 标记该 VM 是 PVE 模板（1）还是普通 VM（0）：
// 模板是供克隆使用的基础镜像而非运行实体，导入与未托管列表都排除它。
type VMStatus struct {
	VMID     int64   `json:"vmid"`
	Name     string  `json:"name,omitempty"`
	Status   string  `json:"status,omitempty"`
	CPU      float64 `json:"cpu,omitempty"`
	Cpus     int64   `json:"cpus,omitempty"`
	Mem      int64   `json:"mem,omitempty"`
	MaxMem   int64   `json:"maxmem,omitempty"`
	Disk     int64   `json:"disk,omitempty"`
	MaxDisk  int64   `json:"maxdisk,omitempty"`
	Uptime   int64   `json:"uptime,omitempty"`
	Template int64   `json:"template,omitempty"`
}

// CreateVMParams 是 POST /nodes/{node}/qemu 支持的参数。零值字段会被省略
// 并交给 PVE 默认处理；Extra 原样携带额外的配置键（ostype 等）。
//
// CreateVM 是单步部署调用：镜像导入、网络和 cloud-init 都在单个 qmcreate
// 任务中完成。Scsi0 接受由 DiskImportString 构建的磁盘字符串（例如
// "local-lvm:0,import-from=..."，PVE 7.0+），用于在创建 VM 的同时导入
// 云镜像。IDE2 接受 cloud-init 数据磁盘字符串（例如 "local-lvm:cloudinit"）：
// 没有它，PVE 会忽略 ciuser/cipassword/ipconfig0/nameserver。
type CreateVMParams struct {
	VMID   int64
	Name   string
	Memory int64 // MiB
	Cores  int
	CPU    string // 模拟的 CPU 类型，例如 "x86-64-v2-AES"

	// 单步部署字段。
	Scsi0        string // 磁盘字符串，例如 DiskImportString(storage, source)
	IDE2         string // cloud-init 数据磁盘，例如 "local-lvm:cloudinit"
	Net0         string // 例如 "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0"
	BootDisk     string // 启动控制器，例如 "scsi0"
	ScsiHW       string // 例如 "virtio-scsi-single" 或 "virtio-scsi-pci"
	CIUser       string // cloud-init 用户，例如镜像的 default_user
	CIPassword   string // cloud-init 密码
	IPConfig0    string // 静态 IP，例如 "ip=10.0.0.5/24,gw=10.0.0.1"
	Nameserver   string
	SearchDomain string

	Extra map[string]string
}

func (p CreateVMParams) body() map[string]any {
	b := map[string]any{"vmid": p.VMID}
	if p.Name != "" {
		b["name"] = p.Name
	}
	if p.Memory > 0 {
		b["memory"] = p.Memory
	}
	if p.Cores > 0 {
		b["cores"] = p.Cores
	}
	if p.CPU != "" {
		b["cpu"] = p.CPU
	}
	if p.Scsi0 != "" {
		b["scsi0"] = p.Scsi0
	}
	if p.IDE2 != "" {
		b["ide2"] = p.IDE2
	}
	if p.Net0 != "" {
		b["net0"] = p.Net0
	}
	if p.BootDisk != "" {
		b["bootdisk"] = p.BootDisk
	}
	if p.ScsiHW != "" {
		b["scsihw"] = p.ScsiHW
	}
	if p.CIUser != "" {
		b["ciuser"] = p.CIUser
	}
	if p.CIPassword != "" {
		b["cipassword"] = p.CIPassword
	}
	if p.IPConfig0 != "" {
		b["ipconfig0"] = p.IPConfig0
	}
	if p.Nameserver != "" {
		b["nameserver"] = p.Nameserver
	}
	if p.SearchDomain != "" {
		b["searchdomain"] = p.SearchDomain
	}
	for k, v := range p.Extra {
		b[k] = v
	}
	return b
}

// CreateVM 通过 POST /nodes/{node}/qemu 创建 VM 并返回 qmcreate 任务的
// 任务 ID（UPID）。VMID 由调用方指定。一次调用覆盖 VM 创建、磁盘供给——
// 包括通过 DiskImportString 构建的 scsi0 磁盘字符串进行镜像导入——
// cloud-init 数据磁盘（IDE2，例如 "local-lvm:cloudinit"）、网络（Net0）
// 与 cloud-init 注入（CIUser/CIPassword/IPConfig0/...）；导入时间包含在
// qmcreate 任务内，调用方通常用 WaitTask 等待该任务。
func (c *Client) CreateVM(ctx context.Context, node string, params CreateVMParams) (string, error) {
	path := fmt.Sprintf("/nodes/%s/qemu", node)
	raw, err := c.doJSON(ctx, http.MethodPost, path, nil, params.body())
	if err != nil {
		return "", err
	}
	return decodeUPID(raw)
}

// StartVM 启动一个 VM（POST /nodes/{node}/qemu/{vmid}/status/start）并
// 返回任务 ID。
func (c *Client) StartVM(ctx context.Context, node string, vmid int64) (string, error) {
	raw, err := c.doJSON(ctx, http.MethodPost, vmStatusPath(node, vmid, "start"), nil, nil)
	if err != nil {
		return "", err
	}
	return decodeUPID(raw)
}

// StopVM 停止一个 VM（POST /nodes/{node}/qemu/{vmid}/status/stop）并
// 返回任务 ID。强制停止会先中止进行中的优雅关机：PVE 的 QEMU 停止端点
// 没有字面上的 "force" 参数（该参数仅存在于容器），因此 force 映射到
// PVE 8.2+ 的 overrule-shutdown 标志。
func (c *Client) StopVM(ctx context.Context, node string, vmid int64, force bool) (string, error) {
	var body any
	if force {
		body = map[string]any{"overrule-shutdown": 1}
	}
	raw, err := c.doJSON(ctx, http.MethodPost, vmStatusPath(node, vmid, "stop"), nil, body)
	if err != nil {
		return "", err
	}
	return decodeUPID(raw)
}

// RebootVM 重启一个 VM（POST /nodes/{node}/qemu/{vmid}/status/reboot）并
// 返回任务 ID。
func (c *Client) RebootVM(ctx context.Context, node string, vmid int64) (string, error) {
	raw, err := c.doJSON(ctx, http.MethodPost, vmStatusPath(node, vmid, "reboot"), nil, nil)
	if err != nil {
		return "", err
	}
	return decodeUPID(raw)
}

// ListVMs 返回节点上的全部 VM（GET /nodes/{node}/qemu）。
func (c *Client) ListVMs(ctx context.Context, node string) ([]VMStatus, error) {
	path := fmt.Sprintf("/nodes/%s/qemu", node)
	raw, err := c.doJSON(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	vms, err := decodeData[[]VMStatus](raw)
	if err != nil {
		return nil, fmt.Errorf("pve: list VMs on %s: %w", node, err)
	}
	return vms, nil
}

// VMConfig 是 VM 的配置映射（GET /nodes/{node}/qemu/{vmid}/config）。PVE
// 以字符串形式返回所有值；数值访问器按需解析。
type VMConfig map[string]string

// parseConfig 将配置负载转换为字符串映射，容忍以 JSON 数字或布尔值
// 形式到达的标量值。
func parseConfig(raw json.RawMessage) (VMConfig, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("parse VM config: %w", err)
	}
	cfg := make(VMConfig, len(fields))
	for k, v := range fields {
		cfg[k] = string(v)
	}
	return cfg, nil
}

// String 返回配置键的值；当它是 JSON 字符串时去掉引号（不存在时
// 返回 ""）。
func (c VMConfig) String(key string) string {
	raw, ok := c[key]
	if !ok {
		return ""
	}
	return unquoteJSONString(raw)
}

// Int 将配置键解析为整数，必要时去掉 JSON 字符串的引号。
func (c VMConfig) Int(key string) (int, error) {
	v, err := c.Int64(key)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

// Int64 将配置键解析为 int64。
func (c VMConfig) Int64(key string) (int64, error) {
	raw, ok := c[key]
	if !ok {
		return 0, fmt.Errorf("pve: VM config has no %q", key)
	}
	raw = unquoteJSONString(raw)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pve: VM config %q = %q is not an integer", key, c[key])
	}
	return v, nil
}

// unquoteJSONString 去掉 JSON 字符串值两端的引号。
func unquoteJSONString(raw string) string {
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1]
	}
	return raw
}

// Cores 返回配置的核数。
func (c VMConfig) Cores() (int, error) { return c.Int("cores") }

// MemoryMB 返回配置的内存（MiB）。
func (c VMConfig) MemoryMB() (int64, error) { return c.Int64("memory") }

// CPUType 返回模拟的 CPU 类型。
func (c VMConfig) CPUType() string { return c.String("cpu") }

// BootDisk 返回启动磁盘控制器名称（例如 "scsi0"）。
func (c VMConfig) BootDisk() string { return c.String("bootdisk") }

// GetVMConfig 读取 VM 配置（GET /nodes/{node}/qemu/{vmid}/config）。
func (c *Client) GetVMConfig(ctx context.Context, node string, vmid int64) (VMConfig, error) {
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid)
	raw, err := c.doJSON(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	return parseConfig(raw)
}

// VMConfigParams 是 PUT /nodes/{node}/qemu/{vmid}/config 支持的字段。
// 为 nil 的字段保持不变；Extra 原样携带额外的配置键（net0、bootdisk、
// ciuser、ipconfig 等）。
type VMConfigParams struct {
	Cores    *int
	MemoryMB *int64
	CPU      *string
	Extra    map[string]string
}

func (p VMConfigParams) body() map[string]any {
	b := make(map[string]any, len(p.Extra)+3)
	if p.Cores != nil {
		b["cores"] = *p.Cores
	}
	if p.MemoryMB != nil {
		b["memory"] = *p.MemoryMB
	}
	if p.CPU != nil {
		b["cpu"] = *p.CPU
	}
	for k, v := range p.Extra {
		b[k] = v
	}
	return b
}

// SetVMConfig 更新 VM 选项（PUT /nodes/{node}/qemu/{vmid}/config）并
// 返回任务 ID。该端点在 PVE 7/8/9 上是同步的：PVE 会将变更应用到运行中
// 的配置或待定变更（取决于 VM 状态），并回复 {"data": null} 而不是 UPID，
// 因此返回的任务 ID 始终为空，无需轮询。
func (c *Client) SetVMConfig(ctx context.Context, node string, vmid int64, params VMConfigParams) (string, error) {
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid)
	_, err := c.doJSON(ctx, http.MethodPut, path, nil, params.body())
	if err != nil {
		return "", err
	}
	return "", nil
}

// ResizeDisk 扩容 VM 磁盘（PUT /nodes/{node}/qemu/{vmid}/resize，按标准
// API 该端点为 PUT 而非 POST）。sizeGB 是 GiB 为单位的绝对目标大小，必须
// 大于当前大小；收缩会被 PVE 服务端以及调用方的服务层拒绝。
//
// 不同 PVE 版本的响应不同：PVE 7 同步应用扩容并返回 {"data": null}
// （空任务 ID，无需轮询），而 PVE 8/9 为异步扩容任务返回 UPID。两种形态
// 均已处理；调用方应仅对非空任务 ID 进行等待。
func (c *Client) ResizeDisk(ctx context.Context, node string, vmid int64, disk string, sizeGB int64) (string, error) {
	if disk == "" {
		return "", fmt.Errorf("pve: resize: empty disk name")
	}
	if sizeGB < 0 {
		return "", fmt.Errorf("pve: resize: negative size %dG", sizeGB)
	}
	path := fmt.Sprintf("/nodes/%s/qemu/%d/resize", node, vmid)
	body := map[string]any{"disk": disk, "size": FormatSizeGB(sizeGB)}
	raw, err := c.doJSON(ctx, http.MethodPut, path, nil, body)
	if err != nil {
		return "", err
	}
	if isEmptyData(raw) {
		// PVE 7 同步完成：扩容已生效。
		return "", nil
	}
	return decodeUPID(raw)
}

// DestroyVM 删除一个 VM（DELETE /nodes/{node}/qemu/{vmid}）并等待销毁
// 任务结束。purge 会将 VMID 从备份/复制任务及 HA 配置中移除。返回的
// UPID 是已完成任务的 ID。
func (c *Client) DestroyVM(ctx context.Context, node string, vmid int64, purge bool) (string, error) {
	path := fmt.Sprintf("/nodes/%s/qemu/%d", node, vmid)
	var query url.Values
	if purge {
		query = url.Values{"purge": {"1"}}
	}
	raw, err := c.doJSON(ctx, http.MethodDelete, path, query, nil)
	if err != nil {
		return "", err
	}
	upid, err := decodeUPID(raw)
	if err != nil {
		return "", err
	}
	if _, err := c.WaitTask(ctx, node, upid, DefaultWaitInterval, DefaultWaitTimeout); err != nil {
		return upid, err
	}
	return upid, nil
}

// vmStatusPath 构建 /nodes/{node}/qemu/{vmid}/status/{action}。
func vmStatusPath(node string, vmid int64, action string) string {
	return fmt.Sprintf("/nodes/%s/qemu/%d/status/%s", node, vmid, action)
}
