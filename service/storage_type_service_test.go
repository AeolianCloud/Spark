package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
	"spark/repository"
)

// fakeStorageTypeRepo 是供 StorageTypeService 测试使用的可脚本化
// StorageTypeRepository：内存行 + upsert/delete 调用计数。
type fakeStorageTypeRepo struct {
	rows         []model.StorageType
	nextID       int64
	upsertCalls  int
	deleteCalls  int
	deleteErrors map[int64]error
}

func (f *fakeStorageTypeRepo) UpsertByZonePveStorage(ctx context.Context, zoneID int64, pveStorage, stype, content string, nodes []string) (*model.StorageType, bool, error) {
	f.upsertCalls++
	// 与真实 repo 的落库-读回一致：nil 一律归一为空切片（空 = 不限制节点）。
	if nodes == nil {
		nodes = []string{}
	}
	for i := range f.rows {
		if f.rows[i].ZoneID == zoneID && f.rows[i].PVEStorage == pveStorage {
			// 扫描更新语义：仅覆盖 type/content/nodes 快照，保留 name/enabled。
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

func (f *fakeStorageTypeRepo) UpdateMeta(ctx context.Context, id int64, name *string, enabled *bool) (*model.StorageType, error) {
	for i := range f.rows {
		if f.rows[i].ID == id {
			if name != nil {
				// NULLIF('', ...) 语义：空串置 NULL。
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

func (f *fakeStorageTypeRepo) ListPage(ctx context.Context, zoneID *int64, limit, offset int) ([]model.StorageType, error) {
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

func (f *fakeStorageTypeRepo) Count(ctx context.Context, zoneID *int64) (int, error) {
	n := 0
	for _, r := range f.rows {
		if zoneID == nil || r.ZoneID == *zoneID {
			n++
		}
	}
	return n, nil
}

func (f *fakeStorageTypeRepo) Get(ctx context.Context, id int64) (*model.StorageType, error) {
	for i := range f.rows {
		if f.rows[i].ID == id {
			return &f.rows[i], nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeStorageTypeRepo) Delete(ctx context.Context, id int64) error {
	f.deleteCalls++
	if f.deleteErrors != nil {
		if err, ok := f.deleteErrors[id]; ok {
			return err
		}
	}
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return pgx.ErrNoRows
}

// fakeStorageTypeNodeRepo 是供测试使用的可脚本化 StorageTypeNodeRepository。
type fakeStorageTypeNodeRepo struct {
	nodes []model.PVENode
	err   error
}

func (f *fakeStorageTypeNodeRepo) ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
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

// fakeStorageTypeZoneRepo 是供测试使用的可脚本化 StorageTypeZoneRepository：
// zone 存在性校验（SyncZone 开头）的替身。
type fakeStorageTypeZoneRepo struct {
	zones []model.Zone
}

func (f *fakeStorageTypeZoneRepo) GetZone(ctx context.Context, id int64) (*model.Zone, error) {
	for i := range f.zones {
		if f.zones[i].ID == id {
			z := f.zones[i]
			return &z, nil
		}
	}
	return nil, pgx.ErrNoRows
}

// storageTypeScanServer 应答集群级 GET /storage 的测试服务器；storage 为
// nil 时以 500 拒绝（模拟 PVE 错误）。
func storageTypeScanServer(t *testing.T, storages []pve.PVEStorage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/storage" {
			http.NotFound(w, r)
			return
		}
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
	}))
}

// newStorageTypeService 装配 StorageTypeService：节点选择器固定返回第一个
// 候选，PVE 客户端指向扫描测试服务器。zoneRepo 提供 zone 1（供 SyncZone
// 的存在性校验放行），zone 2 等其他 zone 视测试需要自行补充。
func newStorageTypeService(t *testing.T, repo *fakeStorageTypeRepo, nodeRepo *fakeStorageTypeNodeRepo, ts *httptest.Server) *StorageTypeService {
	t.Helper()
	svc := NewStorageTypeService(repo, nodeRepo, &fakeStorageTypeZoneRepo{zones: []model.Zone{{ID: 1, Name: "z1"}}})
	svc.selectNode = func(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
		if len(nodes) == 0 {
			return model.PVENode{}, nodeUnavailablef("no reachable node among 0 candidate(s)")
		}
		return nodes[0], nil
	}
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	return svc
}

func storageTypeRow(id int64, zoneID int64, name *string, pveStorage string, enabled bool, typ, content string) model.StorageType {
	return model.StorageType{
		ID: id, ZoneID: zoneID, Name: name, PVEStorage: pveStorage,
		Enabled: enabled, Type: &typ, Content: &content, CreatedAt: time.Now(),
	}
}

// TestSyncZoneUpsertsAndDeletes 覆盖扫描同步的完整语义（设计 D1/D3）：
// 新建（默认启用、name 为空）、更新（仅刷新 type/content）、删除 PVE 已
// 消失的存储、被 VM 引用（ErrInUse）的消失存储跳过并计入 skipped，摘要
// 计数正确。
func TestSyncZoneUpsertsAndDeletes(t *testing.T) {
	ts := storageTypeScanServer(t, []pve.PVEStorage{
		{Storage: "local", Type: "dir", Content: "images", Shared: false, Nodes: []string{"pve1"}},
		{Storage: "local-lvm", Type: "lvm", Content: "images,rootdir", Shared: false, Nodes: []string{"pve1"}},
	})
	defer ts.Close()

	name := "业务名"
	repo := &fakeStorageTypeRepo{
		nextID: 10,
		rows: []model.StorageType{
			storageTypeRow(1, 1, &name, "local", true, "dir", "iso"),    // 已存在：type/content 将被刷新
			storageTypeRow(2, 1, nil, "gone", true, "dir", "iso"),       // PVE 已消失：将删除
			storageTypeRow(3, 1, nil, "referenced", true, "dir", "iso"), // PVE 已消失但被 VM 引用：跳过
			storageTypeRow(4, 2, nil, "local", true, "dir", "images"),   // 其他 zone：不参与
		},
		deleteErrors: map[int64]error{3: repository.ErrInUse},
	}
	nodeRepo := &fakeStorageTypeNodeRepo{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam!probe", APITokenSecret: "secret123", Enabled: true},
	}}
	svc := newStorageTypeService(t, repo, nodeRepo, ts)

	summary, err := svc.SyncZone(context.Background(), 1)
	if err != nil {
		t.Fatalf("SyncZone: %v", err)
	}
	want := ScanSummary{Created: 1, Updated: 1, Deleted: 1, Skipped: 1}
	if summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}

	// local 被刷新为扫描快照，name/enabled 保留。
	var local *model.StorageType
	for i := range repo.rows {
		if repo.rows[i].PVEStorage == "local" && repo.rows[i].ZoneID == 1 {
			local = &repo.rows[i]
		}
	}
	if local == nil || local.Content == nil || *local.Content != "images" || local.Type == nil || *local.Type != "dir" {
		t.Fatalf("local after sync = %+v, want refreshed type/content", local)
	}
	if local.Name == nil || *local.Name != "业务名" || !local.Enabled {
		t.Fatalf("local after sync = %+v, want preserved name/enabled", local)
	}
	// nodes 快照随扫描刷新（PVE 应答的节点挂载列表）。
	if len(local.Nodes) != 1 || local.Nodes[0] != "pve1" {
		t.Fatalf("local nodes after sync = %v, want [pve1]", local.Nodes)
	}

	// local-lvm 新建：默认启用、name 为空。
	var lvm *model.StorageType
	for i := range repo.rows {
		if repo.rows[i].PVEStorage == "local-lvm" && repo.rows[i].ZoneID == 1 {
			lvm = &repo.rows[i]
		}
	}
	if lvm == nil || lvm.Name != nil || !lvm.Enabled || lvm.Content == nil || *lvm.Content != "images,rootdir" {
		t.Fatalf("local-lvm after sync = %+v, want created enabled with empty name", lvm)
	}
	if len(lvm.Nodes) != 1 || lvm.Nodes[0] != "pve1" {
		t.Fatalf("local-lvm nodes after sync = %v, want [pve1]", lvm.Nodes)
	}

	// gone 删除、referenced 保留（skipped）、其他 zone 行不受影响。
	if len(repo.rows) != 4 {
		t.Fatalf("rows = %d, want 4 (3 local rows with 1 deleted + 1 created, plus zone 2 row)", len(repo.rows))
	}
	for _, r := range repo.rows {
		if r.PVEStorage == "gone" {
			t.Fatalf("gone storage still present: %+v", r)
		}
		if r.PVEStorage == "referenced" && r.ZoneID == 1 {
			// 保留（被引用）
		}
	}
}

// TestSyncZoneNodesSnapshot 覆盖节点挂载快照的同步语义（设计 D8）：
// PVE 声明的节点列表（含空 = 不限制）原样快照；节点挂载变更后重扫更新
// 快照，且不触碰管理员的 name/enabled。
func TestSyncZoneNodesSnapshot(t *testing.T) {
	ts := storageTypeScanServer(t, []pve.PVEStorage{
		{Storage: "local", Type: "dir", Content: "images", Nodes: []string{"pve1", "pve2"}},
		{Storage: "unlimited", Type: "dir", Content: "images", Nodes: nil},
	})
	defer ts.Close()

	name := "业务名"
	repo := &fakeStorageTypeRepo{
		nextID: 10,
		rows:   []model.StorageType{storageTypeRow(1, 1, &name, "local", true, "dir", "images")},
	}
	nodeRepo := &fakeStorageTypeNodeRepo{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam!probe", APITokenSecret: "secret123", Enabled: true},
	}}
	svc := newStorageTypeService(t, repo, nodeRepo, ts)

	summary, err := svc.SyncZone(context.Background(), 1)
	if err != nil {
		t.Fatalf("SyncZone: %v", err)
	}
	if summary.Created != 1 || summary.Updated != 1 {
		t.Fatalf("summary = %+v, want created=1 updated=1", summary)
	}

	// local：多节点挂载快照。
	var local *model.StorageType
	for i := range repo.rows {
		if repo.rows[i].PVEStorage == "local" && repo.rows[i].ZoneID == 1 {
			local = &repo.rows[i]
		}
	}
	if local == nil || len(local.Nodes) != 2 || local.Nodes[0] != "pve1" || local.Nodes[1] != "pve2" {
		t.Fatalf("local nodes = %v, want [pve1 pve2]", local.Nodes)
	}

	// unlimited：nodes 缺失（空 = 不限制节点）落空切片（非 nil）。
	var unlimited *model.StorageType
	for i := range repo.rows {
		if repo.rows[i].PVEStorage == "unlimited" && repo.rows[i].ZoneID == 1 {
			unlimited = &repo.rows[i]
		}
	}
	if unlimited == nil || unlimited.Nodes == nil || len(unlimited.Nodes) != 0 {
		t.Fatalf("unlimited nodes = %v, want non-nil empty slice", unlimited.Nodes)
	}

	// 节点挂载变更（pve2 摘挂）后重扫：快照更新，name/enabled 保留。
	ts2 := storageTypeScanServer(t, []pve.PVEStorage{
		{Storage: "local", Type: "dir", Content: "images", Nodes: []string{"pve1"}},
		{Storage: "unlimited", Type: "dir", Content: "images", Nodes: []string{"pve2"}},
	})
	defer ts2.Close()
	svc.newClient = func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts2.URL), pve.WithHTTPClient(ts2.Client()), pve.WithTimeout(5*time.Second))
	}
	summary, err = svc.SyncZone(context.Background(), 1)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if summary.Updated != 2 {
		t.Fatalf("rescan summary = %+v, want updated=2", summary)
	}
	for i := range repo.rows {
		if repo.rows[i].PVEStorage == "local" && repo.rows[i].ZoneID == 1 {
			local = &repo.rows[i]
		}
	}
	if len(local.Nodes) != 1 || local.Nodes[0] != "pve1" {
		t.Fatalf("local nodes after rescan = %v, want [pve1]", local.Nodes)
	}
	if local.Name == nil || *local.Name != "业务名" || !local.Enabled {
		t.Fatalf("rescan must preserve name/enabled: %+v", local)
	}
}

