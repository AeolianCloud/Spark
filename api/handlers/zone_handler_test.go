package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"spark/api/middleware"
	"spark/model"
	"spark/service"
)

// fakeHandlerZoneRepo 是 handler 测试的可脚本化 service.ZoneRepository。
type fakeHandlerZoneRepo struct {
	zones []model.Zone
	err   error
}

func (f *fakeHandlerZoneRepo) CreateZone(ctx context.Context, name string) (*model.Zone, error) {
	if f.err != nil {
		return nil, f.err
	}
	z := model.Zone{ID: int64(len(f.zones) + 1), Name: name, CreatedAt: time.Now()}
	f.zones = append(f.zones, z)
	return &f.zones[len(f.zones)-1], nil
}

func (f *fakeHandlerZoneRepo) GetZone(ctx context.Context, id int64) (*model.Zone, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.zones {
		if f.zones[i].ID == id {
			z := f.zones[i]
			return &z, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeHandlerZoneRepo) ListZones(ctx context.Context) ([]model.Zone, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.zones, nil
}

func (f *fakeHandlerZoneRepo) ListZonesPage(ctx context.Context, limit, offset int) ([]model.Zone, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]model.Zone, 0)
	for _, z := range f.zones {
		out = append(out, z)
	}
	if limit > 0 {
		if offset >= len(out) {
			return []model.Zone{}, nil
		}
		end := offset + limit
		if end > len(out) {
			end = len(out)
		}
		out = out[offset:end]
	}
	return out, nil
}

func (f *fakeHandlerZoneRepo) CountZones(ctx context.Context) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return len(f.zones), nil
}

// fakeHandlerNodeRepo 是 handler 测试的可脚本化 service.NodeRepository。
type fakeHandlerNodeRepo struct {
	nodes []model.PVENode
	err   error
}

func (f *fakeHandlerNodeRepo) CreateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	node.ID = int64(len(f.nodes) + 1)
	f.nodes = append(f.nodes, node)
	n := node
	return &n, nil
}

func (f *fakeHandlerNodeRepo) GetNode(ctx context.Context, id int64) (*model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.nodes {
		if f.nodes[i].ID == id {
			n := f.nodes[i]
			return &n, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeHandlerNodeRepo) ListNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]model.PVENode, 0)
	for _, n := range f.nodes {
		if n.ZoneID == zoneID {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeHandlerNodeRepo) ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]model.PVENode, 0)
	for _, n := range f.nodes {
		if n.ZoneID == zoneID && n.Enabled {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeHandlerNodeRepo) UpdateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.nodes {
		if f.nodes[i].ID == node.ID {
			f.nodes[i] = node
			n := f.nodes[i]
			return &n, nil
		}
	}
	return nil, pgx.ErrNoRows
}

// newZonesTestRouter 构建挂载 requireAuth + zone/node 路由的测试引擎：
// 读路由（GET /zones、GET /zones/:zone_id/nodes）只挂 requireAuth，写路由
//（POST /zones、POST /zones/:zone_id/nodes、PUT /nodes/:id）额外挂
// requireAdmin（H1 权限修复），与生产路由（api/router.go）的分层一致。
// 创建节点时的 PVE 集群名探测替换为脚本化函数（无网络依赖），使管理员
// 写路径可测。
func newZonesTestRouter(t *testing.T, cred *userCredentialRepo, zoneRepo *fakeHandlerZoneRepo, nodeRepo *fakeHandlerNodeRepo) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := service.NewZoneService(zoneRepo, nodeRepo)
	svc.SetProbeNodes(func(ctx context.Context, host string, port int, apiUser, apiTokenSecret string) ([]string, error) {
		// 脚本化探测：恒定失败（不可达），使节点登记/更新的节点名校验
		// 走 node_unavailable 分支——权限测试只关心中间件放行，不依赖
		// 真实网络。
		return nil, errors.New("unreachable")
	})
	zonesRG := r.Group("/zones", middleware.RequireAuth([]byte(testJWTSecret), cred))
	adminZonesRG := r.Group("/zones",
		middleware.RequireAuth([]byte(testJWTSecret), cred), middleware.RequireAdmin())
	adminNodesRG := r.Group("/nodes",
		middleware.RequireAuth([]byte(testJWTSecret), cred), middleware.RequireAdmin())
	RegisterZonesRoutes(zonesRG, adminZonesRG, adminNodesRG, svc)
	return r
}

