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
	ID     int64  `json:"id"`
	ZoneID int64  `json:"zone_id"`
	Name   string `json:"name"`
	// PveName 是 PVE 集群节点名，与业务名 Name 分离；
	// 空值表示沿用 Name。
	PveName string `json:"pve_name"`
	Host    string `json:"host"`
	// Port 是节点 API 端口；0/未登记时语义为默认端口 8006
	// （由 service 层保证非 0 值落库）。
	Port           int       `json:"port"`
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

// Image 是已注册的云镜像条目，包含名称、默认登录用户与下载地址
// （download_url）；镜像在各节点上的存在状态以 PVE 实时扫描为准，不落库。
type Image struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	DefaultUser string    `json:"default_user"`
	DownloadURL string    `json:"download_url"`
	CreatedAt   time.Time `json:"created_at"`
}

// Admin 是管理员账号（双身份认证的管理员身份域，设计 D3）。
// PasswordHash 保存 bcrypt 不可逆哈希（设计 D1），仅用于登录校验；
// json:"-" 保证序列化时永不对外返回，防止哈希泄露。
type Admin struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// User 状态常量（users.status 列取值，设计 D3）。
const (
	// UserStatusEnabled：启用状态，可正常登录与操作。
	UserStatusEnabled = "enabled"
	// UserStatusDisabled：禁用状态，登录与鉴权一律拒绝（设计 D4/D5）。
	UserStatusDisabled = "disabled"
)

// User 是前台用户账号（双身份认证的用户身份域，设计 D3）。
// PasswordHash 保存 bcrypt 不可逆哈希（设计 D1），仅用于登录校验；
// json:"-" 保证序列化时永不对外返回，防止哈希泄露。
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Name         string    `json:"name"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
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

// VM 来源标识常量（设计 D3）：列表与详情响应的 source 字段取值。
const (
	// VMSourceSparkCreated：由 Spark 镜像创建（vms.source 列默认值）。
	VMSourceSparkCreated = "spark_created"
	// VMSourceClaimed：已认领（原"导入"）的外部虚拟机（vms.source 列）。
	VMSourceClaimed = "claimed"
	// VMSourceExternal：PVE 上存在而本地无记录——实时差集判定，不落库。
	VMSourceExternal = "external"
)

// VM 是虚拟机记录；实时状态不存储（对 PVE 的透传查询，见设计 D1）。
type VM struct {
	ID      int64  `json:"id"`
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	ZoneID  int64  `json:"zone_id"`
	NodeID  int64  `json:"node_id"`
	PVEVmid int64  `json:"pve_vmid"`
	// ImageID 指向 vms.image_id；导入的已有 VM 没有关联云镜像，
	// 此时为 nil（SQL NULL），与 IPID 的可空指针风格一致。
	ImageID *int64 `json:"image_id"`
	// StorageTypeID 指向 vms.storage_type_id；导入的已有 VM 没有关联
	// 存储类型，此时为 nil（SQL NULL）。
	StorageTypeID *int64 `json:"storage_type_id"`
	CPU           int    `json:"cpu"`
	MemMB         int64  `json:"mem_mb"`
	DiskGB        int64  `json:"disk_gb"`
	IPID          *int64 `json:"ip_id,omitempty"`
	// PasswordEncrypted 是加密后的 VM 密码；导入的已有 VM 无密码，
	// 读取时 NULL 一律经 COALESCE 归一化为空字符串（见 vmCols）。
	PasswordEncrypted string `json:"password_encrypted,omitempty"`
	// ProvisionError 携带分离式 PVE 预配置链的脱敏失败消息
	// （预配置中或成功后为空）。
	ProvisionError string `json:"provision_error,omitempty"`
	// Source 标识 VM 的来源（spark_created/claimed，设计 D3）。external
	// 由列表接口对 PVE 全量摘要与本地记录实时差集判定，不落库。
	Source string `json:"source"`
	// UserID 归属用户（vms.user_id，设计 D3）：创建/认领时可选绑定，
	// nil（SQL NULL）表示无主 VM，与 source 互不冲突。
	UserID    *int64    `json:"user_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// VM 生命周期操作的动作与结果常量（vm_operations 列取值，设计 D5）。
const (
	VMOpActionStart   = "start"
	VMOpActionStop    = "stop"
	VMOpActionReboot  = "reboot"
	VMOpActionDestroy = "destroy"

	// VMOpResultAccepted：PVE 已受理该操作（受理成功）。
	VMOpResultAccepted = "accepted"
	// VMOpResultFailed：PVE 返回错误，error_message 记录失败原因。
	VMOpResultFailed = "failed"
)

// VMOperation 是一次已受理的虚拟机生命周期操作（启动/停止/重启/销毁）的
// 审计记录（设计 D5）。操作记录不随 vms 行删除而删除（无外键 ON DELETE），
// 供审计与排障使用。
type VMOperation struct {
	ID      int64  `json:"id"`
	NodeID  int64  `json:"node_id"`
	PVEVmid int64  `json:"pve_vmid"`
	Action  string `json:"action"`
	Result  string `json:"result"`
	// ErrorMessage 记录失败原因；受理成功时为空。
	ErrorMessage string `json:"error_message,omitempty"`
	// UserID 是 0008 迁移预留的可空列（用户体系启用前恒为 NULL）；
	// 实际操作者以 OperatorType / OperatorID 为准，本字段仅保留用于
	// 与旧 schema 对齐，避免与操作者语义混淆。
	UserID *int64 `json:"user_id,omitempty"`
	// OperatorType 记录实际操作者的身份域（admin/user，设计 D5/D8）：
	// admin 指管理员，user 指前台用户。
	OperatorType string `json:"operator_type,omitempty"`
	// OperatorID 是实际操作者在对应身份域表（admins/users）中的 ID；
	// 与 OperatorType 均为空（nil）表示旧记录（无操作者信息）。
	OperatorID *int64    `json:"operator_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// 镜像下载操作的动作与结果常量（image_operations 列取值）。
const (
	ImageOpActionDownload = "download"
	ImageOpResultRunning  = "running"
	ImageOpResultSuccess  = "success"
	ImageOpResultFailed   = "failed"
)

// ImageOperation 是一次已受理的镜像下载操作（下载到某节点）的持久化记录。
type ImageOperation struct {
	ID      int64  `json:"id"`
	ImageID int64  `json:"image_id"`
	NodeID  int64  `json:"node_id"`
	Action  string `json:"action"`
	Result  string `json:"result"`
	// ErrorMessage 记录失败原因；失败时非空。
	ErrorMessage string `json:"error_message,omitempty"`
	// UPID 是 PVE 受理后返回的任务 ID；尚未受理时为空。
	UPID string `json:"upid,omitempty"`
	// UserID 预留：用户体系（单独提案）启用前恒为 NULL。
	UserID    *int64    `json:"user_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