// TestSyncZoneEmptyPVEStorage 覆盖 PVE 存储清单为空时的全删语义：本地该
// zone 全部行被删除，其他 zone 不受影响。
func TestSyncZoneEmptyPVEStorage(t *testing.T) {
	ts := storageTypeScanServer(t, []pve.PVEStorage{})
	defer ts.Close()

	repo := &fakeStorageTypeRepo{
		nextID: 10,
		rows: []model.StorageType{
			storageTypeRow(1, 1, nil, "local", true, "dir", "images"),
			storageTypeRow(2, 1, nil, "local-lvm", true, "lvm", "images"),
			storageTypeRow(3, 2, nil, "local", true, "dir", "images"),
		},
	}
	nodeRepo := &fakeStorageTypeNodeRepo{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam!probe", APITokenSecret: "secret123", Enabled: true},
	}}
	svc := newStorageTypeService(t, repo, nodeRepo, ts)

	summary, err := svc.SyncZone(context.Background(), 1)
	if err != nil {
		t.Fatalf("SyncZone: %v", err)
	}
	want := ScanSummary{Deleted: 2}
	if summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if len(repo.rows) != 1 || repo.rows[0].ZoneID != 2 {
		t.Fatalf("rows = %+v, want only the zone-2 row", repo.rows)
	}
}

