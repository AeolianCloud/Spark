package handlers

import (
	"context"
	"encoding/json"
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
	"spark/repository"
	"spark/service"
)

// TestMapVMServiceErrorImportKinds 锁定"导入已有 VM"新增错误类型的映射：
// vmid 不在节点 PVE 上 -> 404 vm_not_found_on_node；重复托管 -> 409
// vm_already_managed；而 zone/node 不存在的普通 404 仍保持 not_found，
// 不受新 Kind 影响。
func TestMapVMServiceErrorImportKinds(t *testing.T) {
	tests := []struct {
		name       string
		serr       *service.Error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "vmid absent on node maps to 404 vm_not_found_on_node",
			serr:       &service.Error{Kind: service.KindVMNotFoundOnNode, Message: "vm 100 not found on node \"pve-1\""},
			wantStatus: http.StatusNotFound,
			wantCode:   CodeVMNotFoundOnNode,
		},
		{
			name:       "duplicate managed vm maps to 409 vm_already_managed",
			serr:       &service.Error{Kind: service.KindVMAlreadyManaged, Message: "vm already managed: node 1 pve_vmid 100"},
			wantStatus: http.StatusConflict,
			wantCode:   CodeVMAlreadyManaged,
		},
		{
			name:       "missing zone keeps generic not_found",
			serr:       &service.Error{Kind: service.KindNotFound, Message: "zone 9 not found"},
			wantStatus: http.StatusNotFound,
			wantCode:   CodeNotFound,
		},
		{
			name:       "malformed vm id maps to 400 invalid_vm_id",
			serr:       &service.Error{Kind: service.KindInvalidVMRef, Message: "invalid external vm id \"ext-1\""},
			wantStatus: http.StatusBadRequest,
			wantCode:   CodeInvalidVMID,
		},
		{
			name:       "operation record write failure maps to 500 operation_log_failed",
			serr:       &service.Error{Kind: service.KindOperationLogFailed, Message: "start accepted but record failed"},
			wantStatus: http.StatusInternalServerError,
			wantCode:   CodeOperationLogFailed,
		},
		{
			name:       "no available ip pool maps to 400 no_available_ip_pool",
			serr:       &service.Error{Kind: service.KindNoAvailableIPPool, Message: "no available ip pool in zone 1"},
			wantStatus: http.StatusBadRequest,
			wantCode:   CodeNoAvailableIPPool,
		},
		{
			name:       "storage not available in zone maps to 400 storage_not_available_in_zone",
			serr:       &service.Error{Kind: service.KindStorageNotAvailableInZone, Message: `storage type "local-ssd" is not available on any candidate node in zone 1`},
			wantStatus: http.StatusBadRequest,
			wantCode:   CodeStorageNotAvailableInZone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr, ok := mapVMServiceError(tt.serr).(*APIError)
			if !ok {
				t.Fatalf("mapVMServiceError(%v) = %T, want *APIError", tt.serr, mapVMServiceError(tt.serr))
			}
			if apiErr.Status != tt.wantStatus {
				t.Errorf("status = %d, want %d", apiErr.Status, tt.wantStatus)
			}
			if apiErr.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", apiErr.Code, tt.wantCode)
			}
		})
	}
}

