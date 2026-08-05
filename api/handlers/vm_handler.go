package handlers

import (
	"errors"
	"fmt"
	"net/http"
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
// DELETE /vms/:id 是销毁。
func RegisterVMsRoutes(rg *gin.RouterGroup, svc *service.VMService) {
	h := NewVMHandler(svc)
	rg.POST("", Handler(h.Create))
	rg.GET("", Handler(h.List))
	rg.GET("/:id", Handler(h.Get))
	rg.POST("/:id/start", Handler(h.Start))
	rg.POST("/:id/stop", Handler(h.Stop))
	rg.POST("/:id/restart", Handler(h.Restart))
	rg.PATCH("/:id", Handler(h.Resize))
	rg.DELETE("/:id", Handler(h.Destroy))
}

// vmResponse 是公开的 VM 负载。password 永不包含在响应中；
// provision_error 为空时省略；VM 尚未在 PVE 上创建时省略 pve_vmid。
type vmResponse struct {
	ID             int64     `json:"id"`
	UUID           string    `json:"uuid"`
	Name           string    `json:"name"`
	CPU            int       `json:"cpu"`
	MemMB          int64     `json:"mem_mb"`
	DiskGB         int64     `json:"disk_gb"`
	ImageID        int64     `json:"image_id"`
	StorageTypeID  int64     `json:"storage_type_id"`
	ZoneID         int64     `json:"zone_id"`
	NodeID         int64     `json:"node_id"`
	PVEVmid        int64     `json:"pve_vmid,omitempty"`
	IP             string    `json:"ip,omitempty"`
	Status         string    `json:"status"`
	ProvisionError string    `json:"provision_error,omitempty"`
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

// vmListItem 是公开的透传 VM 负载（task 8.1/8.2）：7.x 的
// vmResponse 元数据加上从 PVE 读取的实时运行部分（设计 D1）。
// 规格大小（cpu/mem_mb/disk_gb）是创建时请求的本地 DB 值；
// 运行指标（cpu_usage、mem/maxmem、disk/maxdisk（字节）、uptime）
// 来自 PVE，当 VM 没有 PVE 实体（creating/failed）或处于停止状态时省略。
type vmListItem struct {
	vmResponse
	CPUUsage float64 `json:"cpu_usage,omitempty"`
	Mem      int64   `json:"mem,omitempty"`
	MaxMem   int64   `json:"maxmem,omitempty"`
	Disk     int64   `json:"disk,omitempty"`
	MaxDisk  int64   `json:"maxdisk,omitempty"`
	Uptime   int64   `json:"uptime,omitempty"`
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

// List 处理 GET /vms：对每个启用节点各发起一次 PVE 调用，并与
// 本地元数据分页合并（8.1）。分页由共享的 limit/offset 查询参数
// 选择，X-Total-Count 头报告本地 VM 行的总数。失败的节点在
// warnings 中报告（8.3）；warnings 始终是数组（所有节点都响应时为空）。
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

// Start 处理 POST /vms/:id/start。选择 202 + {accepted: true} 而非
// 返回 PVE 任务 ID：该操作是异步分发的，客户端没有任务轮询端点 ——
// VM 的真实状态通过透传读取（batch 8）。Location 头指向透传状态端点
// GET /vms/:id，客户端可在那里观察结果。
func (h *VMHandler) Start(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Start(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/vms/%d", id))
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
	return nil
}

// Stop 处理 POST /vms/:id/stop（干净的 ACPI 关机；见
// VMService.Stop）。Location 头指向透传状态端点 GET /vms/:id。
func (h *VMHandler) Stop(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Stop(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/vms/%d", id))
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
	return nil
}

// Restart 处理 POST /vms/:id/restart。Location 头指向透传状态
// 端点 GET /vms/:id。
func (h *VMHandler) Restart(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Restart(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/vms/%d", id))
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
	return nil
}

// Destroy 处理 DELETE /vms/:id。该操作是同步的（等待 PVE 销毁任务完成），
// 成功时返回无响应体的 204。DELETE 的幂等语义位于 service 层且保持不变：
// 不存在的 VM 行在任何 PVE 调用之前就返回 404 not_found；
// 而 PVE 侧的 404（VM 已在节点上被移除）被视为"已销毁"，
// 仅执行本地清理。
func (h *VMHandler) Destroy(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Destroy(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	c.Status(http.StatusNoContent)
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
