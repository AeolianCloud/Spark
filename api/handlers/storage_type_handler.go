package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"spark/service"
)

// StorageTypeHandler serves the /storage-types routes.
type StorageTypeHandler struct {
	svc *service.StorageTypeService
}

// NewStorageTypeHandler creates a StorageTypeHandler backed by svc.
func NewStorageTypeHandler(svc *service.StorageTypeService) *StorageTypeHandler {
	return &StorageTypeHandler{svc: svc}
}

// RegisterStorageTypesRoutes mounts the storage type CRUD routes on rg. It is
// called by the router with the /storage-types group.
func RegisterStorageTypesRoutes(rg *gin.RouterGroup, svc *service.StorageTypeService) {
	h := NewStorageTypeHandler(svc)
	rg.POST("", Handler(h.Create))
	rg.GET("", Handler(h.List))
	rg.GET("/:id", Handler(h.Get))
	rg.PUT("/:id", Handler(h.Update))
	rg.DELETE("/:id", Handler(h.Delete))
}

// storageTypeRequest is the request body for create/update storage types.
type storageTypeRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	PVEStorage  string `json:"pve_storage"`
}

// Create handles POST /storage-types.
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

// Get handles GET /storage-types/:id.
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

// List handles GET /storage-types: one page of storage types (shared
// limit/offset query parameters), with X-Total-Count carrying the total
// count.
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

// Update handles PUT /storage-types/:id.
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

// Delete handles DELETE /storage-types/:id.
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

// parseIDParam parses a positive int64 path parameter, answering a 400
// bad_request error otherwise.
func parseIDParam(c *gin.Context, name string) (int64, error) {
	raw := c.Param(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrBadRequest("invalid " + name + " path parameter")
	}
	return id, nil
}