// doZonesRequest 发起带/不带 Bearer 令牌的 JSON 请求（与 doUsersRequest /
// doStorageTypesRequest 同构）。
func doZonesRequest(t *testing.T, r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doUsersRequest(t, r, method, path, token, body)
}

// TestZonesRequireAdminToken 覆盖 H1 权限粒度拆分后的路由分层（security
// reviewer 反馈）：缺失令牌 401；读操作（GET /zones、GET
// /zones/:zone_id/nodes）对 user 令牌放行 200；写操作（POST /zones、
// POST /zones/:zone_id/nodes、PUT /nodes/:id）对 user 令牌一律 403——
// 阻断"user 注册伪造节点 → 自动扫描注入存储快照"攻击链；管理员令牌放行
// 读操作。
func TestZonesRequireAdminToken(t *testing.T) {
	cred := newHandlerStorageCred()
	zoneRepo := &fakeHandlerZoneRepo{zones: []model.Zone{{ID: 1, Name: "z1", CreatedAt: time.Now()}}}
	nodeRepo := &fakeHandlerNodeRepo{nodes: []model.PVENode{{
		ID: 1, ZoneID: 1, Name: "pve1", Host: "127.0.0.1", Port: 8006,
		APIUser: "root@pam!t", APITokenSecret: "s", Enabled: true,
	}}}
	r := newZonesTestRouter(t, cred, zoneRepo, nodeRepo)

	t.Run("missing token 401", func(t *testing.T) {
		w := doZonesRequest(t, r, http.MethodGet, "/zones", "", "")
		assertUsersAuthError(t, w, http.StatusUnauthorized, CodeUnauthorized)
	})
	t.Run("user token allowed on zone list", func(t *testing.T) {
		w := doZonesRequest(t, r, http.MethodGet, "/zones", userToken(t), "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("user token allowed on node list", func(t *testing.T) {
		w := doZonesRequest(t, r, http.MethodGet, "/zones/1/nodes", userToken(t), "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("user token forbidden on zone create", func(t *testing.T) {
		w := doZonesRequest(t, r, http.MethodPost, "/zones", userToken(t), `{"name":"hack-zone"}`)
		assertUsersAuthError(t, w, http.StatusForbidden, CodeForbidden)
	})
	t.Run("user token forbidden on node register", func(t *testing.T) {
		w := doZonesRequest(t, r, http.MethodPost, "/zones/1/nodes", userToken(t),
			`{"name":"fake","host":"attacker.example","api_user":"root@pam!x","api_token":"leak"}`)
		assertUsersAuthError(t, w, http.StatusForbidden, CodeForbidden)
	})
	t.Run("user token forbidden on node update", func(t *testing.T) {
		w := doZonesRequest(t, r, http.MethodPut, "/nodes/1", userToken(t), `{"name":"fake"}`)
		assertUsersAuthError(t, w, http.StatusForbidden, CodeForbidden)
	})
	t.Run("admin token allowed on zone list", func(t *testing.T) {
		w := doZonesRequest(t, r, http.MethodGet, "/zones", adminToken(t), "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("admin token allowed on zone create", func(t *testing.T) {
		w := doZonesRequest(t, r, http.MethodPost, "/zones", adminToken(t), `{"name":"ok-zone"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("admin token passes node update middleware", func(t *testing.T) {
		// 管理员令牌放行节点更新（中间件不拦）；探测用脚本化函数恒定失败，
		// 因此业务名变化时返回 503 node_unavailable，而非 403 forbidden——
		// 证明管理员写路径未被权限层阻断。
		w := doZonesRequest(t, r, http.MethodPut, "/nodes/1", adminToken(t),
			`{"name":"renamed","host":"127.0.0.1","api_user":"root@pam!t"}`)
		assertHandlerError(t, w, http.StatusServiceUnavailable, CodeNodeUnavailable)
	})
}