// TestSyncZoneNoReachableNode 覆盖全部节点不可达时（设计 D1）：返回
// node_unavailable 错误且不产生任何同步（不 upsert、不删除）。
func TestSyncZoneNoReachableNode(t *testing.T) {
	ts := storageTypeScanServer(t, []pve.PVEStorage{{Storage: "local", Type: "dir", Content: "images"}})
	defer ts.Close()

	repo := &fakeStorageTypeRepo{
		nextID: 10,
		rows:   []model.StorageType{storageTypeRow(1, 1, nil, "local", true, "dir", "images")},
	}
	nodeRepo := &fakeStorageTypeNodeRepo{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam!probe", APITokenSecret: "secret123", Enabled: true},
	}}
	svc := newStorageTypeService(t, repo, nodeRepo, ts)
	// 所有节点探测失败（模拟真实不可达）：selectNode 被替换为恒定失败。
	svc.selectNode = func(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
		return model.PVENode{}, nodeUnavailablef("no reachable node among %d candidate(s)", len(nodes))
	}

	_, err := svc.SyncZone(context.Background(), 1)
	var serr *Error
	if !errors.As(err, &serr) || serr.Kind != KindNodeUnavailable {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}
	if repo.upsertCalls != 0 || repo.deleteCalls != 0 {
		t.Fatalf("sync produced side effects despite no reachable node: upserts=%d deletes=%d",
			repo.upsertCalls, repo.deleteCalls)
	}
}

