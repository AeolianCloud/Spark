package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"spark/model"
	"spark/service"
)

// zone/node 与 IP pool 域补充的 API 错误码，沿用 error.go 中基础错误码的命名风格。
const (
	CodeNodeUnavailable = "node_unavailable"
	CodeIPExhausted     = "ip_exhausted"
)

// mapServiceErrorExtended 将 service 层错误映射为统一的 API 错误契约。
// 共享的错误类型（bad_request/not_found/conflict）委托给
// mapServiceError；zone/IP-pool 域的错误类型在这里映射。
func mapServiceErrorExtended(err error) error {
	var serr *service.Error
	if !errors.As(err, &serr) {
		return mapServiceError(err)
	}
	switch serr.Kind {
	case service.KindNodeUnavailable:
		return NewError(http.StatusServiceUnavailable, CodeNodeUnavailable, serr.Message)
	case service.KindIPExhausted:
		return NewError(http.StatusConflict, CodeIPExhausted, serr.Message)
	default:
		return mapServiceError(err)
	}
}

// ZoneHandler 提供 /zones 和 /nodes 路由。
type ZoneHandler struct {
	svc *service.ZoneService
}

// NewZoneHandler 创建一个由 svc 支撑的 ZoneHandler。
func NewZoneHandler(svc *service.ZoneService) *ZoneHandler {
	return &ZoneHandler{svc: svc}
}

// RegisterZonesRoutes 挂载 zone 和 node 路由。zonesGroup 是 /zones 分组；
// nodesGroup 是 /nodes 分组（PUT /nodes/:id 位于 /zones 之外）。
// 由 router 同时传入两个分组调用。
func RegisterZonesRoutes(zonesGroup, nodesGroup *gin.RouterGroup, svc *service.ZoneService) {
	h := NewZoneHandler(svc)
	zonesGroup.POST("", Handler(h.CreateZone))
	zonesGroup.GET("", Handler(h.ListZones))
	zonesGroup.POST("/:zone_id/nodes", Handler(h.CreateNode))
	zonesGroup.GET("/:zone_id/nodes", Handler(h.ListNodesByZone))
	nodesGroup.PUT("/:id", Handler(h.UpdateNode))
}

// zoneResponse 是公开的 zone 负载；nodes 永不省略，使 create 与 list 之间的结构保持稳定。
type zoneResponse struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	CreatedAt time.Time      `json:"created_at"`
	Nodes     []nodeResponse `json:"nodes"`
}

// nodeResponse 是公开的 node 负载。API token 永不包含在响应中；
// api_token_set 报告是否存有 token（create 之后恒为 true，
// update 时则表示 token 是否被替换）。
type nodeResponse struct {
	ID          int64     `json:"id"`
	ZoneID      int64     `json:"zone_id"`
	Name        string    `json:"name"`
	Host        string    `json:"host"`
	APIUser     string    `json:"api_user"`
	APITokenSet bool      `json:"api_token_set"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

func toZoneResponse(z service.ZoneWithNodes) zoneResponse {
	nodes := make([]nodeResponse, 0, len(z.Nodes))
	for _, n := range z.Nodes {
		nodes = append(nodes, toNodeResponse(n, true))
	}
	return zoneResponse{ID: z.Zone.ID, Name: z.Zone.Name, CreatedAt: z.Zone.CreatedAt, Nodes: nodes}
}

func toNodeResponse(n model.PVENode, apiTokenSet bool) nodeResponse {
	return nodeResponse{
		ID: n.ID, ZoneID: n.ZoneID, Name: n.Name, Host: n.Host,
		APIUser: n.APIUser, APITokenSet: apiTokenSet, Enabled: n.Enabled,
		CreatedAt: n.CreatedAt,
	}
}

// CreateZone 处理 POST /zones。
func (h *ZoneHandler) CreateZone(c *gin.Context) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	zone, err := h.svc.CreateZone(c.Request.Context(), req.Name)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	c.Header("Location", fmt.Sprintf("/zones/%d", zone.ID))
	c.JSON(http.StatusCreated, zoneResponse{
		ID: zone.ID, Name: zone.Name, CreatedAt: zone.CreatedAt, Nodes: []nodeResponse{},
	})
	return nil
}

// ListZones 处理 GET /zones：一页 zones（共享的 limit/offset
// 查询参数），每个 zone 携带完整的 node 列表；X-Total-Count 携带
// zone 的总数。
func (h *ZoneHandler) ListZones(c *gin.Context) error {
	limit, offset, err := parsePagination(c)
	if err != nil {
		return err
	}
	zones, total, err := h.svc.ListZones(c.Request.Context(), limit, offset)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	out := make([]zoneResponse, 0, len(zones))
	for _, z := range zones {
		out = append(out, toZoneResponse(z))
	}
	setTotalCount(c, total)
	c.JSON(http.StatusOK, out)
	return nil
}

// nodeRequest 是创建/更新 node 的请求体。api_token 在更新时可选
// （空值表示保留已存储的密钥）；enabled 默认为 true。
type nodeRequest struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	APIUser  string `json:"api_user"`
	APIToken string `json:"api_token"`
	Enabled  *bool  `json:"enabled"`
}

// CreateNode 处理 POST /zones/:zone_id/nodes。
func (h *ZoneHandler) CreateNode(c *gin.Context) error {
	zoneID, err := parseIDParam(c, "zone_id")
	if err != nil {
		return err
	}
	var req nodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	node, err := h.svc.CreateNode(c.Request.Context(), zoneID, req.Name, req.Host, req.APIUser, req.APIToken, req.Enabled)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	c.Header("Location", fmt.Sprintf("/zones/%d/nodes/%d", zoneID, node.ID))
	c.JSON(http.StatusCreated, toNodeResponse(*node, true))
	return nil
}

// ListNodesByZone 处理 GET /zones/:zone_id/nodes。
func (h *ZoneHandler) ListNodesByZone(c *gin.Context) error {
	zoneID, err := parseIDParam(c, "zone_id")
	if err != nil {
		return err
	}
	nodes, err := h.svc.ListNodesByZone(c.Request.Context(), zoneID)
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

// UpdateNode 处理 PUT /nodes/:id。
func (h *ZoneHandler) UpdateNode(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	var req nodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	node, tokenChanged, err := h.svc.UpdateNode(c.Request.Context(), id, req.Name, req.Host, req.APIUser, req.APIToken, req.Enabled)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	c.JSON(http.StatusOK, toNodeResponse(*node, tokenChanged))
	return nil
}
