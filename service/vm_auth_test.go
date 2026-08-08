package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spark/model"
	"spark/pve"
	"spark/repository"
)

// ---------- 创建/认领的可选归属用户（任务 6.1/6.2，设计 D3） ----------

// TestCreateVMWithUserID 覆盖创建 VM 的可选归属用户：存在且启用 -> 落库
// user_id；无主（nil）-> 不落库；用户不存在 -> not_found；用户禁用 ->
// bad_request（禁用用户不得获得新资源）。
func TestCreateVMWithUserID(t *testing.T) {
	// 快乐路径：user 令牌归属自己（user_id 与身份一致），用户存在且启用。
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 7, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}}}
	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)
	svc.userRepo = &fakeVMUserRepository{users: []model.User{testVMUser(2)}}

	req := validCreateRequest()
	uid := int64(2)
	req.UserID = &uid
	vm, err := svc.CreateVM(context.Background(), userIdentity(2), req)
	if err != nil {
		t.Fatalf("CreateVM with user: %v", err)
	}
	if vm.VM.UserID == nil || *vm.VM.UserID != 2 {
		t.Fatalf("user_id = %v, want 2", vm.VM.UserID)
	}
	if vmRepo.created == nil || vmRepo.created.UserID == nil || *vmRepo.created.UserID != 2 {
		t.Fatalf("persisted user_id = %v, want 2", vmRepo.created)
	}
	waitForProvision(t, vmRepo)

	// 无主：admin 令牌 + UserID 为 nil，落库保持空。
	zoneRepo2, imageRepo2, stRepo2, nodeRepo2, ipRepo2 := createEnv()
	ipRepo2.claimResults = []claimResult{{ip: model.IP{ID: 8, PoolID: 1, IP: "10.0.0.6", Status: model.IPStatusUsed}}}
	vmRepo2 := &fakeVMRepository{}
	svc2 := newVMService(t, vmRepo2, ipRepo2, zoneRepo2, nodeRepo2, imageRepo2, stRepo2)
	svc2.userRepo = &fakeVMUserRepository{users: []model.User{testVMUser(2)}}
	vm2, err := svc2.CreateVM(context.Background(), adminIdentity(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreateVM without user: %v", err)
	}
	if vm2.VM.UserID != nil {
		t.Fatalf("user_id = %v, want nil (unowned)", vm2.VM.UserID)
	}
	waitForProvision(t, vmRepo2)

	// 用户不存在 -> not_found（admin 指定任意 user_id 时的校验）。
	zoneRepo3, imageRepo3, stRepo3, nodeRepo3, ipRepo3 := createEnv()
	svc3 := newVMService(t, &fakeVMRepository{}, ipRepo3, zoneRepo3, nodeRepo3, imageRepo3, stRepo3)
	svc3.userRepo = &fakeVMUserRepository{users: []model.User{testVMUser(2)}}
	req3 := validCreateRequest()
	uid3 := int64(99)
	req3.UserID = &uid3
	if _, err := svc3.CreateVM(context.Background(), adminIdentity(), req3); !isKind(err, KindNotFound) {
		t.Fatalf("unknown user err = %v, want KindNotFound", err)
	}

	// 用户禁用 -> bad_request（admin 指定禁用用户时的校验）。
	zoneRepo4, imageRepo4, stRepo4, nodeRepo4, ipRepo4 := createEnv()
	svc4 := newVMService(t, &fakeVMRepository{}, ipRepo4, zoneRepo4, nodeRepo4, imageRepo4, stRepo4)
	disabled := testVMUser(2)
	disabled.Status = model.UserStatusDisabled
	svc4.userRepo = &fakeVMUserRepository{users: []model.User{disabled}}
	req4 := validCreateRequest()
	uid4 := int64(2)
	req4.UserID = &uid4
	if _, err := svc4.CreateVM(context.Background(), adminIdentity(), req4); !isKind(err, KindBadRequest) {
		t.Fatalf("disabled user err = %v, want KindBadRequest", err)
	}
}

