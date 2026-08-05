package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"spark/service"
)

// ImageHandler serves the /images routes.
type ImageHandler struct {
	svc *service.ImageService
}

// NewImageHandler creates an ImageHandler backed by svc.
func NewImageHandler(svc *service.ImageService) *ImageHandler {
	return &ImageHandler{svc: svc}
}

// RegisterImagesRoutes mounts the image registration and listing routes on
// rg. It is called by the router with the /images group.
func RegisterImagesRoutes(rg *gin.RouterGroup, svc *service.ImageService) {
	h := NewImageHandler(svc)
	rg.POST("", Handler(h.Create))
	rg.GET("", Handler(h.List))
}

// imageRequest is the request body for registering an image.
type imageRequest struct {
	Name        string            `json:"name"`
	DefaultUser string            `json:"default_user"`
	NodeImages  map[string]string `json:"node_images"`
}

// Create handles POST /images.
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

// List handles GET /images. With a zone_id query parameter it returns only
// the images available on every enabled node of that zone; without it, all
// images including their full node_images map. Both branches honor the
// shared limit/offset query parameters and set X-Total-Count to the total
// count of the branch's result set (all images, or the zone's available
// images).
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
