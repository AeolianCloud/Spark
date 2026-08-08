package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"spark/model"
	"spark/service"
)

// StorageTypeHandler 提供 /storage-types 路由。
type StorageTypeHandler struct {
	svc *service.StorageTypeService
}

// NewStorageTypeHandler 创建一个由 svc 支撑的 StorageTypeHandler。
func NewStorageTypeHandler(svc *service.StorageTypeService) *StorageTypeHandler {
	return &StorageTypeHandler{svc: svc}
}

// RegisterStorageTypesRoutes 在 rg 上挂载 storage type 的读路由、在
// adminRG 上挂载写路由。由 router 以两个 /storage-types 分组调用（B1
// 权限粒度拆分）：读操作仅挂 requireAuth——storage_type_id 是 user 创建
// VM 的必填字段，列表/详情是创建流程的必要输入（与 /images、/ip-pools
// 的粒度一致）；写操作挂 requireAdmin，user 令牌返回 403 forbidden。
// 手动登记（POST）已随扫描同步语义移除（提案 auto-scan-pve-storage），
// 存储类型只能由扫描产生：
//
//   - GET /storage-types：存储类型列表（可选 zone_id 过滤）[rg]
//   - GET /storage-types/:id：存储类型详情 [rg]
//   - POST /storage-types/scan：对指定 zone 手动触发一次存储扫描 [adminRG]
//   - PUT /storage-types/:id：更新 name（可置空）与 enabled [adminRG]
//   - DELETE /storage-types/:id：删除存储类型 [adminRG]
func RegisterStorageTypesRoutes(rg, adminRG *gin.RouterGroup, svc *service.StorageTypeService) {
	h := NewStorageTypeHandler(svc)
	rg.GET("", Handler(h.List))
	rg.GET("/:id", Handler(h.Get))
	adminRG.POST("/scan", Handler(h.Scan))
	adminRG.PUT("/:id", Handler(h.Update))
	adminRG.DELETE("/:id", Handler(h.Delete))
}

// storageTypeUpdateRequest 是更新 storage type 的请求体：仅允许修改
// name（业务名，可空；传空串表示置空为 NULL）与 enabled（启用开关），
// pve_storage 是扫描权威字段不可写。两个字段均可省略（省略表示不更新）。
type storageTypeUpdateRequest struct {
	Name    *string `json:"name"`
	Enabled *bool   `json:"enabled"`
}

// storageTypeCapabilities 是存储类型的能力派生布尔集合（提案
// auto-scan-pve-storage）：由扫描快照的 type/content 派生，覆盖 PVE
// 全部内容枚举（images/iso/backup/vztmpl/rootdir/snippets），外加
// can_download_image（仅 dir 类型存储可执行云镜像下载）。
type storageTypeCapabilities struct {
	CanStoreImages   bool `json:"can_store_images"`
	CanStoreISO      bool `json:"can_store_iso"`
	CanStoreBackup   bool `json:"can_store_backup"`
	CanStoreVZTmpl   bool `json:"can_store_vztmpl"`
	CanStoreRootdir  bool `json:"can_store_rootdir"`
	CanStoreSnippets bool `json:"can_store_snippets"`
	CanDownloadImage bool `json:"can_download_image"`
}

// storageTypeResponse 是公开的 storage type 负载：模型字段 + 能力派生
// 对象。name/type/content 为 null 时输出 null 而非省略（与模型一致）；
// nodes 始终输出数组（nil 归一为空数组 = 不限制节点，设计 D8）。
type storageTypeResponse struct {
	ID           int64                   `json:"id"`
	ZoneID       int64                   `json:"zone_id"`
	Name         *string                 `json:"name"`
	PVEStorage   string                  `json:"pve_storage"`
	Enabled      bool                    `json:"enabled"`
	Type         *string                 `json:"type"`
	Content      *string                 `json:"content"`
	Nodes        []string                `json:"nodes"`
	Capabilities storageTypeCapabilities `json:"capabilities"`
	CreatedAt    time.Time               `json:"created_at"`
}

