package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"spark/model"
	"spark/repository"
	"spark/service"
)

// VM 生命周期域补充的 API 错误码，沿用 error.go 中基础错误码的命名风格。
const (
	// CodeVMNotReady：VM 还没有对应的 PVE 实体（供给尚未完成）
	// 或其 PVE VM 已不存在；生命周期操作被拒绝
	// （409 —— 资源存在但不在可用状态）。
	CodeVMNotReady = "vm_not_ready"
	// CodeDiskShrinkNotAllowed：请求的磁盘大小小于当前值
	// （422 —— 请求格式合法，但被领域规则拒绝）。
	CodeDiskShrinkNotAllowed = "disk_shrink_not_allowed"
	// CodeImageNotAvailableInZone：该镜像无法在请求的 zone 中使用。
	// 选择 400（而非 422）是因为请求参数明显无法满足：客户端请求的
	// image/zone 组合无法被提供，即参数集非法。
	CodeImageNotAvailableInZone = "image_not_available_in_zone"
	// CodeVMNotFoundOnNode：节点 PVE 可达，但请求的 pve_vmid 不在该
	// 节点上（404 —— 区别于 zone/node 自身不存在的 not_found）。
	CodeVMNotFoundOnNode = "vm_not_found_on_node"
	// CodeVMAlreadyManaged：该节点上的 pve_vmid 已被托管，重复认领
	// 被拒绝（409 —— 区别于一般资源冲突的 conflict）。
	CodeVMAlreadyManaged = "vm_already_managed"
	// CodeInvalidVMID：路径 id 无法解析为数字本地行 id 或
	// ext-{nodeID}-{vmid} 合成标识（400）。
	CodeInvalidVMID = "invalid_vm_id"
	// CodeOperationLogFailed：PVE 已受理操作，但操作记录写入失败
	// （500 —— 审计完整性优先，前端提示可刷新确认）。
	CodeOperationLogFailed = "operation_log_failed"
)

// mapVMServiceError 将 service 层错误映射为统一的 API 错误契约。
// 共享及 zone/IP-pool 类型委托给 mapServiceErrorExtended；
// VM 域的错误类型在这里映射。
func mapVMServiceError(err error) error {
	var serr *service.Error
	if !errors.As(err, &serr) {
		return mapServiceErrorExtended(err)
	}
	switch serr.Kind {
	case service.KindVMNotReady:
		return NewError(http.StatusConflict, CodeVMNotReady, serr.Message)
	case service.KindDiskShrinkNotAllowed:
		return NewError(http.StatusUnprocessableEntity, CodeDiskShrinkNotAllowed, serr.Message)
	case service.KindImageNotAvailable:
		return NewError(http.StatusBadRequest, CodeImageNotAvailableInZone, serr.Message)
	case service.KindVMNotFoundOnNode:
		return NewError(http.StatusNotFound, CodeVMNotFoundOnNode, serr.Message)
	case service.KindVMAlreadyManaged:
		return NewError(http.StatusConflict, CodeVMAlreadyManaged, serr.Message)
	case service.KindInvalidVMRef:
		return NewError(http.StatusBadRequest, CodeInvalidVMID, serr.Message)
	case service.KindOperationLogFailed:
		return NewError(http.StatusInternalServerError, CodeOperationLogFailed, serr.Message)
	default:
		return mapServiceErrorExtended(err)
	}
}

// VMHandler 提供 /vms 路由。
type VMHandler struct {
	svc *service.VMService
}

// NewVMHandler 创建一个由 svc 支撑的 VMHandler。
func NewVMHandler(svc *service.VMService) *VMHandler {
	return &VMHandler{svc: svc}
}

