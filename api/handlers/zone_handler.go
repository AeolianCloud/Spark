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

// Additional API error codes for the zone/node and IP pool domains, following
// the naming style of the base codes in error.go.
const (
	CodeNodeUnavailable = "node_unavailable"
	CodeIPExhausted     = "ip_exhausted"
)

// mapServiceErrorExtended maps service errors onto the unified API error
// contract. The shared kinds (bad_request/not_found/conflict) are delegated
// to mapServiceError; the zone/IP-pool domain kinds are mapped here.
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

// ZoneHandler serves the /zones and /nodes routes.
type ZoneHandler struct {
	svc *service.ZoneService
}

// NewZoneHandler creates a ZoneHandler backed by svc.
func NewZoneHandler(svc *service.ZoneService) *ZoneHandler {
	return &ZoneHandler{svc: svc}
}

// RegisterZonesRoutes mounts the zone and node routes. zonesGroup is the
// /zones group; nodesGroup the /nodes group (PUT /nodes/:id lives outside
// /zones). It is called by the router with both groups.
func RegisterZonesRoutes(zonesGroup, nodesGroup *gin.RouterGroup, svc *service.ZoneService) {
	h := NewZoneHandler(svc)
	zonesGroup.POST("", Handler(h.CreateZone))
	zonesGroup.GET("", Handler(h.ListZones))
	zonesGroup.POST("/:zone_id/nodes", Handler(h.CreateNode))
	zonesGroup.GET("/:zone_id/nodes", Handler(h.ListNodesByZone))
	nodesGroup.PUT("/:id", Handler(h.UpdateNode))
}

// zoneResponse is the public zone payload; nodes is never omitted so the
// shape is stable between create and list.
type zoneResponse struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	CreatedAt time.Time      `json:"created_at"`
	Nodes     []nodeResponse `json:"nodes"`
}

// nodeResponse is the public node payload. The API token is never included;
// api_token_set reports whether a token is stored (always true after create,
// and on update whether the token was replaced).
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

// CreateZone handles POST /zones.
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

// ListZones handles GET /zones.
func (h *ZoneHandler) ListZones(c *gin.Context) error {
	zones, err := h.svc.ListZones(c.Request.Context())
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	out := make([]zoneResponse, 0, len(zones))
	for _, z := range zones {
		out = append(out, toZoneResponse(z))
	}
	c.JSON(http.StatusOK, out)
	return nil
}

// nodeRequest is the create/update node body. api_token is optional on
// update (an empty value keeps the stored secret); enabled defaults to true.
type nodeRequest struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	APIUser  string `json:"api_user"`
	APIToken string `json:"api_token"`
	Enabled  *bool  `json:"enabled"`
}

// CreateNode handles POST /zones/:zone_id/nodes.
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

// ListNodesByZone handles GET /zones/:zone_id/nodes.
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

// UpdateNode handles PUT /nodes/:id.
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