// TestVMResponseOmitsNilAssociations 验证导入的 VM（image_id/storage_type_id
// 为 nil）序列化时省略这两个字段；有值（普通创建）时正常输出 ——
// 契约要求导入响应中不出现无关的关联字段。
func TestVMResponseOmitsNilAssociations(t *testing.T) {
	imported := toVMResponse(&repository.VMWithIP{VM: model.VM{ID: 7, ImageID: nil, StorageTypeID: nil}}, "ready")
	raw, err := json.Marshal(imported)
	if err != nil {
		t.Fatalf("marshal imported vm: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["image_id"]; ok {
		t.Error("imported vm must omit image_id")
	}
	if _, ok := m["storage_type_id"]; ok {
		t.Error("imported vm must omit storage_type_id")
	}

	imgID, stID := int64(3), int64(5)
	created := toVMResponse(&repository.VMWithIP{VM: model.VM{ID: 8, ImageID: &imgID, StorageTypeID: &stID}}, "ready")
	raw, err = json.Marshal(created)
	if err != nil {
		t.Fatalf("marshal created vm: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["image_id"] != float64(3) {
		t.Errorf("image_id = %v, want 3", m["image_id"])
	}
	if m["storage_type_id"] != float64(5) {
		t.Errorf("storage_type_id = %v, want 5", m["storage_type_id"])
	}
}

// TestVMListItemExternalSerialization 固定 external 条目的公开形态（设计
// D2）：id 为合成标识 ext-{nodeID}-{vmid}（字符串）、uuid/created_at 为
// 空字符串、source=external、规格来自 PVE 摘要；本地条目的 id 保持数字。
func TestVMListItemExternalSerialization(t *testing.T) {
	// 本地条目：id 保持数字。
	local := toVMListItem(&service.VMListItem{
		VM: repository.VMWithIP{VM: model.VM{ID: 7, UUID: "u-1", Name: "vm1", CPU: 2, MemMB: 2048, DiskGB: 10,
			Source: model.VMSourceSparkCreated, CreatedAt: time.Unix(100, 0)}},
		Status: "running",
	})
	raw, err := json.Marshal(local)
	if err != nil {
		t.Fatalf("marshal local item: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal local item: %v", err)
	}
	if m["id"] != float64(7) {
		t.Errorf("local id = %v, want numeric 7", m["id"])
	}
	if m["source"] != model.VMSourceSparkCreated || m["uuid"] != "u-1" {
		t.Errorf("local source/uuid = %v / %v", m["source"], m["uuid"])
	}

	// external 条目：合成 id、uuid/created_at 空、source=external。
	ext := toVMListItem(&service.VMListItem{
		VM: repository.VMWithIP{VM: model.VM{Name: "ext-vm", ZoneID: 1, NodeID: 3, PVEVmid: 200,
			CPU: 4, MemMB: 8192, DiskGB: 100, Source: model.VMSourceExternal}},
		Status:     "running",
		ExternalID: "ext-3-200",
	})
	raw, err = json.Marshal(ext)
	if err != nil {
		t.Fatalf("marshal external item: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal external item: %v", err)
	}
	if m["id"] != "ext-3-200" {
		t.Errorf("external id = %v, want ext-3-200", m["id"])
	}
	if m["uuid"] != "" || m["created_at"] != "" {
		t.Errorf("external uuid/created_at = %v / %v, want empty strings", m["uuid"], m["created_at"])
	}
	if m["source"] != model.VMSourceExternal || m["name"] != "ext-vm" ||
		m["cpu"] != float64(4) || m["mem_mb"] != float64(8192) || m["disk_gb"] != float64(100) ||
		m["node_id"] != float64(3) || m["pve_vmid"] != float64(200) {
		t.Errorf("external fields = %v, want the PVE summary values", m)
	}
}

// ---------- Get handler 测试（vm-page-experience，设计 D6） ----------

// handlerVMStubs 是 handler Get 测试的 VMService 依赖替身集合：仅
// GetVM（vmRepo）、GetNode（nodeRepo）与 PVE 客户端被使用，其余方法按
// 未使用处理，返回零值。
type handlerVMStubs struct {
	vmRepoGet    *repository.VMWithIP
	vmRepoGetErr error
	nodes        []model.PVENode
	nodeErr      error
	vmOps        []model.VMOperation // ListOperations 的脚本化返回
	userByID     *model.User         // GetUserByID 的脚本化返回（可空）
}

func (s *handlerVMStubs) Begin(ctx context.Context) (pgx.Tx, error) { return nil, nil }
func (s *handlerVMStubs) CreateVMTx(ctx context.Context, tx pgx.Tx, vm model.VM) (*model.VM, error) {
	return nil, nil
}
func (s *handlerVMStubs) ImportVMTx(ctx context.Context, tx pgx.Tx, vm model.VM) (*model.VM, error) {
	return nil, nil
}
func (s *handlerVMStubs) GetVMByNodeVMID(ctx context.Context, nodeID, vmid int64) (*model.VM, error) {
	return nil, pgx.ErrNoRows
}
func (s *handlerVMStubs) GetVM(ctx context.Context, id int64) (*repository.VMWithIP, error) {
	if s.vmRepoGetErr != nil {
		return nil, s.vmRepoGetErr
	}
	if s.vmRepoGet == nil {
		return nil, pgx.ErrNoRows
	}
	return s.vmRepoGet, nil
}
func (s *handlerVMStubs) ListVMs(ctx context.Context) ([]repository.VMWithIP, error) { return nil, nil }
func (s *handlerVMStubs) ListVMsByUser(ctx context.Context, userID int64) ([]repository.VMWithIP, error) {
	return nil, nil
}
func (s *handlerVMStubs) SetVMIPIDTx(ctx context.Context, tx pgx.Tx, id, ipID int64) error {
	return nil
}
func (s *handlerVMStubs) UpdateVMPVEVMID(ctx context.Context, id, vmid, diskGB int64) error {
	return nil
}
func (s *handlerVMStubs) SetProvisionError(ctx context.Context, id int64, message string) error {
	return nil
}
func (s *handlerVMStubs) UpdateSpec(ctx context.Context, id int64, newCPU int, newMemMB, newDiskGB int64, oldCPU int, oldMemMB, oldDiskGB int64) error {
	return nil
}
func (s *handlerVMStubs) DeleteVMTx(ctx context.Context, tx pgx.Tx, id int64) error { return nil }
func (s *handlerVMStubs) CreateOperation(ctx context.Context, op model.VMOperation) (*model.VMOperation, error) {
	return nil, nil
}
func (s *handlerVMStubs) ListOperations(ctx context.Context, nodeID, vmid int64, limit, offset int) ([]model.VMOperation, int, error) {
	return s.vmOps, len(s.vmOps), nil
}
func (s *handlerVMStubs) GetPool(ctx context.Context, id int64) (*model.IPPool, error) {
	return nil, nil
}
func (s *handlerVMStubs) ListPoolsByZone(ctx context.Context, zoneID int64) ([]model.IPPool, error) {
	return nil, nil
}
func (s *handlerVMStubs) GetPoolNodes(ctx context.Context, poolID int64) ([]model.PVENode, error) {
	return nil, nil
}
func (s *handlerVMStubs) ClaimFreeIP(ctx context.Context, tx pgx.Tx, poolID int64, vmID *int64) (model.IP, error) {
	return model.IP{}, nil
}
func (s *handlerVMStubs) ClaimIPByAddressTx(ctx context.Context, tx pgx.Tx, poolID int64, ipAddr string, vmID *int64) (model.IP, error) {
	return model.IP{}, nil
}
func (s *handlerVMStubs) ReleaseIPByVMTx(ctx context.Context, tx pgx.Tx, vmID int64) error {
	return nil
}
func (s *handlerVMStubs) GetZone(ctx context.Context, id int64) (*model.Zone, error) { return nil, nil }
func (s *handlerVMStubs) ListZones(ctx context.Context) ([]model.Zone, error)        { return nil, nil }
func (s *handlerVMStubs) GetNode(ctx context.Context, id int64) (*model.PVENode, error) {
	if s.nodeErr != nil {
		return nil, s.nodeErr
	}
	for i := range s.nodes {
		if s.nodes[i].ID == id {
			n := s.nodes[i]
			return &n, nil
		}
	}
	return nil, pgx.ErrNoRows
}
func (s *handlerVMStubs) ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	return nil, nil
}
func (s *handlerVMStubs) ListNodesByIDs(ctx context.Context, ids []int64) ([]model.PVENode, error) {
	return nil, nil
}

func (s *handlerVMStubs) GetUserByID(ctx context.Context, id int64) (*model.User, error) {
	if s.userByID != nil && s.userByID.ID == id {
		u := *s.userByID
		return &u, nil
	}
	return nil, pgx.ErrNoRows
}

// stubImageRepo 与 stubStorageRepo 是 VMImageRepository/VMStorageTypeRepository
// 的最小替身（Get 路径不使用，返回零值）。
type stubImageRepo struct{}

func (stubImageRepo) Get(ctx context.Context, id int64) (*model.Image, error) { return nil, nil }

type stubStorageRepo struct{}

// Get 返回一个合法的存储类型快照：创建 VM 的存储可用性两道闸（设计 D5）
// 要求所选存储启用且 content 含 images，P1 加固要求存储属于请求 zone
// （ZoneID=1，与本文件 CreateVM 测试请求的 zone_id=1 一致）——替身返回
// 合法值让请求流走到被测分支（归属校验等），而非在存储校验处提前返回。
func (stubStorageRepo) Get(ctx context.Context, id int64) (*model.StorageType, error) {
	content := "images"
	return &model.StorageType{ID: id, ZoneID: 1, Enabled: true, Content: &content}, nil
}

// newVMGetTestService 装配 handler Get 测试的 VMService：stub 替身集合 +
// 指向假 PVE 服务器的客户端工厂（cipher 传 nil——Get 路径不触碰密码器）。
func newVMGetTestService(t *testing.T, stubs *handlerVMStubs, pveSrv *httptest.Server) *service.VMService {
	t.Helper()
	svc := service.NewVMService(stubs, stubs, stubs, stubs, stubs, stubs, stubImageRepo{}, stubStorageRepo{}, stubs, nil)
	if pveSrv != nil {
		svc.SetClientFactory(func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient(host, apiUser, apiTokenSecret,
				pve.WithBaseURL(pveSrv.URL), pve.WithHTTPClient(pveSrv.Client()), pve.WithTimeout(5*time.Second))
		})
	}
	return svc
}