// TestImportVMWithUserID 覆盖认领 VM 的可选归属用户（设计 D3）：存在且启用
// -> 落库 user_id；用户不存在 -> not_found；禁用 -> bad_request。
func TestImportVMWithUserID(t *testing.T) {
	// 快乐路径：user 令牌归属自己（user_id 与身份一致），用户存在且启用。
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)
	svc.userRepo = &fakeVMUserRepository{users: []model.User{testVMUser(2)}}

	uid := int64(2)
	vm, err := svc.ImportVM(context.Background(), userIdentity(2), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, UserID: &uid})
	if err != nil {
		t.Fatalf("ImportVM with user: %v", err)
	}
	if vm.VM.UserID == nil || *vm.VM.UserID != 2 {
		t.Fatalf("user_id = %v, want 2", vm.VM.UserID)
	}
	if vmRepo.imported == nil || vmRepo.imported.UserID == nil || *vmRepo.imported.UserID != 2 {
		t.Fatalf("persisted user_id = %v, want 2", vmRepo.imported)
	}

	// 用户不存在 -> not_found（admin 指定任意 user_id 时的校验，不触碰 PVE）。
	zoneRepo2, nodeRepo2, ipRepo2 := importTestEnv()
	noCall := noCallServer(t)
	defer noCall.Close()
	svc2 := newImportSvc(t, &fakeVMRepository{}, ipRepo2, zoneRepo2, nodeRepo2, noCall)
	svc2.userRepo = &fakeVMUserRepository{users: []model.User{testVMUser(2)}}
	uid99 := int64(99)
	_, err = svc2.ImportVM(context.Background(), adminIdentity(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, UserID: &uid99})
	if !isKind(err, KindNotFound) {
		t.Fatalf("unknown user err = %v, want KindNotFound", err)
	}

	// 用户禁用 -> bad_request。
	svc3 := newImportSvc(t, &fakeVMRepository{}, ipRepo2, zoneRepo2, nodeRepo2, noCall)
	disabled := testVMUser(2)
	disabled.Status = model.UserStatusDisabled
	svc3.userRepo = &fakeVMUserRepository{users: []model.User{disabled}}
	uid2 := int64(2)
	_, err = svc3.ImportVM(context.Background(), adminIdentity(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, UserID: &uid2})
	if !isKind(err, KindBadRequest) {
		t.Fatalf("disabled user err = %v, want KindBadRequest", err)
	}
}

// ---------- 列表分流（任务 6.3，设计 D5） ----------