// RegisterVMsRoutes 在 rg 上挂载 VM 生命周期路由。由 router 以 /vms
// 分组调用。GET /vms 与 GET /vms/:id 和 POST/PATCH/DELETE 路由
// 共存而不冲突（gin 按 HTTP 方法分别维护路由树）。规格变更与销毁
// 操作遵循 REST 方法语义：PATCH /vms/:id 是规格的部分更新，
// DELETE /vms/:id 是销毁。生命周期操作（start/stop/restart/destroy）
// 与操作记录查询的 :id 既接受数字本地行 id 也接受 external 合成标识
// ext-{nodeID}-{vmid}（设计 D2）。
func RegisterVMsRoutes(rg *gin.RouterGroup, svc *service.VMService) {
	h := NewVMHandler(svc)
	rg.POST("", Handler(h.Create))
	rg.GET("", Handler(h.List))
	rg.POST("/import", Handler(h.Import))
	rg.GET("/:id", Handler(h.Get))
	rg.POST("/:id/start", Handler(h.Start))
	rg.POST("/:id/stop", Handler(h.Stop))
	rg.POST("/:id/restart", Handler(h.Restart))
	rg.GET("/:id/operations", Handler(h.Operations))
	rg.PATCH("/:id", Handler(h.Resize))
	rg.DELETE("/:id", Handler(h.Destroy))
}