// newVMGetEngine 构建挂载 /vms 路由的 gin 引擎（Get 路径测试用）。
func newVMGetEngine(svc *service.VMService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterVMsRoutes(r.Group("/vms"), svc)
	return r
}

// withIdentity 是 handler 测试中模拟 requireAuth 注入身份的中间件（设计
// D4：身份经 middleware.IdentityKey 注入 gin.Context，供 handler 分流）。
func withIdentity(ident middleware.Identity) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(middleware.IdentityKey, ident)
		c.Next()
	}
}

// newVMEngineWithIdentity 构建带身份注入中间件的 gin 引擎，验证 handler
// 从 gin.Context 读取身份并传递给 service 的完整链路（任务 6.7）。
func newVMEngineWithIdentity(svc *service.VMService, ident middleware.Identity) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(withIdentity(ident))
	RegisterVMsRoutes(r.Group("/vms"), svc)
	return r
}

// TestVMHandlerIdentityInjection 覆盖身份注入传递的正确性（任务 6.7）：
// 用户令牌访问他人 VM 详情 -> 403 forbidden；访问归属自己的 VM -> 200；
// ext- 标识对用户 -> 403；管理员令牌对无主 VM -> 200。
func TestVMHandlerIdentityInjection(t *testing.T) {
	uid2, uid3 := int64(2), int64(3)
	stubsOwned := &handlerVMStubs{vmRepoGet: &repository.VMWithIP{
		VM: model.VM{ID: 7, UUID: "u-7", Name: "vm7", NodeID: 1, PVEVmid: 0, UserID: &uid2,
			CPU: 2, MemMB: 2048, DiskGB: 10, Source: model.VMSourceSparkCreated},
		IP: "10.0.0.5",
	}}
	svcOwned := newVMGetTestService(t, stubsOwned, nil)
	rOwned := newVMEngineWithIdentity(svcOwned, middleware.Identity{Role: middleware.RoleUser, ID: uid2})

	// 归属自己的 VM：200。
	w := httptest.NewRecorder()
	rOwned.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/vms/7", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /vms/7 (own) status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	// 他人 VM：403 forbidden。
	stubsOther := &handlerVMStubs{vmRepoGet: &repository.VMWithIP{
		VM: model.VM{ID: 7, UUID: "u-7", Name: "vm7", NodeID: 1, PVEVmid: 0, UserID: &uid3,
			CPU: 2, MemMB: 2048, DiskGB: 10, Source: model.VMSourceSparkCreated},
		IP: "10.0.0.5",
	}}
	svcOther := newVMGetTestService(t, stubsOther, nil)
	rOther := newVMEngineWithIdentity(svcOther, middleware.Identity{Role: middleware.RoleUser, ID: uid2})
	w = httptest.NewRecorder()
	rOther.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/vms/7", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET /vms/7 (other's) status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
	var errBody map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	apiErr, ok := errBody["error"].(map[string]any)
	if !ok || apiErr["code"] != CodeForbidden {
		t.Fatalf("error = %+v, want code %s", errBody, CodeForbidden)
	}

	// ext- 标识对用户：403（纯 external，无本地托管行，不触碰 PVE）。
	stubsExt := &handlerVMStubs{nodes: []model.PVENode{
		{ID: 1, ZoneID: 3, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	svcExt := newVMGetTestService(t, stubsExt, nil)
	rExt := newVMEngineWithIdentity(svcExt, middleware.Identity{Role: middleware.RoleUser, ID: uid2})
	w = httptest.NewRecorder()
	rExt.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/vms/ext-1-200", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET /vms/ext-1-200 (user) status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}

	// 管理员令牌：无主 VM 放行（现状不回归）。
	stubsAdmin := &handlerVMStubs{vmRepoGet: &repository.VMWithIP{
		VM: model.VM{ID: 7, UUID: "u-7", Name: "vm7", NodeID: 1, PVEVmid: 0,
			CPU: 2, MemMB: 2048, DiskGB: 10, Source: model.VMSourceSparkCreated},
		IP: "10.0.0.5",
	}}
	svcAdmin := newVMGetTestService(t, stubsAdmin, nil)
	rAdmin := newVMEngineWithIdentity(svcAdmin, middleware.Identity{Role: middleware.RoleAdmin, ID: 1})
	w = httptest.NewRecorder()
	rAdmin.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/vms/7", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /vms/7 (admin) status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

// TestVMHandlerLifecycleIdentityForbidden 覆盖生命周期操作的 handler 层
// 归属校验：用户令牌对他人 VM 发起 start -> 403 forbidden（不触碰 PVE）。
func TestVMHandlerLifecycleIdentityForbidden(t *testing.T) {
	uid2, uid3 := int64(2), int64(3)
	stubs := &handlerVMStubs{vmRepoGet: &repository.VMWithIP{
		VM: model.VM{ID: 7, NodeID: 1, PVEVmid: 100, UserID: &uid3,
			CPU: 2, MemMB: 2048, DiskGB: 10, Source: model.VMSourceSparkCreated},
	}}
	svc := newVMGetTestService(t, stubs, nil)
	r := newVMEngineWithIdentity(svc, middleware.Identity{Role: middleware.RoleUser, ID: uid2})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/vms/7/start", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST /vms/7/start (other's) status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
}

// TestVMHandlerCreateImportIdentityConstraint 覆盖创建/认领 handler 层的身
// 份归属约束（H1）：user 令牌指定他人/未知用户的 user_id -> 403 forbidden
// （不触碰 PVE/不落库，且不区分未知用户，杜绝枚举 S2）；"user 不指定默认
// 归属自身"的归属解析在 service 层测试覆盖（vm_auth_test.go）。
func TestVMHandlerCreateImportIdentityConstraint(t *testing.T) {
	uid2 := int64(2)
	stubs := &handlerVMStubs{userByID: &model.User{ID: 2, Username: "u2", Status: model.UserStatusEnabled}}
	svc := newVMGetTestService(t, stubs, nil)
	r := newVMEngineWithIdentity(svc, middleware.Identity{Role: middleware.RoleUser, ID: uid2})

	// user 令牌创建 VM 时指定他人 user_id -> 403。
	w := httptest.NewRecorder()
	body := `{"name":"vm1","cpu":1,"mem_mb":1024,"disk_gb":10,"image_id":1,"storage_type_id":1,"zone_id":1,"password":"pw","user_id":3}`
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(body)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST /vms (user_id of another) status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}

	// user 令牌认领 VM 时指定他人 user_id -> 403（节点存在时在归属门禁拒绝）。
	stubsImp := &handlerVMStubs{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	svcImp := newVMGetTestService(t, stubsImp, nil)
	rImp := newVMEngineWithIdentity(svcImp, middleware.Identity{Role: middleware.RoleUser, ID: uid2})
	w = httptest.NewRecorder()
	rImp.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/vms/import", strings.NewReader(
		`{"zone_id":1,"node_id":1,"pve_vmid":100,"user_id":3}`)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST /vms/import (user_id of another) status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}

	// 未知用户的 user_id（user 99 不存在）：user 令牌只能指定自身，指定他人
	// 一律 403，绝不区分 404/400（杜绝枚举，S2）。
	stubsNoUser := &handlerVMStubs{}
	svcNoUser := newVMGetTestService(t, stubsNoUser, nil)
	rNoUser := newVMEngineWithIdentity(svcNoUser, middleware.Identity{Role: middleware.RoleUser, ID: uid2})
	w = httptest.NewRecorder()
	rNoUser.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(
		`{"name":"vm1","cpu":1,"mem_mb":1024,"disk_gb":10,"image_id":1,"storage_type_id":1,"zone_id":1,"password":"pw","user_id":99}`)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST /vms (unknown user_id) status = %d, want 403, not a user enumeration signal (body: %s)", w.Code, w.Body.String())
	}
}

// TestVMHandlerOperationsOperatorMasking 覆盖操作记录响应的操作者脱敏
// （L2）：user 令牌查看操作记录时 operator_id 置空省略、仅保留
// operator_type；admin 可见完整操作者信息。
func TestVMHandlerOperationsOperatorMasking(t *testing.T) {
	uid2, uid3 := int64(2), int64(3)
	opAdminID := int64(1)
	stubs := &handlerVMStubs{
		vmRepoGet: &repository.VMWithIP{VM: model.VM{ID: 7, NodeID: 1, PVEVmid: 100, UserID: &uid2}},
		vmOps: []model.VMOperation{
			{ID: 1, NodeID: 1, PVEVmid: 100, Action: "start", Result: "accepted",
				OperatorType: service.RoleAdmin, OperatorID: &opAdminID, CreatedAt: time.Unix(100, 0)},
			{ID: 2, NodeID: 1, PVEVmid: 100, Action: "stop", Result: "accepted",
				OperatorType: service.RoleUser, OperatorID: &uid3, CreatedAt: time.Unix(101, 0)},
		},
	}
	svc := newVMGetTestService(t, stubs, nil)

	// user 令牌：200，operator_type 保留，operator_id 一律省略。
	rUser := newVMEngineWithIdentity(svc, middleware.Identity{Role: middleware.RoleUser, ID: uid2})
	w := httptest.NewRecorder()
	rUser.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/vms/7/operations", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /vms/7/operations (user) status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode operations (user): %v", err)
	}
	ops, ok := body["operations"].([]any)
	if !ok || len(ops) != 2 {
		t.Fatalf("operations = %+v, want 2 records", body["operations"])
	}
	for i, raw := range ops {
		op := raw.(map[string]any)
		if op["operator_id"] != nil {
			t.Fatalf("user ops[%d] operator_id = %v, want masked (nil)", i, op["operator_id"])
		}
		if op["operator_type"] == nil || op["operator_type"] == "" {
			t.Fatalf("user ops[%d] operator_type = %v, want kept", i, op["operator_type"])
		}
	}

	// admin 令牌：200，operator_id 完整可见。
	rAdmin := newVMEngineWithIdentity(svc, middleware.Identity{Role: middleware.RoleAdmin, ID: 1})
	w = httptest.NewRecorder()
	rAdmin.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/vms/7/operations", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /vms/7/operations (admin) status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode operations (admin): %v", err)
	}
	ops, ok = body["operations"].([]any)
	if !ok || len(ops) != 2 {
		t.Fatalf("operations = %+v, want 2 records", body["operations"])
	}
	op0 := ops[0].(map[string]any)
	op1 := ops[1].(map[string]any)
	if op0["operator_type"] != service.RoleAdmin || op0["operator_id"] != float64(1) {
		t.Fatalf("admin ops[0] operator = %v / %v, want admin / 1", op0["operator_type"], op0["operator_id"])
	}
	if op1["operator_type"] != service.RoleUser || op1["operator_id"] != float64(3) {
		t.Fatalf("admin ops[1] operator = %v / %v, want user / 3", op1["operator_type"], op1["operator_id"])
	}
}

