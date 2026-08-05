package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"spark/service"
)

// IPPoolHandler serves the /ip-pools routes.
type IPPoolHandler struct {
	svc *service.IPPoolService
}

// NewIPPoolHandler creates an IPPoolHandler backed by svc.
func NewIPPoolHandler(svc *service.IPPoolService) *IPPoolHandler {
	return &IPPoolHandler{svc: svc}
}

// RegisterIPPoolsRoutes mounts the IP pool routes on rg. It is called by the
// router with the /ip-pools group.
func RegisterIPPoolsRoutes(rg *gin.RouterGroup, svc *service.IPPoolService) {
	h := NewIPPoolHandler(svc)
	rg.POST("", Handler(h.CreatePool))
	rg.GET("", Handler(h.ListPools))
	rg.PUT("/:id/nodes", Handler(h.SetPoolNodes))
	rg.GET("/:id/nodes", Handler(h.GetPoolNodes))
}

// createPoolRequest is the request body for POST /ip-pools.
type createPoolRequest struct {
	ZoneID      int64  `json:"zone_id"`
	Name        string `json:"name"`
	NetworkCIDR string `json:"network_cidr"`
	Gateway     string `json:"gateway"`
	DNS         string `json:"dns"`
}

// CreatePool handles POST /ip-pools: creates the pool and expands the CIDR
// into per-address ip rows.
func (h *IPPoolHandler) CreatePool(c *gin.Context) error {
	var req createPoolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	if req.ZoneID <= 0 {
		return ErrBadRequest("zone_id is required")
	}
	pool, err := h.svc.CreateIPPool(c.Request.Context(), req.ZoneID, req.Name, req.NetworkCIDR, req.Gateway, req.DNS)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	c.Header("Location", fmt.Sprintf("/ip-pools/%d", pool.ID))
	c.JSON(http.StatusCreated, pool)
	return nil
}

// ListPools handles GET /ip-pools. With a zone_id query parameter it returns
// only that zone's pools; without it, all pools.
func (h *IPPoolHandler) ListPools(c *gin.Context) error {
	ctx := c.Request.Context()
	if raw := c.Query("zone_id"); raw != "" {
		zoneID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || zoneID <= 0 {
			return ErrBadRequest("invalid zone_id query parameter")
		}
		pools, err := h.svc.ListPools(ctx, &zoneID)
		if err != nil {
			return mapServiceErrorExtended(err)
		}
		c.JSON(http.StatusOK, pools)
		return nil
	}
	pools, err := h.svc.ListPools(ctx, nil)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	c.JSON(http.StatusOK, pools)
	return nil
}

// setPoolNodesRequest is the request body for PUT /ip-pools/:id/nodes.
type setPoolNodesRequest struct {
	NodeIDs []int64 `json:"node_ids"`
}

// SetPoolNodes handles PUT /ip-pools/:id/nodes: full replacement of the
// pool's node whitelist. The response returns the resulting whitelist.
func (h *IPPoolHandler) SetPoolNodes(c *gin.Context) error {
	poolID, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	var req setPoolNodesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	ctx := c.Request.Context()
	if err := h.svc.SetPoolNodes(ctx, poolID, req.NodeIDs); err != nil {
		return mapServiceErrorExtended(err)
	}
	nodes, err := h.svc.GetPoolNodes(ctx, poolID)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	out := make([]nodeResponse, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, toNodeResponse(n, true))
	}
	c.JSON(http.StatusOK, out)
	return nil
}

// GetPoolNodes handles GET /ip-pools/:id/nodes.
func (h *IPPoolHandler) GetPoolNodes(c *gin.Context) error {
	poolID, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	nodes, err := h.svc.GetPoolNodes(c.Request.Context(), poolID)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	out := make([]nodeResponse, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, toNodeResponse(n, true))
	}
	c.JSON(http.StatusOK, out)
	return nil
}