// toStorageTypeResponse 由模型转换为公开负载，并按快照派生能力布尔。
// content 快照为逗号分隔字符串（如 "images,iso"），逐项拆分配置枚举；
// type 为 "dir" 时派生 can_download_image=true（本地路径类存储可代发
// 云镜像下载）。nodes 为 nil 时归一为空数组——契约保证空数组 = 不限制
// 节点、所有节点可用，null 与 [] 不得双形态。
func toStorageTypeResponse(st model.StorageType) storageTypeResponse {
	var caps storageTypeCapabilities
	if st.Content != nil {
		for _, item := range strings.Split(*st.Content, ",") {
			switch strings.TrimSpace(item) {
			case "images":
				caps.CanStoreImages = true
			case "iso":
				caps.CanStoreISO = true
			case "backup":
				caps.CanStoreBackup = true
			case "vztmpl":
				caps.CanStoreVZTmpl = true
			case "rootdir":
				caps.CanStoreRootdir = true
			case "snippets":
				caps.CanStoreSnippets = true
			}
		}
	}
	if st.Type != nil && *st.Type == "dir" {
		caps.CanDownloadImage = true
	}
	nodes := st.Nodes
	if nodes == nil {
		nodes = []string{}
	}
	return storageTypeResponse{
		ID: st.ID, ZoneID: st.ZoneID, Name: st.Name, PVEStorage: st.PVEStorage,
		Enabled: st.Enabled, Type: st.Type, Content: st.Content, Nodes: nodes,
		Capabilities: caps, CreatedAt: st.CreatedAt,
	}
}

// parseZoneIDQuery 解析可选的 zone_id 查询参数：空值返回 nil（不过滤），
// 非法值返回 400 bad_request。
func parseZoneIDQuery(c *gin.Context) (*int64, error) {
	raw := c.Query("zone_id")
	if raw == "" {
		return nil, nil
	}
	zoneID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || zoneID <= 0 {
		return nil, ErrBadRequest("invalid zone_id query parameter")
	}
	return &zoneID, nil
}

// Get 处理 GET /storage-types/:id。
func (h *StorageTypeHandler) Get(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	st, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	c.JSON(http.StatusOK, toStorageTypeResponse(*st))
	return nil
}

// List 处理 GET /storage-types：一页 storage types（共享的
// limit/offset 查询参数），X-Total-Count 携带总数。带 zone_id 查询
// 参数时仅返回该 zone 的存储类型（过滤后的总数）。
func (h *StorageTypeHandler) List(c *gin.Context) error {
	limit, offset, err := parsePagination(c)
	if err != nil {
		return err
	}
	zoneID, err := parseZoneIDQuery(c)
	if err != nil {
		return err
	}
	types, total, err := h.svc.List(c.Request.Context(), zoneID, limit, offset)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	out := make([]storageTypeResponse, 0, len(types))
	for _, st := range types {
		out = append(out, toStorageTypeResponse(st))
	}
	setTotalCount(c, total)
	c.JSON(http.StatusOK, out)
	return nil
}

// Scan 处理 POST /storage-types/scan：对指定 zone 手动触发一次存储
// 扫描（设计 D6），返回同步摘要 {created, updated, deleted, skipped}。
// zone_id 为必填查询参数；zone 内没有任何可达的启用节点时返回 503
// node_unavailable，且不产生部分同步。
func (h *StorageTypeHandler) Scan(c *gin.Context) error {
	zoneID, err := parseZoneIDQuery(c)
	if err != nil {
		return err
	}
	if zoneID == nil {
		return ErrBadRequest("zone_id query parameter is required")
	}
	summary, err := h.svc.SyncZone(c.Request.Context(), *zoneID)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	c.JSON(http.StatusOK, summary)
	return nil
}

// Update 处理 PUT /storage-types/:id：仅允许修改 name（业务名，可空、
// 传空串置空）与 enabled；pve_storage 是扫描权威字段不可写。
func (h *StorageTypeHandler) Update(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	var req storageTypeUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	st, err := h.svc.Update(c.Request.Context(), id, req.Name, req.Enabled)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	c.JSON(http.StatusOK, toStorageTypeResponse(*st))
	return nil
}

// Delete 处理 DELETE /storage-types/:id。
func (h *StorageTypeHandler) Delete(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		return mapServiceErrorExtended(err)
	}
	c.Status(http.StatusNoContent)
	return nil
}

// parseIDParam 解析正 int64 路径参数，否则返回 400
// bad_request 错误。
func parseIDParam(c *gin.Context, name string) (int64, error) {
	raw := c.Param(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrBadRequest("invalid " + name + " path parameter")
	}
	return id, nil
}