// TestListVMsIdentityScoping 覆盖列表身份分流：用户令牌仅返回归属自己的
// 本地 VM（他人/无主/external 一律不可见），total 口径一致；管理员返回
// 合并后的全量（含 external）；nil 身份（M1 fail-closed）按用户语义处理
// ——user_id 按 0 过滤，列表为空，绝不放行为管理员的全量视图。
func TestListVMsIdentityScoping(t *testing.T) {
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nodes/pve1/qemu" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data": [
			{"vmid": 100, "name": "vm1", "status": "running"},
			{"vmid": 999, "name": "ext-orphan", "status": "stopped"}
		]}`)
	}))
	defer alive.Close()

	zoneRepo := &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	uid2, uid3 := int64(2), int64(3)
	own := vw(1, 1, 100, "vm1")
	own.VM.UserID = &uid2
	other := vw(2, 1, 200, "vm2")
	other.VM.UserID = &uid3
	unowned := vw(3, 1, 300, "vm3") // 无主
	vmRepo := &fakeVMRepository{vms: []repository.VMWithIP{own, other, unowned}}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, zoneRepo, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(alive.URL), pve.WithHTTPClient(alive.Client()), pve.WithTimeout(5*time.Second))
	}

	// 用户 2：仅自己的 vm1，external（999）与 他人/无主 不可见。
	items, _, total, err := svc.ListVMs(context.Background(), userIdentity(2), 25, 0)
	if err != nil {
		t.Fatalf("ListVMs as user: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].VM.VM.ID != 1 {
		t.Fatalf("user items = %+v total = %d, want only vm1 (own)", items, total)
	}
	for _, it := range items {
		if it.ExternalID != "" {
			t.Fatalf("user list must not contain external entries, got %+v", it)
		}
	}

	// 管理员：全量合并（3 本地 + 1 external = 4）。
	items, _, total, err = svc.ListVMs(context.Background(), adminIdentity(), 25, 0)
	if err != nil {
		t.Fatalf("ListVMs as admin: %v", err)
	}
	if total != 4 || len(items) != 4 {
		t.Fatalf("admin items = %+v total = %d, want 4 merged entries", items, total)
	}
	if items[3].ExternalID != "ext-1-999" {
		t.Fatalf("admin list must include the external entry, got %+v", items[3])
	}

	// nil 身份（M1 fail-closed）：按非管理员处理——user_id 按 0 过滤，
	// 无任何归属用户 0 的 VM，列表为空；绝不放行为管理员的全量视图。
	items, _, total, err = svc.ListVMs(context.Background(), nil, 25, 0)
	if err != nil {
		t.Fatalf("ListVMs with nil identity: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Fatalf("nil-identity items = %+v total = %d, want empty (fail-closed)", items, total)
	}
}

// TestListVMsUserWarningsScoped 覆盖用户视角列表警告的收窄（M2）：节点 2
// 查询失败时，user 令牌只看到自己 VM 所在节点（节点 1）的警告或空——
// 节点 2 的失败/禁用绝不泄露；管理员照常看到全部警告。
func TestListVMsUserWarningsScoped(t *testing.T) {
	// 节点 1 正常应答（含用户自己的 VM 100），节点 2 全部 500。
	node1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [{"vmid": 100, "name": "vm1", "status": "running"}]}`)
	}))
	defer node1.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"errors": {"_": "pve daemon down"}}`)
	}))
	defer dead.Close()

	zoneRepo := &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
		{ID: 2, ZoneID: 1, Name: "pve2", Host: "h2", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	uid2 := int64(2)
	own := vw(1, 1, 100, "vm1")
	own.VM.UserID = &uid2
	vmRepo := &fakeVMRepository{vms: []repository.VMWithIP{own}}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, zoneRepo, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	clients := map[string]*httptest.Server{"h1": node1, "h2": dead}
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(clients[host].URL), pve.WithHTTPClient(clients[host].Client()), pve.WithTimeout(5*time.Second))
	}

	// 用户视角：自己的 VM 在节点 1（正常），节点 2 的失败警告被收窄掉。
	items, warnings, _, err := svc.ListVMs(context.Background(), userIdentity(2), 25, 0)
	if err != nil {
		t.Fatalf("ListVMs as user: %v", err)
	}
	if len(items) != 1 || items[0].VM.VM.ID != 1 {
		t.Fatalf("user items = %+v, want only own vm1", items)
	}
	for _, w := range warnings {
		if w.Node == "pve2" {
			t.Fatalf("user warnings must not leak node pve2 availability, got %+v", warnings)
		}
	}

	// 管理员视角：两条节点的失败警告都在。
	_, warnings, _, err = svc.ListVMs(context.Background(), adminIdentity(), 25, 0)
	if err != nil {
		t.Fatalf("ListVMs as admin: %v", err)
	}
	found := map[string]bool{}
	for _, w := range warnings {
		found[w.Node] = true
	}
	if !found["pve2"] {
		t.Fatalf("admin warnings = %+v, want node pve2 failure included", warnings)
	}
}

// ---------- 创建/认领的归属身份约束（任务 6.1/6.2，H1） ----------

// TestCreateVMIdentityConstraint 覆盖创建 VM 的身份归属约束（H1）：user
// 令牌指定他人 user_id -> 403；user 令牌不指定 -> 默认归属自身；admin 可
// 任意指定或留空（无主）；nil 身份（M1 fail-closed）按用户语义处理
// （user_id 0 -> not_found）。
func TestCreateVMIdentityConstraint(t *testing.T) {
	ctx := context.Background()

	// user 令牌不指定 user_id：默认归属自身。
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 7, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}}}
	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)
	svc.userRepo = &fakeVMUserRepository{users: []model.User{testVMUser(2)}}
	vm, err := svc.CreateVM(ctx, userIdentity(2), validCreateRequest())
	if err != nil {
		t.Fatalf("user CreateVM without user_id: %v", err)
	}
	if vm.VM.UserID == nil || *vm.VM.UserID != 2 {
		t.Fatalf("user_id = %v, want default to own user 2", vm.VM.UserID)
	}
	waitForProvision(t, vmRepo)

	// user 令牌指定他人 user_id -> 403（不触碰 PVE/不落库）。
	zoneRepo2, imageRepo2, stRepo2, nodeRepo2, ipRepo2 := createEnv()
	vmRepo2 := &fakeVMRepository{}
	svc2 := newVMService(t, vmRepo2, ipRepo2, zoneRepo2, nodeRepo2, imageRepo2, stRepo2)
	svc2.userRepo = &fakeVMUserRepository{users: []model.User{testVMUser(2), testVMUser(3)}}
	req2 := validCreateRequest()
	uid3 := int64(3)
	req2.UserID = &uid3
	if _, err := svc2.CreateVM(ctx, userIdentity(2), req2); !isKind(err, KindForbidden) {
		t.Fatalf("user create for other user err = %v, want KindForbidden", err)
	}
	if vmRepo2.created != nil {
		t.Fatal("CreateVMTx must not be called for a forbidden cross-user create")
	}

	// admin 指定任意用户（存在且启用）-> 落库该 user_id。
	zoneRepo3, imageRepo3, stRepo3, nodeRepo3, ipRepo3 := createEnv()
	ipRepo3.claimResults = []claimResult{{ip: model.IP{ID: 9, PoolID: 1, IP: "10.0.0.7", Status: model.IPStatusUsed}}}
	vmRepo3 := &fakeVMRepository{}
	svc3 := newVMService(t, vmRepo3, ipRepo3, zoneRepo3, nodeRepo3, imageRepo3, stRepo3)
	svc3.userRepo = &fakeVMUserRepository{users: []model.User{testVMUser(3)}}
	req3 := validCreateRequest()
	req3.UserID = &uid3
	vm3, err := svc3.CreateVM(ctx, adminIdentity(), req3)
	if err != nil {
		t.Fatalf("admin CreateVM with user_id: %v", err)
	}
	if vm3.VM.UserID == nil || *vm3.VM.UserID != 3 {
		t.Fatalf("user_id = %v, want 3 (admin may assign any user)", vm3.VM.UserID)
	}
	waitForProvision(t, vmRepo3)

	// nil 身份（M1 fail-closed）：按用户语义处理，user_id 默认 0 ->
	// not_found（身份缺失不得被放行为管理员创建无主 VM）。
	zoneRepo4, imageRepo4, stRepo4, nodeRepo4, ipRepo4 := createEnv()
	svc4 := newVMService(t, &fakeVMRepository{}, ipRepo4, zoneRepo4, nodeRepo4, imageRepo4, stRepo4)
	if _, err := svc4.CreateVM(ctx, nil, validCreateRequest()); !isKind(err, KindNotFound) {
		t.Fatalf("nil identity CreateVM err = %v, want KindNotFound (fail-closed)", err)
	}
}

// TestImportVMIdentityConstraint 覆盖认领 VM 的身份归属约束（H1）：user
// 令牌指定他人 user_id -> 403；user 令牌不指定 -> 默认归属自身；admin 可
// 任意指定。
func TestImportVMIdentityConstraint(t *testing.T) {
	ctx := context.Background()

	// user 令牌不指定 user_id：默认归属自身。
	zoneRepo, nodeRepo, ipRepo := importTestEnv()
	vmRepo := &fakeVMRepository{}
	srv := newImportPVEServer(t, importListJSON(), map[int64]string{100: importConfigJSON()})
	defer srv.Close()
	svc := newImportSvc(t, vmRepo, ipRepo, zoneRepo, nodeRepo, srv)
	svc.userRepo = &fakeVMUserRepository{users: []model.User{testVMUser(2)}}
	vm, err := svc.ImportVM(ctx, userIdentity(2), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100})
	if err != nil {
		t.Fatalf("user ImportVM without user_id: %v", err)
	}
	if vm.VM.UserID == nil || *vm.VM.UserID != 2 {
		t.Fatalf("user_id = %v, want default to own user 2", vm.VM.UserID)
	}

	// user 令牌指定他人 user_id -> 403（不触碰 PVE/不落库）。
	noCall := noCallServer(t)
	defer noCall.Close()
	svc2 := newImportSvc(t, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo, noCall)
	svc2.userRepo = &fakeVMUserRepository{users: []model.User{testVMUser(2), testVMUser(3)}}
	uid3 := int64(3)
	if _, err := svc2.ImportVM(ctx, userIdentity(2), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, UserID: &uid3}); !isKind(err, KindForbidden) {
		t.Fatalf("user import for other user err = %v, want KindForbidden", err)
	}

	// admin 指定任意用户 -> 落库该 user_id（无主留空已被既有测试覆盖）。
	vmRepo3 := &fakeVMRepository{}
	svc3 := newImportSvc(t, vmRepo3, ipRepo, zoneRepo, nodeRepo, srv)
	svc3.userRepo = &fakeVMUserRepository{users: []model.User{testVMUser(3)}}
	vm3, err := svc3.ImportVM(ctx, adminIdentity(), ImportVMRequest{ZoneID: 1, NodeID: 1, PVEVmid: 100, UserID: &uid3})
	if err != nil {
		t.Fatalf("admin ImportVM with user_id: %v", err)
	}
	if vm3.VM.UserID == nil || *vm3.VM.UserID != 3 {
		t.Fatalf("user_id = %v, want 3 (admin may assign any user)", vm3.VM.UserID)
	}
}

// ---------- 生命周期操作归属校验（任务 6.4，设计 D5） ----------

// TestLifecycleOpOwnership 覆盖 start/stop/restart 的归属校验：用户操作
// 归属自己的 VM -> 放行且操作记录携带操作者；操作他人/无主 VM -> 403；
// 管理员放行（含无主 VM）；ext- 对用户一律 403。
func TestLifecycleOpOwnership(t *testing.T) {
	ts := startStopRestartServer(t)
	defer ts.Close()
	ctx := context.Background()

	newSvc := func(vm *repository.VMWithIP) (*VMService, *fakeVMOperationRepository) {
		opRepo := &fakeVMOperationRepository{}
		nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
			{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
		}}
		svc := newVMServiceWithOps(t, opRepo, &fakeVMRepository{get: vm}, &fakeVMIPPoolRepository{},
			&fakeVMZoneRepository{}, nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
		svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient("pve1", apiUser, apiTokenSecret,
				pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
		}
		return svc, opRepo
	}

	// 用户操作归属自己的 VM：放行，accepted 记录携带操作者 user/2（任务 6.6）。
	own := provisionedVM()
	uid := int64(2)
	own.VM.UserID = &uid
	svc, opRepo := newSvc(own)
	if err := svc.Start(ctx, "1", userIdentity(2)); err != nil {
		t.Fatalf("Start own vm: %v", err)
	}
	if len(opRepo.ops) != 1 {
		t.Fatalf("operations = %+v, want 1 accepted record", opRepo.ops)
	}
	op := opRepo.ops[0]
	if op.OperatorType != RoleUser || op.OperatorID == nil || *op.OperatorID != 2 {
		t.Fatalf("operator = %q / %v, want user / 2", op.OperatorType, op.OperatorID)
	}

	// 用户操作他人的 VM -> 403（PVE 不被调用）。
	other := provisionedVM()
	uid3 := int64(3)
	other.VM.UserID = &uid3
	svc2, _ := newSvc(other)
	if err := svc2.Stop(ctx, "1", userIdentity(2)); !isKind(err, KindForbidden) {
		t.Fatalf("stop other's vm err = %v, want KindForbidden", err)
	}

	// 用户操作无主 VM -> 403（用户视角视同不存在）。
	svc3, _ := newSvc(provisionedVM())
	if err := svc3.Restart(ctx, "1", userIdentity(2)); !isKind(err, KindForbidden) {
		t.Fatalf("restart unowned vm err = %v, want KindForbidden", err)
	}

	// 管理员放行无主 VM（现状不回归），记录携带操作者 admin/1。
	svc4, opRepo4 := newSvc(provisionedVM())
	if err := svc4.Start(ctx, "1", adminIdentity()); err != nil {
		t.Fatalf("admin start unowned vm: %v", err)
	}
	if len(opRepo4.ops) != 1 || opRepo4.ops[0].OperatorType != RoleAdmin ||
		opRepo4.ops[0].OperatorID == nil || *opRepo4.ops[0].OperatorID != 1 {
		t.Fatalf("admin operator = %+v, want admin/1", opRepo4.ops)
	}

	// 用户操作不存在的 VM -> 404（现状不回归）。
	svc5, _ := newSvc(nil)
	if err := svc5.Start(ctx, "404", userIdentity(2)); !isKind(err, KindNotFound) {
		t.Fatalf("start missing vm err = %v, want KindNotFound", err)
	}
}

// TestLifecycleOpExternalOwnership 覆盖 ext- 标识的归属校验（任务 6.4）：
// 纯 external（本地无托管行）对用户一律 403 且不触碰 PVE；ext- 指向本地
// 托管行时按该行归属校验（归属自身放行，G1 路由语义）。
func TestLifecycleOpExternalOwnership(t *testing.T) {
	ctx := context.Background()
	noCall := noCallServer(t)
	defer noCall.Close()

	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 3, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	newSvc := func(vmRepo VMRepository, srv *httptest.Server) *VMService {
		svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{}, nodeRepo,
			&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
		if srv != nil {
			svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
				return pve.NewClient("pve1", apiUser, apiTokenSecret,
					pve.WithBaseURL(srv.URL), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second))
			}
		}
		return svc
	}

	// 纯 external：用户 403，绝不发起 PVE 调用。
	svc := newSvc(&fakeVMRepository{}, noCall)
	if err := svc.Start(ctx, "ext-3-200", userIdentity(2)); !isKind(err, KindForbidden) {
		t.Fatalf("user start external err = %v, want KindForbidden", err)
	}
	// 管理员照常放行 ext-（现状不回归）：服务器需同时应答 PVE 列表预检
	//（resolveVMTarget）与状态操作。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/nodes/pve1/qemu" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"data": [{"vmid": 200, "name": "ext-vm", "status": "stopped"}]}`)
		case strings.HasSuffix(r.URL.Path, "/start") || strings.HasSuffix(r.URL.Path, "/stop") || strings.HasSuffix(r.URL.Path, "/reboot"):
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmstart:200:root@pam:"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	svcAdmin := newSvc(&fakeVMRepository{}, ts)
	if err := svcAdmin.Start(ctx, "ext-3-200", adminIdentity()); err != nil {
		t.Fatalf("admin start external: %v", err)
	}

	// ext- 指向已托管行且归属自身：放行（路由本地行流程，G1）。
	managed := &model.VM{ID: 5, NodeID: 3, PVEVmid: 200, Name: "managed"}
	uid := int64(2)
	managed.UserID = &uid
	vmRepo := &fakeVMRepository{getByNodeVMID: managed, get: &repository.VMWithIP{VM: *managed, IP: "10.0.0.5"}}
	svcOwned := newSvc(vmRepo, ts)
	if err := svcOwned.Start(ctx, "ext-3-200", userIdentity(2)); err != nil {
		t.Fatalf("user start managed own vm: %v", err)
	}

	// ext- 指向已托管行但归属他人：403。
	managed2 := &model.VM{ID: 6, NodeID: 3, PVEVmid: 200, Name: "managed2"}
	uid3 := int64(3)
	managed2.UserID = &uid3
	vmRepo2 := &fakeVMRepository{getByNodeVMID: managed2, get: &repository.VMWithIP{VM: *managed2}}
	svcOther := newSvc(vmRepo2, noCall)
	if err := svcOther.Start(ctx, "ext-3-200", userIdentity(2)); !isKind(err, KindForbidden) {
		t.Fatalf("user start managed other's vm err = %v, want KindForbidden", err)
	}
}