// TestSyncZoneNoEnabledNodes 覆盖 zone 下没有任何启用节点时的失败语义
// （同样的 node_unavailable，且不落库）。
func TestSyncZoneNoEnabledNodes(t *testing.T) {
	ts := storageTypeScanServer(t, []pve.PVEStorage{{Storage: "local", Type: "dir", Content: "images"}})
	defer ts.Close()

	repo := &fakeStorageTypeRepo{nextID: 10}
	nodeRepo := &fakeStorageTypeNodeRepo{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam!probe", APITokenSecret: "secret123", Enabled: false},
	}}
	svc := newStorageTypeService(t, repo, nodeRepo, ts)

	_, err := svc.SyncZone(context.Background(), 1)
	var serr *Error
	if !errors.As(err, &serr) || serr.Kind != KindNodeUnavailable {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}
	if repo.upsertCalls != 0 {
		t.Fatalf("no enabled nodes: upserts = %d, want 0", repo.upsertCalls)
	}
}

// TestSyncZonePVEUpstreamError 覆盖 ListStorage 失败（PVE 拒绝）时整个
// 扫描失败、无任何落库。
func TestSyncZonePVEUpstreamError(t *testing.T) {
	ts := storageTypeScanServer(t, nil) // 500
	defer ts.Close()

	repo := &fakeStorageTypeRepo{nextID: 10}
	nodeRepo := &fakeStorageTypeNodeRepo{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam!probe", APITokenSecret: "secret123", Enabled: true},
	}}
	svc := newStorageTypeService(t, repo, nodeRepo, ts)

	if _, err := svc.SyncZone(context.Background(), 1); err == nil {
		t.Fatal("SyncZone with PVE 500: want error")
	}
	if repo.upsertCalls != 0 {
		t.Fatalf("PVE error: upserts = %d, want 0", repo.upsertCalls)
	}
}

