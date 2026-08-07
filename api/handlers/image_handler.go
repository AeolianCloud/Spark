package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"spark/service"
)

// ImageHandler 提供 /images 路由。
type ImageHandler struct {
	svc *service.ImageService
}

// NewImageHandler 创建一个由 svc 支撑的 ImageHandler。
func NewImageHandler(svc *service.ImageService) *ImageHandler {
	return &ImageHandler{svc: svc}
}

// RegisterImagesRoutes 在 rg 上挂载镜像路由，由 router 以 /images 分组调用：
//
//   - POST /images：注册镜像（201 + Location 指向新资源）
//   - GET /images：镜像列表（可选 zone_id 过滤为区域可用镜像）
//   - GET /images/:id：镜像详情
//   - GET /images/:id/nodes-status：镜像在各启用节点上的存在状态
//   - POST /images/:id/download：受理镜像下载（202，异步执行）
//   - GET /images/:id/operations：镜像的下载操作历史（分页）
//
// :id 恒为数字本地行 id（images.id），非法值由 parseIDParam 拒绝为 400。
func RegisterImagesRoutes(rg *gin.RouterGroup, svc *service.ImageService) {
	h := NewImageHandler(svc)
	rg.POST("", Handler(h.Create))
	rg.GET("", Handler(h.List))
	rg.GET("/:id", Handler(h.Get))
	rg.GET("/:id/nodes-status", Handler(h.NodeStatus))
	rg.POST("/:id/download", Handler(h.Download))
	rg.GET("/:id/operations", Handler(h.Operations))
}

// imageRequest 是注册镜像的请求体。name/default_user/download_url 均必填，
// 校验位于 service 层（download_url 还必须是可解析的 http(s) URL，
// 由 PVE 节点代发下载，协议校验在服务端统一把关），handler 只负责透传。
type imageRequest struct {
	Name        string `json:"name"`
	DefaultUser string `json:"default_user"`
	DownloadURL string `json:"download_url"`
}

// Create 处理 POST /images：注册一个新镜像并返回 201 + 完整镜像负载。
// Location 头指向新资源 GET /images/:id（由 service 层保证
// download_url 等字段已校验）。
func (h *ImageHandler) Create(c *gin.Context) error {
	var req imageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	img, err := h.svc.Create(c.Request.Context(), req.Name, req.DefaultUser, req.DownloadURL)
	if err != nil {
		return mapServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/images/%d", img.ID))
	c.JSON(http.StatusCreated, img)
	return nil
}

// List 处理 GET /images。带 zone_id 查询参数时返回该区域可用的镜像
// （[]ImageZoneItem：镜像元数据 + 各启用节点上的存在状态数组，设计 D7）；
// 不带时返回全部已注册镜像的一页。两个分支都遵循共享的 limit/offset
// 分页参数，X-Total-Count 为该分支结果集的总数（区域过滤后或全量）。
func (h *ImageHandler) List(c *gin.Context) error {
	ctx := c.Request.Context()
	limit, offset, err := parsePagination(c)
	if err != nil {
		return err
	}
	if raw := c.Query("zone_id"); raw != "" {
		zoneID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || zoneID <= 0 {
			return ErrBadRequest("invalid zone_id query parameter")
		}
		images, total, err := h.svc.ListImagesByZone(ctx, zoneID, limit, offset)
		if err != nil {
			return mapServiceError(err)
		}
		setTotalCount(c, total)
		c.JSON(http.StatusOK, images)
		return nil
	}
	images, total, err := h.svc.List(ctx, limit, offset)
	if err != nil {
		return mapServiceError(err)
	}
	setTotalCount(c, total)
	c.JSON(http.StatusOK, images)
	return nil
}

// Get 处理 GET /images/:id：返回指定镜像的元数据（200 + 完整 Image
// 负载）。镜像不存在由 service 层映射 404 not_found；POST /images 的
// 201 Location 头即指向本端点。
func (h *ImageHandler) Get(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	img, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		return mapServiceError(err)
	}
	c.JSON(http.StatusOK, img)
	return nil
}

// NodeStatus 处理 GET /images/:id/nodes-status：返回该镜像在各启用节点上的
// 存在状态数组（[]NodeImageStatus，设计 D7）。可选 query 参数 zone_id 将
// 扫描限定在该区域的启用节点上（区域必须存在，否则 404）；不带时扫描全部
// 启用节点。单节点扫描失败降级为"未下载"（Downloaded=false），不会令整个
// 请求失败。
func (h *ImageHandler) NodeStatus(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	var zoneID *int64
	if raw := c.Query("zone_id"); raw != "" {
		zid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || zid <= 0 {
			return ErrBadRequest("invalid zone_id query parameter")
		}
		zoneID = &zid
	}
	statuses, err := h.svc.GetImageNodeStatus(c.Request.Context(), id, zoneID)
	if err != nil {
		return mapServiceError(err)
	}
	c.JSON(http.StatusOK, statuses)
	return nil
}

// imageDownloadRequest 是 POST /images/:id/download 的请求体：node_ids 与
// zone_id 二选一指定下载目标（两者同时提供由 service 层按 bad_request
// 拒绝；都不提供时同样由 service 层拒绝——无目标节点）。handler 只做
// JSON 解析与透传。
type imageDownloadRequest struct {
	NodeIDs []int64 `json:"node_ids"`
	ZoneID  *int64  `json:"zone_id"`
}

// Download 处理 POST /images/:id/download：受理镜像到目标节点（显式节点
// 列表或区域全部启用节点）的下载，返回 202 Accepted 与本次创建的
// []model.ImageOperation（每节点一条 running 记录）。下载在 service 层按
// 节点在后台 goroutine 中执行，终态（success/failed）异步写回操作记录。
// Location 头指向 GET /images/:id/operations，供前端轮询下载进度
// （202 受理 + Location 指向操作列表供轮询，design D6/D7）；202 语义
// 即"已受理、未完成"，客户端不应等待响应体即视为完成。
func (h *ImageHandler) Download(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	var req imageDownloadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	if req.ZoneID != nil && *req.ZoneID <= 0 {
		return ErrBadRequest("invalid zone_id")
	}
	ops, err := h.svc.Download(c.Request.Context(), id, req.NodeIDs, req.ZoneID)
	if err != nil {
		return mapServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/images/%d/operations", id))
	c.JSON(http.StatusAccepted, ops)
	return nil
}

// Operations 处理 GET /images/:id/operations：按时间倒序分页返回该镜像的
// 下载操作历史（[]model.ImageOperation），X-Total-Count 报告匹配总数
// （limit/offset 之前）。前端可用它轮询 Download 202 响应中 Location 头
// 指向的进度。
func (h *ImageHandler) Operations(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	limit, offset, err := parsePagination(c)
	if err != nil {
		return err
	}
	ops, total, err := h.svc.ListImageOperations(c.Request.Context(), id, limit, offset)
	if err != nil {
		return mapServiceError(err)
	}
	setTotalCount(c, total)
	c.JSON(http.StatusOK, ops)
	return nil
}