// TestDestroyOwnership 覆盖销毁的归属校验：用户销毁归属自己的 VM -> 放行
// （本地清理执行）；他人/无主 -> 403；ext- 对用户 -> 403（不触碰 PVE）。
func TestDestroyOwnership(t *testing.T) {
	ctx := context.Background()
	uid := int64(2)

	// 归属自身：本地销毁流程完整执行。
	own := provisionedVM()
	own.VM.UserID = &uid
	vmRepo := &fakeVMRepository{get: own}
	ipRepo := &fakeVMIPPoolRepository{}
	opRepo := &fakeVMOperationRepository{}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
	}}
	svc := newVMServiceWithOps(t, opRepo, vmRepo, ipRepo, &fakeVMZoneRepository{}, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	// 无 PVE 调用：pve_vmid > 0 会触发 PVE 调用，置 0 走纯本地清理路径。
	own.VM.PVEVmid = 0
	if err := svc.Destroy(ctx, "1", userIdentity(2)); err != nil {
		t.Fatalf("user destroy own vm: %v", err)
	}
	if len(ipRepo.released) != 1 || len(vmRepo.deleted) != 1 {
		t.Fatalf("released = %v deleted = %v, want local cleanup", ipRepo.released, vmRepo.deleted)
	}
	if len(opRepo.ops) != 1 || opRepo.ops[0].OperatorType != RoleUser ||
		opRepo.ops[0].OperatorID == nil || *opRepo.ops[0].OperatorID != 2 {
		t.Fatalf("destroy operator = %+v, want user/2", opRepo.ops)
	}

	// 他人 VM -> 403。
	other := provisionedVM()
	uid3 := int64(3)
	other.VM.UserID = &uid3
	svc2 := newVMService(t, &fakeVMRepository{get: other}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if err := svc2.Destroy(ctx, "1", userIdentity(2)); !isKind(err, KindForbidden) {
		t.Fatalf("user destroy other's vm err = %v, want KindForbidden", err)
	}

	// 无主 VM -> 403。
	svc3 := newVMService(t, &fakeVMRepository{get: provisionedVM()}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if err := svc3.Destroy(ctx, "1", userIdentity(2)); !isKind(err, KindForbidden) {
		t.Fatalf("user destroy unowned vm err = %v, want KindForbidden", err)
	}

	// ext- 对用户 -> 403（不触碰 PVE）。
	noCall := noCallServer(t)
	defer noCall.Close()
	svc4 := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{nodes: []model.PVENode{{ID: 3, ZoneID: 1, Name: "pve1", Enabled: true}}},
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc4.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(noCall.URL), pve.WithHTTPClient(noCall.Client()), pve.WithTimeout(5*time.Second))
	}
	if err := svc4.Destroy(ctx, "ext-3-200", userIdentity(2)); !isKind(err, KindForbidden) {
		t.Fatalf("user destroy external err = %v, want KindForbidden", err)
	}
}