// TestSyncZoneZoneNotFound 覆盖 P3 加固的 zone 存在性校验：zone 不存在时
// 直接返回 not_found（而非 503 node_unavailable），且不产生任何同步。
func TestSyncZoneZoneNotFound(t *testing.T) {
	ts := storageTypeScanServer(t, []pve.PVEStorage{{Storage: "local", Type: "dir", Content: "images"}})
	defer ts.Close()

	repo := &fakeStorageTypeRepo{
		nextID: 10,
		rows:   []model.StorageType{storageTypeRow(1, 99, nil, "local", true, "dir", "images")},
	}
	// 测试服务装配的 zoneRepo 只含 zone 1；对 zone 99 的扫描必须 404。
	svc := newStorageTypeService(t, repo, &fakeStorageTypeNodeRepo{}, ts)

	_, err := svc.SyncZone(context.Background(), 99)
	if !isKind(err, KindNotFound) {
		t.Fatalf("err = %v, want KindNotFound", err)
	}
	if repo.upsertCalls != 0 || repo.deleteCalls != 0 {
		t.Fatalf("zone-not-found scan produced side effects: upserts=%d deletes=%d",
			repo.upsertCalls, repo.deleteCalls)
	}
}

// TestSyncZoneDeleteConcurrentRemovedSkipped 覆盖删除路径的 pgx.ErrNoRows
// 分支：PVE 已消失的存储行在扫描前被并发扫描删除（Delete 返回 ErrNoRows）
// 时计入 skipped 而非报错，其余消失存储照常删除。
func TestSyncZoneDeleteConcurrentRemovedSkipped(t *testing.T) {
	ts := storageTypeScanServer(t, []pve.PVEStorage{})
	defer ts.Close()

	repo := &fakeStorageTypeRepo{
		nextID: 10,
		rows: []model.StorageType{
			storageTypeRow(1, 1, nil, "gone-now", true, "dir", "iso"),    // 并发已删：Delete -> ErrNoRows，跳过
			storageTypeRow(2, 1, nil, "gone-simple", true, "dir", "iso"), // 正常删除
		},
		deleteErrors: map[int64]error{1: pgx.ErrNoRows},
	}
	nodeRepo := &fakeStorageTypeNodeRepo{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam!probe", APITokenSecret: "secret123", Enabled: true},
	}}
	svc := newStorageTypeService(t, repo, nodeRepo, ts)

	summary, err := svc.SyncZone(context.Background(), 1)
	if err != nil {
		t.Fatalf("SyncZone: %v", err)
	}
	want := ScanSummary{Deleted: 1, Skipped: 1}
	if summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if len(repo.rows) != 1 || repo.rows[0].PVEStorage != "gone-now" {
		t.Fatalf("rows = %+v, want only the concurrently-removed row kept", repo.rows)
	}
}

// TestUpdateRejectsOverlongName 覆盖 P4 加固：name trim 后超过 255 字符
// （rune）返回 bad_request（消息含长度上限），恰好 255 字符放行。
func TestUpdateRejectsOverlongName(t *testing.T) {
	name := "ssd"
	repo := &fakeStorageTypeRepo{
		nextID: 1,
		rows:   []model.StorageType{storageTypeRow(1, 1, &name, "local-ssd", true, "dir", "images")},
	}
	svc := NewStorageTypeService(repo, &fakeStorageTypeNodeRepo{}, &fakeStorageTypeZoneRepo{})

	// 256 字符 -> 拒绝。
	tooLong := strings.Repeat("名", maxStorageTypeNameLen+1)
	_, err := svc.Update(context.Background(), 1, &tooLong, nil)
	if !isKind(err, KindBadRequest) {
		t.Fatalf("overlong name err = %v, want KindBadRequest", err)
	}
	if !strings.Contains(err.Error(), "255") {
		t.Fatalf("overlong name message = %q, want it to mention the 255 limit", err.Error())
	}

	// 恰好 255 字符 -> 放行（与 OpenAPI maxLength: 255 一致）。
	exact := strings.Repeat("名", maxStorageTypeNameLen)
	st, err := svc.Update(context.Background(), 1, &exact, nil)
	if err != nil {
		t.Fatalf("exact-255 name: %v", err)
	}
	if st.Name == nil || *st.Name != exact {
		t.Fatalf("exact-255 name stored = %v, want the full name", st.Name)
	}

	// 超长 name 由空白包裹（trim 后超限）同样拒绝。
	spaced := " " + strings.Repeat("x", maxStorageTypeNameLen+1) + " "
	if _, err := svc.Update(context.Background(), 1, &spaced, nil); !isKind(err, KindBadRequest) {
		t.Fatalf("trimmed-overlong name err = %v, want KindBadRequest", err)
	}
}