// TestVMGetExternal 覆盖 GET /vms/ext-{nodeID}-{vmid}：200 + external 条目
// 形态（合成 id、uuid/created_at 为空、source=external、规格取 PVE 摘要、
// 实时指标透传）。
func TestVMGetExternal(t *testing.T) {
	pveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"vmid": 200, "name": "ext-vm", "status": "running", "cpus": 2, "maxmem": 4294967296, "maxdisk": 21474836480, "cpu": 0.5, "mem": 2147483648, "uptime": 77}]}`))
	}))
	defer pveSrv.Close()

	stubs := &handlerVMStubs{nodes: []model.PVENode{
		{ID: 1, ZoneID: 3, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	svc := newVMGetTestService(t, stubs, pveSrv)
	// 未挂身份中间件的引擎在 M1 fail-closed 下会拒绝所有请求；该测试意图
	// 覆盖管理员视角的 external 形态，显式注入管理员身份。
	r := newVMEngineWithIdentity(svc, middleware.Identity{Role: middleware.RoleAdmin, ID: 1})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/vms/ext-1-200", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /vms/ext-1-200 status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["id"] != "ext-1-200" || body["source"] != model.VMSourceExternal {
		t.Fatalf("id/source = %v / %v, want ext-1-200 / external", body["id"], body["source"])
	}
	if body["uuid"] != "" || body["created_at"] != "" || body["updated_at"] != "" {
		t.Fatalf("uuid/created_at/updated_at = %v/%v/%v, want all empty", body["uuid"], body["created_at"], body["updated_at"])
	}
	if body["name"] != "ext-vm" || body["node_id"] != float64(1) || body["pve_vmid"] != float64(200) {
		t.Fatalf("identity fields = %+v, want ext-vm on node 1 vmid 200", body)
	}
	if body["cpu"] != float64(2) || body["mem_mb"] != float64(4096) || body["disk_gb"] != float64(20) {
		t.Fatalf("spec = %+v, want 2/4096/20 from the PVE summary", body)
	}
	if body["status"] != "running" || body["cpu_usage"] != 0.5 || body["uptime"] != float64(77) {
		t.Fatalf("live = %+v, want running with pass-through metrics", body)
	}
}

// TestVMGetNumericNoRegression 覆盖数字 id 不回归：GET /vms/7 -> 200 本地
// 形态（数字 id、creating 过渡状态）；GET /vms/007 经 parseIDParam 解析后
// 规范化，与 /vms/7 等价（前导零兼容，保持原路径语义）——规范化后走正常
// 查询路径：行不存在时与 /vms/7 同样返回 404 not_found，而非 handler 层
// 的 400。
func TestVMGetNumericNoRegression(t *testing.T) {
	stubs := &handlerVMStubs{vmRepoGet: &repository.VMWithIP{
		VM: model.VM{ID: 7, UUID: "u-7", Name: "vm7", NodeID: 1, PVEVmid: 0,
			CPU: 2, MemMB: 2048, DiskGB: 10, Source: model.VMSourceSparkCreated},
		IP: "10.0.0.5",
	}}
	svc := newVMGetTestService(t, stubs, nil)
	// 未挂身份中间件的引擎在 M1 fail-closed 下会拒绝所有请求；该测试意图
	// 覆盖管理员视角的数字 id 形态，显式注入管理员身份。
	r := newVMEngineWithIdentity(svc, middleware.Identity{Role: middleware.RoleAdmin, ID: 1})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/vms/7", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /vms/7 status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["id"] != float64(7) || body["status"] != model.VMStateCreating {
		t.Fatalf("id/status = %v / %v, want numeric 7 with creating", body["id"], body["status"])
	}
	if body["uuid"] != "u-7" || body["source"] != model.VMSourceSparkCreated {
		t.Fatalf("uuid/source = %v / %v, want the local row values", body["uuid"], body["source"])
	}

	// GET /vms/007：前导零经 parseIDParam 解析后规范化为 7 再传入服务层，
	// 与 /vms/7 走同一条查询路径——行不存在时同样 404 not_found，绝不落到
	// handler 层的 400（规范化是数字 id 的既有语义，非格式拒绝）。
	svc2 := newVMGetTestService(t, &handlerVMStubs{}, nil)
	r2 := newVMGetEngine(svc2)
	w = httptest.NewRecorder()
	r2.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/vms/007", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /vms/007 status = %d, want 404 not_found (body: %s)", w.Code, w.Body.String())
	}
	var errBody map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body of /vms/007: %v", err)
	}
	apiErr, ok := errBody["error"].(map[string]any)
	if !ok || apiErr["code"] != CodeNotFound {
		t.Fatalf("GET /vms/007 error = %+v, want code %s", errBody, CodeNotFound)
	}
}

// TestVMGetInvalidRefs 覆盖 handler 层的 id 格式校验：数字 id 走
// parseIDParam 原路径（0/负数 400 bad_request）；非数字须匹配 ext- 正则，
// 非法格式 400 bad_request——包括 /vms/unmanaged 等旧形态请求的契约保持。
func TestVMGetInvalidRefs(t *testing.T) {
	svc := newVMGetTestService(t, &handlerVMStubs{}, nil)
	r := newVMGetEngine(svc)
	cases := []string{
		"ext-0-100",  // nodeID 为 0
		"ext-1-0",    // vmid 为 0
		"ext-01-100", // nodeID 前导零
		"ext-1-01",   // vmid 前导零
		"ext-1",      // 缺段
		"ext-1-2-3",  // 多余段
		"ext--1-2",   // 负数
		"ext-abc-100",
		"unmanaged", // 已下线接口的旧路径形态，保持 400 bad_request
		"abc",
		"0",
		"-1",
	}
	for _, id := range cases {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/vms/"+id, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("GET /vms/%s status = %d, want 400 (body: %s)", id, w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode error body of %s: %v", id, err)
		}
		apiErr, ok := body["error"].(map[string]any)
		if !ok || apiErr["code"] != "bad_request" {
			t.Fatalf("GET /vms/%s error = %+v, want code bad_request", id, body)
		}
	}
}