// TestResizeOwnership 覆盖规格调整的归属校验：用户调整他人的 VM -> 403，
// 且不发起任何 PVE 调用；调整自己的 VM -> 放行。
func TestResizeOwnership(t *testing.T) {
	ctx := context.Background()
	ts := noCallServer(t)
	defer ts.Close()

	uid3 := int64(3)
	other := provisionedVM()
	other.VM.UserID = &uid3
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
	}}
	svc := newVMService(t, &fakeVMRepository{get: other}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	cpu := 4
	if _, err := svc.Resize(ctx, 1, userIdentity(2), &cpu, nil, nil); !isKind(err, KindForbidden) {
		t.Fatalf("user resize other's vm err = %v, want KindForbidden", err)
	}

	// 归属自身：放行（PVE resize 服务器应答）。
	uid := int64(2)
	own := provisionedVM()
	own.VM.UserID = &uid
	rts := resizeServer(t)
	defer rts.Close()
	svc2 := newVMService(t, &fakeVMRepository{get: own}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc2.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(rts.URL), pve.WithHTTPClient(rts.Client()), pve.WithTimeout(5*time.Second))
	}
	if _, err := svc2.Resize(ctx, 1, userIdentity(2), &cpu, nil, nil); err != nil {
		t.Fatalf("user resize own vm: %v", err)
	}
}

