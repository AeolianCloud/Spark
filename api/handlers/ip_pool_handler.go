package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"spark/service"
)

// IPPoolHandler 提供 /ip-pools 路由。
type IPPoolHandler struct {
	svc *service.IPPoolService
}

// NewIPPoolHandler 创建一个由 svc 支撑的 IPPoolHandler。
func NewIPPoolHandler(svc *service.IPPoolService) *IPPoolHandler {
	return &IPPoolHandler{svc: svc}
}

// RegisterIPPoolsRoutes 在 rg 上挂载 IP pool 路由。由
// router 以 /ip-pools 分组调用。
func RegisterIPPoolsRoutes(rg *gin.RouterGroup, svc *service.IPPoolService) {
	h := NewIPPoolHandler(svc)
	rg.POST("", Handler(h.CreatePool))
	rg.GET("", Handler(h.ListPools))
	rg.PUT("/:id/nodes", Handler(h.SetPoolNodes))
	rg.GET("/:id/nodes", Handler(h.GetPoolNodes))
}

// createPoolRequest 是 POST /ip-pools 的请求体。
type createPoolRequest struct {
	ZoneID      int64  `json:"zone_id"`
	Name        string `json:"name"`
	NetworkCIDR string `json:"network_cidr"`
	Gateway     string `json:"gateway"`
	DNS         string `json:"dns"`
}

// CreatePool 处理 POST /ip-pools：创建 pool 并将 CIDR 展开为
// 逐地址的 ip 行。
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

// ListPools 处理 GET /ip-pools。带 zone_id 查询参数时只返回该 zone 的
// pools —— 刻意不分页（一个 zone 内嵌的 pool 很少）；不带时返回全部
// pools，按共享的 limit/offset 查询参数分页。两个分支都通过
// X-Total-Count 携带总数。limit/offset 参数在两个分支都会校验，
// 使非法值无论是否带过滤条件都一致失败。
func (h *IPPoolHandler) ListPools(c *gin.Context) error {
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
		pools, total, err := h.svc.ListPools(ctx, &zoneID, limit, offset)
		if err != nil {
			return mapServiceErrorExtended(err)
		}
		setTotalCount(c, total)
		c.JSON(http.StatusOK, pools)
		return nil
	}
	pools, total, err := h.svc.ListPools(ctx, nil, limit, offset)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	setTotalCount(c, total)
	c.JSON(http.StatusOK, pools)
	return nil
}

// setPoolNodesRequest 是 PUT /ip-pools/:id/nodes 的请求体。
type setPoolNodesRequest struct {
	NodeIDs []int64 `json:"node_ids"`
}

// SetPoolNodes 处理 PUT /ip-pools/:id/nodes：完整替换 pool 的
// node 白名单。响应返回替换后的白名单。
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

// GetPoolNodes 处理 GET /ip-pools/:id/nodes。
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
