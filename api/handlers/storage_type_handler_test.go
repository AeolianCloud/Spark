package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"spark/api/middleware"
	"spark/model"
	"spark/pve"
	"spark/service"
)

// handlerStorageTypeRow 构造一行带完整快照字段的存储类型（ZoneID 默认 1）。
func handlerStorageTypeRow(id int64, name *string, pveStorage string, enabled bool, stype, content string) model.StorageType {
	return model.StorageType{
		ID: id, ZoneID: 1, Name: name, PVEStorage: pveStorage,
		Enabled: enabled, Type: &stype, Content: &content, CreatedAt: time.Now(),
	}
}

// handlerStorageTypePtr 返回指向 s 的指针，供构造可空字段（name）用。
func handlerStorageTypePtr(s string) *string { return &s }

// fakeHandlerStorageTypeRepo 是 handler 测试的可脚本化
// service.StorageTypeRepository（内存行 + 最小语义）。
type fakeHandlerStorageTypeRepo struct {
	rows   []model.StorageType
	nextID int64
}

func (f *fakeHandlerStorageTypeRepo) UpsertByZonePveStorage(ctx context.Context, zoneID int64, pveStorage, stype, content string, nodes []string) (*model.StorageType, bool, error) {
	// 与真实 repo 的落库-读回一致：nil 一律归一为空切片（空 = 不限制节点）。
	if nodes == nil {
		nodes = []string{}
	}
	for i := range f.rows {
		if f.rows[i].ZoneID == zoneID && f.rows[i].PVEStorage == pveStorage {
			t, c := stype, content
			f.rows[i].Type, f.rows[i].Content = &t, &c
			f.rows[i].Nodes = nodes
			return &f.rows[i], false, nil
		}
	}
	row := model.StorageType{
		ID: f.nextID, ZoneID: zoneID, Name: nil, PVEStorage: pveStorage,
		Enabled: true, Type: &stype, Content: &content, Nodes: nodes, CreatedAt: time.Now(),
	}
	f.nextID++
	f.rows = append(f.rows, row)
	return &f.rows[len(f.rows)-1], true, nil
}