// ---------- 详情归属校验（任务 6.5，设计 D5） ----------

// TestGetVMIdentityScoping 覆盖详情接口的身份分流：用户查看归属自己的 VM
// -> 200；他人/无主 VM -> 403；ext- 对用户 -> 403；ext- 指向已托管且归属
// 自身的 VM -> 本地形态放行（G1）。
func TestGetVMIdentityScoping(t *testing.T) {
	ctx := context.Background()
	noCall := noCallServer(t)
	defer noCall.Close()
	newClient := func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("x", apiUser, apiTokenSecret,
			pve.WithBaseURL(noCall.URL), pve.WithHTTPClient(noCall.Client()), pve.WithTimeout(5*time.Second))
	}

	// 归属自身（pve_vmid=0 -> creating，无需 PVE）。
	uid := int64(2)
	own := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 0, UserID: &uid, Source: model.VMSourceSparkCreated}, IP: "10.0.0.5"}
	svc := newVMService(t, &fakeVMRepository{get: own}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	item, err := svc.GetVM(ctx, "1", userIdentity(2))
	if err != nil {
		t.Fatalf("user get own vm: %v", err)
	}
	if item.Status != model.VMStateCreating {
		t.Fatalf("item = %+v, want creating", item)
	}

	// 他人 VM -> 403。
	uid3 := int64(3)
	other := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 0, UserID: &uid3}}
	svc2 := newVMService(t, &fakeVMRepository{get: other}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if _, err := svc2.GetVM(ctx, "1", userIdentity(2)); !isKind(err, KindForbidden) {
		t.Fatalf("user get other's vm err = %v, want KindForbidden", err)
	}

	// 无主 VM -> 403（用户视角视同不存在）。
	unowned := &repository.VMWithIP{VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 0}}
	svc3 := newVMService(t, &fakeVMRepository{get: unowned}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if _, err := svc3.GetVM(ctx, "1", userIdentity(2)); !isKind(err, KindForbidden) {
		t.Fatalf("user get unowned vm err = %v, want KindForbidden", err)
	}
	// 管理员查看无主 VM：放行（现状不回归）。
	if _, err := svc3.GetVM(ctx, "1", adminIdentity()); err != nil {
		t.Fatalf("admin get unowned vm: %v", err)
	}

	// ext- 对用户 -> 403（不触碰 PVE）。
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h1", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	svc4 := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc4.newClient = newClient
	if _, err := svc4.GetVM(ctx, "ext-1-300", userIdentity(2)); !isKind(err, KindForbidden) {
		t.Fatalf("user get external err = %v, want KindForbidden", err)
	}

	// ext- 指向已托管且归属自身的 VM：本地形态放行（G1）。
	managed := &model.VM{ID: 7, NodeID: 1, PVEVmid: 0, Name: "managed", UserID: &uid, Source: model.VMSourceClaimed}
	vmRepo := &fakeVMRepository{getByNodeVMID: managed, get: &repository.VMWithIP{VM: *managed, IP: "10.0.0.7"}}
	svc5 := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	item, err = svc5.GetVM(ctx, "ext-1-300", userIdentity(2))
	if err != nil {
		t.Fatalf("user get managed own vm: %v", err)
	}
	if item.ExternalID != "" || item.VM.VM.ID != 7 {
		t.Fatalf("item = %+v, want the local form (id 7, no ExternalID)", item)
	}
}

