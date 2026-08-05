package model

import "time"

// Zone 是一个部署区域，用于将节点、IP 池和虚拟机分组。
type Zone struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// PVENode 是已注册的 Proxmox VE 节点，包含其 API 凭据。
type PVENode struct {
	ID             int64     `json:"id"`
	ZoneID         int64     `json:"zone_id"`
	Name           string    `json:"name"`
	Host           string    `json:"host"`
	APIUser        string    `json:"api_user"`
	APITokenSecret string    `json:"api_token_secret"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
}

// IPPool 是区域内的一个 IP 地址池。
type IPPool struct {
	ID          int64     `json:"id"`
	ZoneID      int64     `json:"zone_id"`
	Name        string    `json:"name"`
	NetworkCIDR string    `json:"network_cidr"`
	Gateway     string    `json:"gateway"`
	DNS         string    `json:"dns"`
	CreatedAt   time.Time `json:"created_at"`
}

// IPPoolNode 是 IP 池与节点之间的多对多关联，这些节点可以从池中提供地址
// （"可用"白名单）。
type IPPoolNode struct {
	IPPoolID int64 `json:"ip_pool_id"`
	NodeID   int64 `json:"node_id"`
}

// IP 状态值。
const (
	IPStatusFree = "free"
	IPStatusUsed = "used"
)

// IP 是 IP 池中的单个地址。
type IP struct {
	ID        int64     `json:"id"`
	PoolID    int64     `json:"pool_id"`
	IP        string    `json:"ip"`
	Status    string    `json:"status"`
	VMID      *int64    `json:"vm_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StorageType 用一个显示名称对 PVE 存储（如 local-ssd）进行抽象。
type StorageType struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	PVEStorage  string    `json:"pve_storage"`
	CreatedAt   time.Time `json:"created_at"`
}

// Image 是已注册的云镜像。NodeImages 将节点名称映射为该节点上镜像的
// 存储路径（或存在标记）。
type Image struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	DefaultUser string            `json:"default_user"`
	NodeImages  map[string]string `json:"node_images"`
	CreatedAt   time.Time         `json:"created_at"`
}

// VM 状态常量，用于虚拟机尚未（或暂时未）存在于 PVE 侧时。
// 实时状态本身是透传的（从 PVE 查询，不存储）。
const (
	VMStateCreating = "creating"
	// VMStateFailed 标记预配置链失败的虚拟机；
	// 失败消息记录在 vms.provision_error 中。
	VMStateFailed = "failed"
	// VMStateReady 是 batch 8 透传状态的过渡占位：
	// 虚拟机已存在于 PVE 上（已设置 pve_vmid，无预配置错误）。
	VMStateReady = "ready"
)

// VM 是虚拟机记录；实时状态不存储（对 PVE 的透传查询，见设计 D1）。
type VM struct {
	ID                int64  `json:"id"`
	UUID              string `json:"uuid"`
	Name              string `json:"name"`
	ZoneID            int64  `json:"zone_id"`
	NodeID            int64  `json:"node_id"`
	PVEVmid           int64  `json:"pve_vmid"`
	ImageID           int64  `json:"image_id"`
	StorageTypeID     int64  `json:"storage_type_id"`
	CPU               int    `json:"cpu"`
	MemMB             int64  `json:"mem_mb"`
	DiskGB            int64  `json:"disk_gb"`
	IPID              *int64 `json:"ip_id,omitempty"`
	PasswordEncrypted string `json:"password_encrypted,omitempty"`
	// ProvisionError 携带分离式 PVE 预配置链的脱敏失败消息
	// （预配置中或成功后为空）。
	ProvisionError string    `json:"provision_error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