func (f *fakeHandlerStorageTypeRepo) UpdateMeta(ctx context.Context, id int64, name *string, enabled *bool) (*model.StorageType, error) {
	for i := range f.rows {
		if f.rows[i].ID == id {
			if name != nil {
				if *name == "" {
					f.rows[i].Name = nil
				} else {
					n := *name
					f.rows[i].Name = &n
				}
			}
			if enabled != nil {
				f.rows[i].Enabled = *enabled
			}
			return &f.rows[i], nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeHandlerStorageTypeRepo) ListPage(ctx context.Context, zoneID *int64, limit, offset int) ([]model.StorageType, error) {
	out := make([]model.StorageType, 0)
	for _, r := range f.rows {
		if zoneID == nil || r.ZoneID == *zoneID {
			out = append(out, r)
		}
	}
	if limit > 0 {
		if offset >= len(out) {
			return []model.StorageType{}, nil
		}
		end := offset + limit
		if end > len(out) {
			end = len(out)
		}
		out = out[offset:end]
	}
	return out, nil
}

func (f *fakeHandlerStorageTypeRepo) Count(ctx context.Context, zoneID *int64) (int, error) {
	n := 0
	for _, r := range f.rows {
		if zoneID == nil || r.ZoneID == *zoneID {
			n++
		}
	}
	return n, nil
}

func (f *fakeHandlerStorageTypeRepo) Get(ctx context.Context, id int64) (*model.StorageType, error) {
	for i := range f.rows {
		if f.rows[i].ID == id {
			st := f.rows[i]
			return &st, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeHandlerStorageTypeRepo) Delete(ctx context.Context, id int64) error {
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return pgx.ErrNoRows
}

// fakeHandlerStorageNodeRepo 是 handler 测试的可脚本化
// service.StorageTypeNodeRepository。
type fakeHandlerStorageNodeRepo struct {
	nodes []model.PVENode
}

func (f *fakeHandlerStorageNodeRepo) ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	out := make([]model.PVENode, 0)
	for _, n := range f.nodes {
		if n.ZoneID == zoneID && n.Enabled {
			out = append(out, n)
		}
	}
	return out, nil
}

// fakeHandlerStorageZoneRepo 是 handler 测试的可脚本化
// service.StorageTypeZoneRepository。
type fakeHandlerStorageZoneRepo struct {
	zones []model.Zone
}

func (f *fakeHandlerStorageZoneRepo) GetZone(ctx context.Context, id int64) (*model.Zone, error) {
	for i := range f.zones {
		if f.zones[i].ID == id {
			z := f.zones[i]
			return &z, nil
		}
	}
	return nil, pgx.ErrNoRows
}

// handlerStoragePVEServer 是 handler 测试的假 PVE 服务器：应答可达性探测
// （GET /version）与集群存储清单（GET /storage）。storages 为 nil 时
// /storage 返回 500（模拟 PVE 上游错误）。
func handlerStoragePVEServer(t *testing.T, storages []pve.PVEStorage) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 可达性探测（SelectReachableNode 的 Ping）走默认 base URL
		// （https://host:port/api2/json），扫描的 ListStorage 走注入的
		// SetClientFactory base URL（https://host:port）——两种前缀都兼容。
		switch {
		case strings.HasSuffix(r.URL.Path, "/version"):
			fmt.Fprint(w, `{"data": {"release": "9.1", "repoid": "e2e", "version": "9.1.1"}}`)
		case strings.HasSuffix(r.URL.Path, "/storage"):
			w.Header().Set("Content-Type", "application/json")
			if storages == nil {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"errors": {"storage": "internal failure"}}`)
				return
			}
			var sb strings.Builder
			sb.WriteString(`{"data": [`)
			for i, s := range storages {
				if i > 0 {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, `{"storage": %q, "type": %q, "content": %q, "shared": %v, "nodes": %q}`,
					s.Storage, s.Type, s.Content, s.Shared, strings.Join(s.Nodes, ","))
			}
			sb.WriteString(`]}`)
			fmt.Fprint(w, sb.String())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// handlerStorageProbeNode 构造指向 pveSrv 的启用节点：host/port 命中假 PVE
// 服务器，使默认 SelectReachableNode 的可达性探测（Ping -> GET /version）
// 真实成功；由需要"扫描成功"路径的测试自行注入 nodeRepo。
func handlerStorageProbeNode(pveSrv *httptest.Server) model.PVENode {
	port := pveSrv.Listener.Addr().(*net.TCPAddr).Port
	return model.PVENode{ID: 1, ZoneID: 1, Name: "pve1", Host: "127.0.0.1", Port: port,
		APIUser: "root@pam!probe", APITokenSecret: "secret123", Enabled: true}
}

// newStorageTypeHandlerTestService 装配 handler 测试的 StorageTypeService：
// 节点选择器用默认 SelectReachableNode（对假 PVE 服务器做真实探测），PVE
// 客户端工厂指向 pveSrv（SetClientFactory 覆盖扫描的 ListStorage 调用）。
// pveSrv 为 nil 时不注入客户端工厂（扫描路径未覆盖的测试用）。
func newStorageTypeHandlerTestService(t *testing.T, repo *fakeHandlerStorageTypeRepo, nodeRepo *fakeHandlerStorageNodeRepo, zoneRepo *fakeHandlerStorageZoneRepo, pveSrv *httptest.Server) *service.StorageTypeService {
	t.Helper()
	svc := service.NewStorageTypeService(repo, nodeRepo, zoneRepo)
	if pveSrv != nil {
		svc.SetClientFactory(func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient(host, apiUser, apiTokenSecret,
				pve.WithPort(port), pve.WithHTTPClient(pveSrv.Client()), pve.WithTimeout(5*time.Second))
		})
	}
	return svc
}

// newStorageTypeTestRouter 构建挂载 requireAuth + storage-type 路由的
// 测试引擎：读路由只挂 requireAuth、写路由额外挂 requireAdmin（B1 权限
// 粒度拆分），与生产路由（api/router.go）的分层一致。
func newStorageTypeTestRouter(t *testing.T, cred *userCredentialRepo, repo *fakeHandlerStorageTypeRepo, nodeRepo *fakeHandlerStorageNodeRepo, zoneRepo *fakeHandlerStorageZoneRepo, pveSrv *httptest.Server) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := newStorageTypeHandlerTestService(t, repo, nodeRepo, zoneRepo, pveSrv)
	rg := r.Group("/storage-types", middleware.RequireAuth([]byte(testJWTSecret), cred))
	adminRG := r.Group("/storage-types",
		middleware.RequireAuth([]byte(testJWTSecret), cred), middleware.RequireAdmin())
	RegisterStorageTypesRoutes(rg, adminRG, svc)
	return r
}

// doStorageTypesRequest 发起带/不带 Bearer 令牌的 JSON 请求。
func doStorageTypesRequest(t *testing.T, r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r.ServeHTTP(w, req)
	return w
}

// newHandlerStorageCred 是带管理员 1 与启用用户 2 的标准凭据仓库。
func newHandlerStorageCred() *userCredentialRepo {
	return &userCredentialRepo{
		admins: []model.Admin{{ID: 1}},
		users:  []model.User{{ID: 2, Status: model.UserStatusEnabled}},
	}
}

// TestStorageTypesRequireAdminToken 覆盖 B1 权限粒度拆分后的路由分层：
// 缺失令牌 401；读操作（GET 列表/详情）对 user 令牌放行 200（创建 VM 的
// 必填输入，与 /images 粒度一致）；写操作（扫描/PUT/DELETE）对 user
// 令牌一律 403；管理员令牌放行读操作。
func TestStorageTypesRequireAdminToken(t *testing.T) {
	cred := newHandlerStorageCred()
	repo := &fakeHandlerStorageTypeRepo{rows: []model.StorageType{handlerStorageTypeRow(1, nil, "local", true, "dir", "images")}}
	r := newStorageTypeTestRouter(t, cred, repo, &fakeHandlerStorageNodeRepo{}, &fakeHandlerStorageZoneRepo{}, nil)

	t.Run("missing token 401", func(t *testing.T) {
		w := doStorageTypesRequest(t, r, http.MethodGet, "/storage-types", "", "")
		assertUsersAuthError(t, w, http.StatusUnauthorized, CodeUnauthorized)
	})
	t.Run("user token allowed on list", func(t *testing.T) {
		w := doStorageTypesRequest(t, r, http.MethodGet, "/storage-types", userToken(t), "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("user token allowed on detail", func(t *testing.T) {
		w := doStorageTypesRequest(t, r, http.MethodGet, "/storage-types/1", userToken(t), "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("user token forbidden on scan", func(t *testing.T) {
		w := doStorageTypesRequest(t, r, http.MethodPost, "/storage-types/scan?zone_id=1", userToken(t), "")
		assertUsersAuthError(t, w, http.StatusForbidden, CodeForbidden)
	})
	t.Run("user token forbidden on update", func(t *testing.T) {
		w := doStorageTypesRequest(t, r, http.MethodPut, "/storage-types/1", userToken(t), `{"name":"hack"}`)
		assertUsersAuthError(t, w, http.StatusForbidden, CodeForbidden)
	})
	t.Run("user token forbidden on delete", func(t *testing.T) {
		w := doStorageTypesRequest(t, r, http.MethodDelete, "/storage-types/1", userToken(t), "")
		assertUsersAuthError(t, w, http.StatusForbidden, CodeForbidden)
	})
	t.Run("admin token allowed on list", func(t *testing.T) {
		w := doStorageTypesRequest(t, r, http.MethodGet, "/storage-types", adminToken(t), "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
	})
}

// TestStorageTypeCapabilitiesDerivation 覆盖 capabilities 派生的纯函数语义
// （提案 auto-scan-pve-storage）：content 快照按逗号拆分、容忍空白；NULL 与
// 空串不派生任何能力；can_download_image 仅 type=dir 为 true。
func TestStorageTypeCapabilitiesDerivation(t *testing.T) {
	cases := []struct {
		name    string
		content *string
		stype   *string
		want    storageTypeCapabilities
	}{
		{
			name: "images,iso", content: handlerStorageTypePtr("images,iso"), stype: handlerStorageTypePtr("dir"),
			want: storageTypeCapabilities{CanStoreImages: true, CanStoreISO: true, CanDownloadImage: true},
		},
		{
			name: "whitespace tolerated", content: handlerStorageTypePtr(" images, iso "), stype: handlerStorageTypePtr("dir"),
			want: storageTypeCapabilities{CanStoreImages: true, CanStoreISO: true, CanDownloadImage: true},
		},
		{
			name: "full enum", content: handlerStorageTypePtr("images,iso,backup,vztmpl,rootdir,snippets"), stype: handlerStorageTypePtr("nfs"),
			want: storageTypeCapabilities{
				CanStoreImages: true, CanStoreISO: true, CanStoreBackup: true,
				CanStoreVZTmpl: true, CanStoreRootdir: true, CanStoreSnippets: true,
			},
		},
		{
			name: "null content", content: nil, stype: handlerStorageTypePtr("dir"),
			want: storageTypeCapabilities{CanDownloadImage: true},
		},
		{
			name: "empty content", content: handlerStorageTypePtr(""), stype: handlerStorageTypePtr("dir"),
			want: storageTypeCapabilities{CanDownloadImage: true},
		},
		{
			name: "lvm cannot download image", content: handlerStorageTypePtr("images,rootdir"), stype: handlerStorageTypePtr("lvm"),
			want: storageTypeCapabilities{CanStoreImages: true, CanStoreRootdir: true},
		},
		{
			name: "dir can download image", content: handlerStorageTypePtr("iso"), stype: handlerStorageTypePtr("dir"),
			want: storageTypeCapabilities{CanStoreISO: true, CanDownloadImage: true},
		},
		{
			name: "null type cannot download image", content: handlerStorageTypePtr("images"), stype: nil,
			want: storageTypeCapabilities{CanStoreImages: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toStorageTypeResponse(handlerStorageTypeRow(1, nil, "local", true, derefOr(tc.stype, ""), derefOr(tc.content, ""))).Capabilities
			if got != tc.want {
				t.Fatalf("capabilities = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestStorageTypeNodesInResponse 覆盖响应中节点挂载快照（nodes，设计 D8）
// 的序列化契约：非空挂载列表原样透传；nil 归一为空数组（JSON []，绝不
// 输出 null）——契约保证空数组 = 不限制节点、所有节点可用，null 与 []
// 不得双形态。
func TestStorageTypeNodesInResponse(t *testing.T) {
	// 非空挂载列表：split 后的节点名数组原样输出。
	st := handlerStorageTypeRow(1, nil, "local", true, "dir", "images")
	st.Nodes = []string{"pve1", "pve2"}
	resp := toStorageTypeResponse(st)
	if len(resp.Nodes) != 2 || resp.Nodes[0] != "pve1" || resp.Nodes[1] != "pve2" {
		t.Fatalf("nodes = %v, want [pve1 pve2]", resp.Nodes)
	}

	// nil 归一为空数组（JSON []）。
	st2 := handlerStorageTypeRow(2, nil, "unlimited", true, "dir", "images")
	resp2 := toStorageTypeResponse(st2)
	if resp2.Nodes == nil {
		t.Fatal("nodes = nil, want non-nil empty slice")
	}
	if len(resp2.Nodes) != 0 {
		t.Fatalf("nodes = %v, want empty", resp2.Nodes)
	}
	raw, err := json.Marshal(resp2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	nodes, ok := m["nodes"].([]any)
	if !ok || len(nodes) != 0 {
		t.Fatalf("json nodes = %v (%T), want []", m["nodes"], m["nodes"])
	}
}

// derefOr 返回指针解引用值，nil 时返回 fallback。
func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

// TestStorageTypeScanHandler 覆盖 POST /storage-types/scan 的端点语义：
// 缺 zone_id 400、成功 200 返回摘要、zone 不存在 404、zone 无节点 503
// node_unavailable。
func TestStorageTypeScanHandler(t *testing.T) {
	cred := newHandlerStorageCred()

	t.Run("missing zone_id returns 400", func(t *testing.T) {
		r := newStorageTypeTestRouter(t, cred, &fakeHandlerStorageTypeRepo{}, &fakeHandlerStorageNodeRepo{}, &fakeHandlerStorageZoneRepo{zones: []model.Zone{{ID: 1, Name: "z1"}}}, nil)
		w := doStorageTypesRequest(t, r, http.MethodPost, "/storage-types/scan", adminToken(t), "")
		assertHandlerError(t, w, http.StatusBadRequest, CodeBadRequest)
	})

	t.Run("invalid zone_id returns 400", func(t *testing.T) {
		r := newStorageTypeTestRouter(t, cred, &fakeHandlerStorageTypeRepo{}, &fakeHandlerStorageNodeRepo{}, &fakeHandlerStorageZoneRepo{zones: []model.Zone{{ID: 1, Name: "z1"}}}, nil)
		w := doStorageTypesRequest(t, r, http.MethodPost, "/storage-types/scan?zone_id=abc", adminToken(t), "")
		assertHandlerError(t, w, http.StatusBadRequest, CodeBadRequest)
	})

	t.Run("zone not found returns 404", func(t *testing.T) {
		pveSrv := handlerStoragePVEServer(t, []pve.PVEStorage{{Storage: "local", Type: "dir", Content: "images"}})
		r := newStorageTypeTestRouter(t, cred, &fakeHandlerStorageTypeRepo{nextID: 10}, &fakeHandlerStorageNodeRepo{}, &fakeHandlerStorageZoneRepo{}, pveSrv)
		w := doStorageTypesRequest(t, r, http.MethodPost, "/storage-types/scan?zone_id=99", adminToken(t), "")
		assertHandlerError(t, w, http.StatusNotFound, CodeNotFound)
	})

	t.Run("zone without nodes returns 503 node_unavailable", func(t *testing.T) {
		// nodeRepo 无节点：默认 selectNode 空候选直接失败，无网络依赖。
		pveSrv := handlerStoragePVEServer(t, []pve.PVEStorage{{Storage: "local", Type: "dir", Content: "images"}})
		r := newStorageTypeTestRouter(t, cred, &fakeHandlerStorageTypeRepo{nextID: 10}, &fakeHandlerStorageNodeRepo{}, &fakeHandlerStorageZoneRepo{zones: []model.Zone{{ID: 1, Name: "z1"}}}, pveSrv)
		w := doStorageTypesRequest(t, r, http.MethodPost, "/storage-types/scan?zone_id=1", adminToken(t), "")
		assertHandlerError(t, w, http.StatusServiceUnavailable, CodeNodeUnavailable)
	})

	t.Run("success returns scan summary", func(t *testing.T) {
		pveSrv := handlerStoragePVEServer(t, []pve.PVEStorage{
			{Storage: "local", Type: "dir", Content: "images,iso", Shared: true, Nodes: []string{"pve1"}},
			{Storage: "local-lvm", Type: "lvm", Content: "images,rootdir", Shared: true, Nodes: []string{"pve1"}},
		})
		nodeRepo := &fakeHandlerStorageNodeRepo{nodes: []model.PVENode{handlerStorageProbeNode(pveSrv)}}
		r := newStorageTypeTestRouter(t, cred, &fakeHandlerStorageTypeRepo{nextID: 10}, nodeRepo, &fakeHandlerStorageZoneRepo{zones: []model.Zone{{ID: 1, Name: "z1"}}}, pveSrv)
		w := doStorageTypesRequest(t, r, http.MethodPost, "/storage-types/scan?zone_id=1", adminToken(t), "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["created"] != float64(2) || body["updated"] != float64(0) ||
			body["deleted"] != float64(0) || body["skipped"] != float64(0) {
			t.Fatalf("summary = %+v, want created=2 rest=0", body)
		}
	})
}

// TestStorageTypeUpdateHandler 覆盖 PUT /storage-types/:id 的端点语义：
// 空串 name 置空（NULL）、超长 name 400、未知 id 404。
func TestStorageTypeUpdateHandler(t *testing.T) {
	cred := newHandlerStorageCred()

	t.Run("empty name clears to null", func(t *testing.T) {
		name := "ssd"
		repo := &fakeHandlerStorageTypeRepo{nextID: 10, rows: []model.StorageType{
			handlerStorageTypeRow(1, &name, "local-ssd", true, "dir", "images"),
		}}
		r := newStorageTypeTestRouter(t, cred, repo, &fakeHandlerStorageNodeRepo{}, &fakeHandlerStorageZoneRepo{}, nil)
		w := doStorageTypesRequest(t, r, http.MethodPut, "/storage-types/1", adminToken(t), `{"name": ""}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		var body struct {
			Name *string `json:"name"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Name != nil {
			t.Fatalf("name = %v, want null after clearing", body.Name)
		}
	})

	t.Run("overlong name returns 400", func(t *testing.T) {
		repo := &fakeHandlerStorageTypeRepo{nextID: 10, rows: []model.StorageType{
			handlerStorageTypeRow(1, nil, "local-ssd", true, "dir", "images"),
		}}
		r := newStorageTypeTestRouter(t, cred, repo, &fakeHandlerStorageNodeRepo{}, &fakeHandlerStorageZoneRepo{}, nil)
		long := strings.Repeat("名", 256)
		body, _ := json.Marshal(map[string]string{"name": long})
		w := doStorageTypesRequest(t, r, http.MethodPut, "/storage-types/1", adminToken(t), string(body))
		assertHandlerError(t, w, http.StatusBadRequest, CodeBadRequest)
	})

	t.Run("unknown id returns 404", func(t *testing.T) {
		r := newStorageTypeTestRouter(t, cred, &fakeHandlerStorageTypeRepo{nextID: 10}, &fakeHandlerStorageNodeRepo{}, &fakeHandlerStorageZoneRepo{}, nil)
		w := doStorageTypesRequest(t, r, http.MethodPut, "/storage-types/99", adminToken(t), `{"enabled": false}`)
		assertHandlerError(t, w, http.StatusNotFound, CodeNotFound)
	})

	t.Run("non-numeric id returns 400", func(t *testing.T) {
		r := newStorageTypeTestRouter(t, cred, &fakeHandlerStorageTypeRepo{}, &fakeHandlerStorageNodeRepo{}, &fakeHandlerStorageZoneRepo{}, nil)
		w := doStorageTypesRequest(t, r, http.MethodPut, "/storage-types/abc", adminToken(t), `{"enabled": false}`)
		assertHandlerError(t, w, http.StatusBadRequest, CodeBadRequest)
	})
}

// TestStorageTypeGetHandler 覆盖 GET /storage-types/:id 的 404 语义。
func TestStorageTypeGetHandler(t *testing.T) {
	cred := newHandlerStorageCred()
	r := newStorageTypeTestRouter(t, cred, &fakeHandlerStorageTypeRepo{nextID: 10}, &fakeHandlerStorageNodeRepo{}, &fakeHandlerStorageZoneRepo{}, nil)
	w := doStorageTypesRequest(t, r, http.MethodGet, "/storage-types/99", adminToken(t), "")
	assertHandlerError(t, w, http.StatusNotFound, CodeNotFound)
}