// TestUpdateMetaSemantics 覆盖 Update 的指针语义：nil 字段不更新、空串
// name 置空（NULL）、enabled 切换、未知 id -> not_found。
func TestUpdateMetaSemantics(t *testing.T) {
	name := "ssd"
	repo := &fakeStorageTypeRepo{
		nextID: 1,
		rows:   []model.StorageType{storageTypeRow(1, 1, &name, "local-ssd", true, "dir", "images")},
	}
	svc := NewStorageTypeService(repo, &fakeStorageTypeNodeRepo{}, &fakeStorageTypeZoneRepo{})

	// 空串 name -> 置空（repo 层 NULLIF 语义：空串写 NULL）。
	empty := ""
	st, err := svc.Update(context.Background(), 1, &empty, nil)
	if err != nil {
		t.Fatalf("Update clear name: %v", err)
	}
	if st.Name != nil {
		t.Fatalf("Update clear name: name = %v, want nil", st.Name)
	}
	if !st.Enabled {
		t.Fatalf("Update clear name: enabled must stay true: %+v", st)
	}

	// 带空白 name -> trim 后应用。
	spaced := " 新名字 "
	st, err = svc.Update(context.Background(), 1, &spaced, nil)
	if err != nil {
		t.Fatalf("Update trimmed name: %v", err)
	}
	if st.Name == nil || *st.Name != "新名字" {
		t.Fatalf("Update trimmed name: name = %v, want 新名字", st.Name)
	}

	// enabled 切换，name 保持。
	disabled := false
	st, err = svc.Update(context.Background(), 1, nil, &disabled)
	if err != nil {
		t.Fatalf("Update disable: %v", err)
	}
	if st.Enabled {
		t.Fatalf("Update disable: enabled = true, want false")
	}
	if st.Name == nil || *st.Name != "新名字" {
		t.Fatalf("Update disable: name must stay: %+v", st.Name)
	}

	// 未知 id -> not_found。
	if _, err := svc.Update(context.Background(), 99, nil, &disabled); !isKind(err, KindNotFound) {
		t.Fatalf("Update missing id: err = %v, want KindNotFound", err)
	}
}

// TestListZoneFilter 覆盖 List 的 zone 过滤透传：zoneID 非 nil 时仅返回
// 该 zone 的行与总数。
func TestListZoneFilter(t *testing.T) {
	repo := &fakeStorageTypeRepo{
		nextID: 10,
		rows: []model.StorageType{
			storageTypeRow(1, 1, nil, "local", true, "dir", "images"),
			storageTypeRow(2, 1, nil, "local-lvm", true, "lvm", "images"),
			storageTypeRow(3, 2, nil, "local", true, "dir", "images"),
		},
	}
	svc := NewStorageTypeService(repo, &fakeStorageTypeNodeRepo{}, &fakeStorageTypeZoneRepo{})

	zone1 := int64(1)
	types, total, err := svc.List(context.Background(), &zone1, 10, 0)
	if err != nil {
		t.Fatalf("List(zone 1): %v", err)
	}
	if total != 2 || len(types) != 2 {
		t.Fatalf("List(zone 1) = %d rows, total %d; want 2/2", len(types), total)
	}
	for _, st := range types {
		if st.ZoneID != 1 {
			t.Fatalf("List(zone 1) leaked zone %d row", st.ZoneID)
		}
	}

	all, totalAll, err := svc.List(context.Background(), nil, 10, 0)
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if totalAll != 3 || len(all) != 3 {
		t.Fatalf("List(all) = %d rows, total %d; want 3/3", len(all), totalAll)
	}
}
