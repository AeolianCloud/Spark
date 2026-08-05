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

// RegisterImagesRoutes 在 rg 上挂载镜像注册与列表路由。
// 由 router 以 /images 分组调用。
func RegisterImagesRoutes(rg *gin.RouterGroup, svc *service.ImageService) {
	h := NewImageHandler(svc)
	rg.POST("", Handler(h.Create))
	rg.GET("", Handler(h.List))
}

// imageRequest 是注册镜像的请求体。
type imageRequest struct {
	Name        string            `json:"name"`
	DefaultUser string            `json:"default_user"`
	NodeImages  map[string]string `json:"node_images"`
}

// Create 处理 POST /images。
func (h *ImageHandler) Create(c *gin.Context) error {
	var req imageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	img, err := h.svc.Create(c.Request.Context(), req.Name, req.DefaultUser, req.NodeImages)
	if err != nil {
		return mapServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/images/%d", img.ID))
	c.JSON(http.StatusCreated, img)
	return nil
}

// List 处理 GET /images。带 zone_id 查询参数时只返回该 zone 所有
// 启用节点上都可用的镜像；不带时返回全部镜像及其完整的 node_images 映射。
// 两个分支都遵循共享的 limit/offset 查询参数，并将 X-Total-Count 设为
// 该分支结果集的总数（全部镜像，或该 zone 可用的镜像）。
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