// vmResponse 是公开的 VM 负载。password 永不包含在响应中；
// provision_error 为空时省略；VM 尚未在 PVE 上创建时省略 pve_vmid。
// Source 标识 VM 来源（spark_created/claimed/external，设计 D3）。
type vmResponse struct {
	ID             int64     `json:"id"`
	UUID           string    `json:"uuid"`
	Name           string    `json:"name"`
	CPU            int       `json:"cpu"`
	MemMB          int64     `json:"mem_mb"`
	DiskGB         int64     `json:"disk_gb"`
	ImageID        *int64    `json:"image_id,omitempty"`
	StorageTypeID  *int64    `json:"storage_type_id,omitempty"`
	ZoneID         int64     `json:"zone_id"`
	NodeID         int64     `json:"node_id"`
	PVEVmid        int64     `json:"pve_vmid,omitempty"`
	IP             string    `json:"ip,omitempty"`
	Status         string    `json:"status"`
	ProvisionError string    `json:"provision_error,omitempty"`
	Source         string    `json:"source"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// toVMResponse 将 repository 的 VM 行转换为公开负载。
func toVMResponse(vm *repository.VMWithIP, status string) vmResponse {
	return vmResponse{
		ID:             vm.VM.ID,
		UUID:           vm.VM.UUID,
		Name:           vm.VM.Name,
		CPU:            vm.VM.CPU,
		MemMB:          vm.VM.MemMB,
		DiskGB:         vm.VM.DiskGB,
		ImageID:        vm.VM.ImageID,
		StorageTypeID:  vm.VM.StorageTypeID,
		ZoneID:         vm.VM.ZoneID,
		NodeID:         vm.VM.NodeID,
		PVEVmid:        vm.VM.PVEVmid,
		IP:             vm.IP,
		Status:         status,
		ProvisionError: vm.VM.ProvisionError,
		Source:         vm.VM.Source,
		CreatedAt:      vm.VM.CreatedAt,
		UpdatedAt:      vm.VM.UpdatedAt,
	}
}

// localVMStatus 推导过渡状态：PVE VM 尚不存在时为 "creating"，
// 异步链路失败时为 "failed"，否则为 "ready" —— 仅作为 create
// 响应的临时值；list/detail/resize 响应携带从 PVE 实时读取的透传状态。
func localVMStatus(vm *repository.VMWithIP) string {
	switch {
	case vm.VM.ProvisionError != "":
		return model.VMStateFailed
	case vm.VM.PVEVmid == 0:
		return model.VMStateCreating
	default:
		return model.VMStateReady
	}
}

// vmListItem 是公开的透传 VM 负载（task 8.1/8.2 + 全部 PVE 虚拟机可见）：
// 7.x 的 vmResponse 元数据加上从 PVE 读取的实时运行部分（设计 D1）。
// 规格大小（cpu/mem_mb/disk_gb）是创建时请求的本地 DB 值（external 条目
// 为 PVE 摘要值）；运行指标（cpu_usage、mem/maxmem、disk/maxdisk（字节）、
// uptime）来自 PVE，当 VM 没有 PVE 实体（creating/failed）或处于停止状态
// 时省略。
//
// external 条目（设计 D2）覆盖 id/uuid/created_at/updated_at 四个字段：
// id 为合成标识 ext-{nodeID}-{vmid}（字符串），其余三个返回空字符串，
// 本地行字段保持零值。
type vmListItem struct {
	vmResponse
	ID        any     `json:"id"`         // 本地行为数字 id，external 为合成标识
	UUID      string  `json:"uuid"`       // external 为空字符串
	CreatedAt any     `json:"created_at"` // 本地行为时间戳，external 为空字符串
	UpdatedAt any     `json:"updated_at"` // 本地行为时间戳，external 为空字符串
	CPUUsage  float64 `json:"cpu_usage,omitempty"`
	Mem       int64   `json:"mem,omitempty"`
	MaxMem    int64   `json:"maxmem,omitempty"`
	Disk      int64   `json:"disk,omitempty"`
	MaxDisk   int64   `json:"maxdisk,omitempty"`
	Uptime    int64   `json:"uptime,omitempty"`
}

// nodeWarning 是 GET /vms 的部分失败通知：实时查询失败的节点，
// 其 VM 不会出现在列表中（task 8.3）。
type nodeWarning struct {
	Node  string `json:"node"`
	Error string `json:"error"`
}

// toVMListItem 将合并后的 service 条目转换为公开负载。
func toVMListItem(item *service.VMListItem) vmListItem {
	out := vmListItem{vmResponse: toVMResponse(&item.VM, item.Status)}
	out.ID = item.VM.VM.ID
	out.UUID = item.VM.VM.UUID
	out.CreatedAt = item.VM.VM.CreatedAt
	out.UpdatedAt = item.VM.VM.UpdatedAt
	if item.ExternalID != "" {
		// external 条目：合成 id，uuid/created_at/updated_at 返回空。
		out.ID = item.ExternalID
		out.UUID = ""
		out.CreatedAt = ""
		out.UpdatedAt = ""
	}
	if item.Live != nil {
		out.CPUUsage = item.Live.CPUUsage
		out.Mem = item.Live.Mem
		out.MaxMem = item.Live.MaxMem
		out.Disk = item.Live.Disk
		out.MaxDisk = item.Live.MaxDisk
		out.Uptime = item.Live.Uptime
	}
	return out
}

// List 处理 GET /vms：对每个启用节点各发起一次 PVE 调用，并与本地
// 元数据合并（8.1 + 全部 PVE 虚拟机可见，设计 D1/D2/D3）。分页由共享的
// limit/offset 查询参数选择，作用于合并排序后的完整条目（内存分页），
// X-Total-Count 头报告合并后的条目总数。失败的节点在 warnings 中报告
// （8.3）；warnings 始终是数组（所有节点都响应时为空）。
func (h *VMHandler) List(c *gin.Context) error {
	limit, offset, err := parsePagination(c)
	if err != nil {
		return err
	}
	items, warnings, total, err := h.svc.ListVMs(c.Request.Context(), limit, offset)
	if err != nil {
		return mapVMServiceError(err)
	}
	vms := make([]vmListItem, 0, len(items))
	for i := range items {
		vms = append(vms, toVMListItem(&items[i]))
	}
	warns := make([]nodeWarning, 0, len(warnings))
	for _, w := range warnings {
		warns = append(warns, nodeWarning{Node: w.Node, Error: w.Error})
	}
	setTotalCount(c, total)
	c.JSON(http.StatusOK, gin.H{"vms": vms, "warnings": warns})
	return nil
}

// Get 处理 GET /vms/:id：本地元数据加实时状态透传（8.2）。
// 节点故障时返回 503 node_unavailable，而不是伪造 creating 状态（8.3）。
func (h *VMHandler) Get(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	item, err := h.svc.GetVM(c.Request.Context(), id)
	if err != nil {
		return mapVMServiceError(err)
	}
	c.JSON(http.StatusOK, toVMListItem(item))
	return nil
}

// Create 处理 POST /vms：校验请求、分配 IP、持久化记录、
// 触发分离的供给链路，并以 201 返回 VM —— 包含明文 IP，
// password 永不回显。
func (h *VMHandler) Create(c *gin.Context) error {
	var req service.CreateVMRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	vm, err := h.svc.CreateVM(c.Request.Context(), req)
	if err != nil {
		return mapVMServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/vms/%d", vm.VM.ID))
	// 供给链路是分离运行的，到这里还未完成，因此状态始终是
	// "creating" —— PVE 还没有这个 VM。
	c.JSON(http.StatusCreated, toVMResponse(vm, localVMStatus(vm)))
	return nil
}

// Import 处理 POST /vms/import：把节点 PVE 上已有的 VM 认领为托管 VM。
// 请求体 {zone_id, node_id, pve_vmid} 必填，{name, ip} 可选（zero 表示
// 缺省：取 PVE 配置名 / 不分配 IP）。成功返回 201 + Location 头
// + 完整 VMListItem（透传状态：认领是同步的，PVE 实体已存在，因此像
// Resize 一样经 GetVM 读取实时状态）；GetVM 读取失败时降级返回无实时字段
// 的 vmResponse（事务已提交，客户端若收到 5xx 会重试并撞上 409
// vm_already_managed，所以查询失败不能令请求失败）。
// 错误区分两种 404：zone/node 不存在 -> not_found；vmid 不在该节点 PVE
// 上 -> vm_not_found_on_node；重复托管 -> 409 vm_already_managed。
func (h *VMHandler) Import(c *gin.Context) error {
	var req service.ImportVMRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	vm, err := h.svc.ImportVM(c.Request.Context(), req)
	if err != nil {
		return mapVMServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/vms/%d", vm.VM.ID))
	item, err := h.svc.GetVM(c.Request.Context(), vm.VM.ID)
	if err != nil {
		// 降级：导入已落库，返回 201 + 无实时字段的本地形态（与 Create
		// 的降级思路一致，客户端仍能拿到 VM 元数据与 Location）。
		c.JSON(http.StatusCreated, toVMResponse(vm, localVMStatus(vm)))
		return nil
	}
	c.JSON(http.StatusCreated, toVMListItem(item))
	return nil
}

// Start 处理 POST /vms/:id/start（:id 为数字本地行 id 或 external 合成
// 标识，设计 D2）。选择 202 + {accepted: true} 而非返回 PVE 任务 ID：该
// 操作是异步分发的，客户端没有任务轮询端点 —— VM 的真实状态通过透传读取
// （batch 8）。Location 头指向透传状态端点 GET /vms/:id，客户端可在那里
// 观察结果；external（ext- 标识）没有可用的 GET 端点（详情仅支持数字
// 本地行 id），因此省略 Location 头（reviewer-3）。
func (h *VMHandler) Start(c *gin.Context) error {
	id := c.Param("id")
	if err := h.svc.Start(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	h.setLifecycleLocation(c, id)
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
	return nil
}

// Stop 处理 POST /vms/:id/stop（干净的 ACPI 关机；见
// VMService.Stop）。Location 头指向透传状态端点 GET /vms/:id（external
// 标识省略，同 Start）。
func (h *VMHandler) Stop(c *gin.Context) error {
	id := c.Param("id")
	if err := h.svc.Stop(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	h.setLifecycleLocation(c, id)
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
	return nil
}

// Restart 处理 POST /vms/:id/restart。Location 头指向透传状态
// 端点 GET /vms/:id（external 标识省略，同 Start）。
func (h *VMHandler) Restart(c *gin.Context) error {
	id := c.Param("id")
	if err := h.svc.Restart(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	h.setLifecycleLocation(c, id)
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
	return nil
}

// setLifecycleLocation 仅对数字本地行 id 设置 Location 头：GET /vms/:id
// 详情端点只接受数字 id，external（ext- 标识）没有可用的状态端点，设置
// Location 会指向不可用 URL（reviewer-3）。
func (h *VMHandler) setLifecycleLocation(c *gin.Context, id string) {
	if !strings.HasPrefix(id, "ext-") {
		c.Header("Location", fmt.Sprintf("/vms/%s", id))
	}
}

// Destroy 处理 DELETE /vms/:id（:id 为数字本地行 id 或 external 合成
// 标识，设计 D2/D4）。该操作是同步的（等待 PVE 销毁任务完成），成功时
// 返回无响应体的 204。DELETE 的幂等语义位于 service 层且保持如下差异：
// 本地行 id 对不存在的 VM 行在任何 PVE 调用之前就返回 404 not_found；
// 而 PVE 侧的 404（VM 已在节点上被移除）被视为"已销毁"（幂等成功），
// 仅执行本地清理。external 标识相反：PVE 侧的 404 在 resolveVMTarget
// 预检阶段就映射为 404 vm_not_found_on_node（资源不存在，非幂等成功）
// ——ext- 标识不存在"本地清理"可言（reviewer-6）。
func (h *VMHandler) Destroy(c *gin.Context) error {
	id := c.Param("id")
	if err := h.svc.Destroy(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	c.Status(http.StatusNoContent)
	return nil
}

// vmOperationResponse 是 GET /vms/:id/operations 的操作记录负载（设计
// D5）。user_id 预留（用户体系启用前恒为 NULL），响应中省略。
type vmOperationResponse struct {
	ID           int64     `json:"id"`
	NodeID       int64     `json:"node_id"`
	PVEVmid      int64     `json:"pve_vmid"`
	Action       string    `json:"action"`
	Result       string    `json:"result"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// Operations 处理 GET /vms/:id/operations：按时间倒序分页返回该 VM 的
// 生命周期操作记录（设计 D5）。:id 支持数字本地行 id（要求本地行存在，
// 否则 404 not_found）与 external 合成标识 ext-{nodeID}-{vmid}。分页
// 语义与列表一致：limit 默认 25 上限 100，X-Total-Count 报告匹配总数。
func (h *VMHandler) Operations(c *gin.Context) error {
	id := c.Param("id")
	limit, offset, err := parsePagination(c)
	if err != nil {
		return err
	}
	ops, total, err := h.svc.ListOperations(c.Request.Context(), id, limit, offset)
	if err != nil {
		return mapVMServiceError(err)
	}
	out := make([]vmOperationResponse, 0, len(ops))
	for _, op := range ops {
		out = append(out, vmOperationResponse{
			ID:           op.ID,
			NodeID:       op.NodeID,
			PVEVmid:      op.PVEVmid,
			Action:       op.Action,
			Result:       op.Result,
			ErrorMessage: op.ErrorMessage,
			CreatedAt:    op.CreatedAt,
		})
	}
	setTotalCount(c, total)
	c.JSON(http.StatusOK, gin.H{"operations": out})
	return nil
}

// resizeRequest 是 PATCH /vms/:id 的请求体；每个字段都可选，
// 至少一个必须设置。未出现的字段保持当前值。
type resizeRequest struct {
	CPU    *int   `json:"cpu"`
	MemMB  *int64 `json:"mem_mb"`
	DiskGB *int64 `json:"disk_gb"`
}

// Resize 处理 PATCH /vms/:id：对 VM 规格的部分更新。只应用请求体
// {cpu?, mem_mb?, disk_gb?} 中出现的字段；缺失或为 null 的字段
// 保持当前值（先改 PVE，再改本地行）。返回的 VM 携带真实透传状态：
// 规格变更应用后，从 PVE 重新读取实时状态（GetVM），
// 因此响应反映的是 VM 的实际状态而非临时值 ——
// 与 GET /vms/:id 结构相同。
func (h *VMHandler) Resize(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	var req resizeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	if _, err := h.svc.Resize(c.Request.Context(), id, req.CPU, req.MemMB, req.DiskGB); err != nil {
		return mapVMServiceError(err)
	}
	item, err := h.svc.GetVM(c.Request.Context(), id)
	if err != nil {
		return mapVMServiceError(err)
	}
	c.JSON(http.StatusOK, toVMListItem(item))
	return nil
}
