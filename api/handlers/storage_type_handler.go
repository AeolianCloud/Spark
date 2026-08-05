package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

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

// RegisterStorageTypesRoutes 在 rg 上挂载 storage type 的 CRUD 路由。
// 由 router 以 /storage-types 分组调用。
func RegisterStorageTypesRoutes(rg *gin.RouterGroup, svc *service.StorageTypeService) {
	h := NewStorageTypeHandler(svc)
	rg.POST("", Handler(h.Create))
	rg.GET("", Handler(h.List))
	rg.GET("/:id", Handler(h.Get))
	rg.PUT("/:id", Handler(h.Update))
	rg.DELETE("/:id", Handler(h.Delete))
}

// storageTypeRequest 是创建/更新 storage type 的请求体。
type storageTypeRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	PVEStorage  string `json:"pve_storage"`
}

// Create 处理 POST /storage-types。
func (h *StorageTypeHandler) Create(c *gin.Context) error {
	var req storageTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	st, err := h.svc.Create(c.Request.Context(), req.Name, req.DisplayName, req.PVEStorage)
	if err != nil {
		return mapServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/storage-types/%d", st.ID))
	c.JSON(http.StatusCreated, st)
	return nil
}

// Get 处理 GET /storage-types/:id。
func (h *StorageTypeHandler) Get(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	st, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		return mapServiceError(err)
	}
	c.JSON(http.StatusOK, st)
	return nil
}

// List 处理 GET /storage-types：一页 storage types（共享的
// limit/offset 查询参数），X-Total-Count 携带总数。
func (h *StorageTypeHandler) List(c *gin.Context) error {
	limit, offset, err := parsePagination(c)
	if err != nil {
		return err
	}
	types, total, err := h.svc.List(c.Request.Context(), limit, offset)
	if err != nil {
		return mapServiceError(err)
	}
	setTotalCount(c, total)
	c.JSON(http.StatusOK, types)
	return nil
}

// Update 处理 PUT /storage-types/:id。
func (h *StorageTypeHandler) Update(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	var req storageTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	st, err := h.svc.Update(c.Request.Context(), id, req.Name, req.DisplayName, req.PVEStorage)
	if err != nil {
		return mapServiceError(err)
	}
	c.JSON(http.StatusOK, st)
	return nil
}

// Delete 处理 DELETE /storage-types/:id。
func (h *StorageTypeHandler) Delete(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		return mapServiceError(err)
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