// ---------- 操作记录查询归属校验（任务 6.4/6.6） ----------

// TestListOperationsIdentityScoping 覆盖操作记录查询的身份分流：用户查询
// 归属自己的 VM -> 放行；他人 VM -> 403；ext- 对用户 -> 403。
func TestListOperationsIdentityScoping(t *testing.T) {
	ctx := context.Background()
	uid := int64(2)
	own := provisionedVM()
	own.VM.UserID = &uid
	opRepo := &fakeVMOperationRepository{}
	svc := newVMServiceWithOps(t, opRepo, &fakeVMRepository{get: own}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, &fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})

	if _, _, err := svc.ListOperations(ctx, "1", userIdentity(2), 25, 0); err != nil {
		t.Fatalf("user list own operations: %v", err)
	}

	uid3 := int64(3)
	other := provisionedVM()
	other.VM.UserID = &uid3
	svc2 := newVMServiceWithOps(t, opRepo, &fakeVMRepository{get: other}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, &fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if _, _, err := svc2.ListOperations(ctx, "1", userIdentity(2), 25, 0); !isKind(err, KindForbidden) {
		t.Fatalf("user list other's operations err = %v, want KindForbidden", err)
	}

	svc3 := newVMServiceWithOps(t, opRepo, &fakeVMRepository{}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, &fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if _, _, err := svc3.ListOperations(ctx, "ext-3-200", userIdentity(2), 25, 0); !isKind(err, KindForbidden) {
		t.Fatalf("user list external operations err = %v, want KindForbidden", err)
	}
}

// TestOperationRecordsCarryOperator 固定失败操作的记录同样携带操作者
// （任务 6.6，设计 D8）：PVE 拒绝后写入 failed 记录，operator 为 user/2。
func TestOperationRecordsCarryOperator(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/start") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors": {"_": "cannot start"}}`)
	}))
	defer ts.Close()

	uid := int64(2)
	own := provisionedVM()
	own.VM.UserID = &uid
	opRepo := &fakeVMOperationRepository{}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true},
	}}
	svc := newVMServiceWithOps(t, opRepo, &fakeVMRepository{get: own}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	if err := svc.Start(context.Background(), "1", userIdentity(2)); err == nil {
		t.Fatal("Start succeeded, want PVE failure")
	}
	if len(opRepo.ops) != 1 || opRepo.ops[0].Result != model.VMOpResultFailed {
		t.Fatalf("operations = %+v, want 1 failed record", opRepo.ops)
	}
	op := opRepo.ops[0]
	if op.OperatorType != RoleUser || op.OperatorID == nil || *op.OperatorID != 2 {
		t.Fatalf("failed record operator = %q / %v, want user / 2", op.OperatorType, op.OperatorID)
	}
}
