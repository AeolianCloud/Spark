package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
)

// fakeZoneRepository 是供测试使用的可脚本化 ZoneRepository。
type fakeZoneRepository struct {
	zones   []model.Zone
	err     error
	created []model.Zone
}

func (f *fakeZoneRepository) CreateZone(ctx context.Context, name string) (*model.Zone, error) {
	if f.err != nil {
		return nil, f.err
	}
	z := model.Zone{ID: int64(len(f.created) + 1), Name: name}
	f.created = append(f.created, z)
	return &z, nil
}

func (f *fakeZoneRepository) GetZone(ctx context.Context, id int64) (*model.Zone, error) {
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

func (f *fakeZoneRepository) ListZones(ctx context.Context) ([]model.Zone, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.zones, nil
}

func (f *fakeZoneRepository) ListZonesPage(ctx context.Context, limit, offset int) ([]model.Zone, error) {
	if f.err != nil {
		return nil, f.err
	}
	return slicePage(f.zones, limit, offset), nil
}

func (f *fakeZoneRepository) CountZones(ctx context.Context) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return len(f.zones), nil
}

// fakeNodeRepository 是供测试使用的可脚本化 NodeRepository。
type fakeNodeRepository struct {
	nodes []model.PVENode
	err   error
}

func (f *fakeNodeRepository) CreateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	node.ID = int64(len(f.nodes) + 1)
	f.nodes = append(f.nodes, node)
	n := node
	return &n, nil
}

func (f *fakeNodeRepository) GetNode(ctx context.Context, id int64) (*model.PVENode, error) {
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

func (f *fakeNodeRepository) ListNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
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

func (f *fakeNodeRepository) ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
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

func (f *fakeNodeRepository) UpdateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.nodes {
		if f.nodes[i].ID == node.ID {
			f.nodes[i] = node
			n := node
			return &n, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func TestCreateZone(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{{ID: 1, Name: "cn-east-1"}}}
	svc := NewZoneService(zoneRepo, &fakeNodeRepository{})

	if _, err := svc.CreateZone(context.Background(), "  "); err == nil {
		t.Fatal("empty name: want error")
	} else if err.(*Error).Kind != KindBadRequest {
		t.Fatalf("empty name: kind = %v, want KindBadRequest", err.(*Error).Kind)
	}

	if _, err := svc.CreateZone(context.Background(), "cn-east-1"); err == nil {
		t.Fatal("duplicate name: want error")
	} else if err.(*Error).Kind != KindConflict {
		t.Fatalf("duplicate name: kind = %v, want KindConflict", err.(*Error).Kind)
	}

	z, err := svc.CreateZone(context.Background(), "cn-north-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if z.Name != "cn-north-1" || z.ID != 1 {
		t.Fatalf("unexpected zone: %+v", z)
	}
}

func TestListZonesIncludesNodes(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{{ID: 1, Name: "cn-east-1"}}}
	nodeRepo := &fakeNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "10.0.0.10", Enabled: true},
		{ID: 2, ZoneID: 1, Name: "pve2", Host: "10.0.0.11", Enabled: false},
	}}
	svc := NewZoneService(zoneRepo, nodeRepo)

	zones, total, err := svc.ListZones(context.Background(), 25, 0)
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	if len(zones) != 1 || len(zones[0].Nodes) != 2 {
		t.Fatalf("unexpected result: %+v", zones)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
}

// TestListZonesPagination 验证页切片：limit/offset 选择区域页（页内的节点
// 列表保持完整），total 始终是区域总数，与页无关。
func TestListZonesPagination(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{
		{ID: 1, Name: "z1"}, {ID: 2, Name: "z2"}, {ID: 3, Name: "z3"},
	}}
	nodeRepo := &fakeNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Enabled: true},
		{ID: 2, ZoneID: 2, Name: "pve2", Enabled: true},
		{ID: 3, ZoneID: 3, Name: "pve3", Enabled: true},
	}}
	svc := NewZoneService(zoneRepo, nodeRepo)

	page, total, err := svc.ListZones(context.Background(), 2, 1)
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(page) != 2 || page[0].Zone.ID != 2 || page[1].Zone.ID != 3 {
		t.Fatalf("page = %+v, want zones 2 and 3", page)
	}
	if len(page[0].Nodes) != 1 || page[0].Nodes[0].Name != "pve2" {
		t.Fatalf("page zone 2 nodes = %+v, want the complete node list", page[0].Nodes)
	}

	empty, _, err := svc.ListZones(context.Background(), 25, 10)
	if err != nil {
		t.Fatalf("ListZones past the end: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("page past the end = %+v, want empty", empty)
	}
}

func TestCreateNodeValidation(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{{ID: 1, Name: "cn-east-1"}}}
	nodeRepo := &fakeNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "10.0.0.10", APIUser: "root@pam!spark", APITokenSecret: "s1", Enabled: true},
	}}
	svc := NewZoneService(zoneRepo, nodeRepo)
	// 登记成功路径会探测 PVE 集群节点名（任务 4.1）：桩化为返回后续创建
	// 的业务名，避免打真实网络；失败断言（未知区域/重名/缺字段）在探测之前
	// 就已终止。
	svc.probeNodes = func(ctx context.Context, host string, port int, apiUser, apiTokenSecret string) ([]string, error) {
		return []string{"pve9", "pve10"}, nil
	}

	// 未知区域 -> not_found。
	if _, err := svc.CreateNode(context.Background(), 99, "pve9", "10.0.0.9", "root@pam!spark", "t", nil); err == nil {
		t.Fatal("unknown zone: want error")
	} else if err.(*Error).Kind != KindNotFound {
		t.Fatalf("unknown zone: kind = %v, want KindNotFound", err.(*Error).Kind)
	}

	// 区域内重名 -> conflict。
	if _, err := svc.CreateNode(context.Background(), 1, "pve1", "10.0.0.9", "root@pam!spark", "t", nil); err == nil {
		t.Fatal("duplicate name: want error")
	} else if err.(*Error).Kind != KindConflict {
		t.Fatalf("duplicate name: kind = %v, want KindConflict", err.(*Error).Kind)
	}

	// 缺少 host -> bad_request。
	if _, err := svc.CreateNode(context.Background(), 1, "pve9", " ", "root@pam!spark", "t", nil); err == nil {
		t.Fatal("empty host: want error")
	} else if err.(*Error).Kind != KindBadRequest {
		t.Fatalf("empty host: kind = %v, want KindBadRequest", err.(*Error).Kind)
	}

	// 缺少 api_user -> bad_request。
	if _, err := svc.CreateNode(context.Background(), 1, "pve9", "10.0.0.9", "", "t", nil); err == nil {
		t.Fatal("empty api_user: want error")
	} else if err.(*Error).Kind != KindBadRequest {
		t.Fatalf("empty api_user: kind = %v, want KindBadRequest", err.(*Error).Kind)
	}

	// 成功：省略时 enabled 默认为 true。
	n, err := svc.CreateNode(context.Background(), 1, "pve9", "10.0.0.9", "root@pam!spark", "tok", nil)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if !n.Enabled || n.APITokenSecret != "tok" || n.ZoneID != 1 {
		t.Fatalf("unexpected node: %+v", n)
	}

	// enabled=false 会被采纳。
	n, err = svc.CreateNode(context.Background(), 1, "pve10", "10.0.0.10", "root@pam!spark", "tok", boolPtr(false))
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if n.Enabled {
		t.Fatalf("node should be disabled: %+v", n)
	}
}

func TestUpdateNode(t *testing.T) {
	nodeRepo := &fakeNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "10.0.0.10", APIUser: "root@pam!spark", APITokenSecret: "old-secret", Enabled: true},
	}}
	svc := NewZoneService(&fakeZoneRepository{}, nodeRepo)
	// host 变化会触发集群节点名探测（任务 4.2）：桩化为返回业务名 pve1，
	// 避免打真实网络；重名/未知节点的失败断言在探测之前就已终止。
	svc.probeNodes = func(ctx context.Context, host string, port int, apiUser, apiTokenSecret string) ([]string, error) {
		return []string{"pve1"}, nil
	}

	// 空的 api_token 保留已存储的密钥。
	n, tokenChanged, err := svc.UpdateNode(context.Background(), 1, "pve1", "", "10.0.0.20", "root@pam!spark", "", nil)
	if err != nil {
		t.Fatalf("update node: %v", err)
	}
	if tokenChanged {
		t.Fatal("tokenChanged = true, want false")
	}
	if n.APITokenSecret != "old-secret" {
		t.Fatalf("secret changed to %q, want %q", n.APITokenSecret, "old-secret")
	}
	if n.Host != "10.0.0.20" {
		t.Fatalf("host = %q, want 10.0.0.20", n.Host)
	}

	// host 携带 :port 后缀 -> 更新后 Port 生效，host 只存纯地址。
	n, tokenChanged, err = svc.UpdateNode(context.Background(), 1, "pve1", "", "10.0.0.5:9001", "root@pam!spark", "", nil)
	if err != nil {
		t.Fatalf("update node with port: %v", err)
	}
	if tokenChanged {
		t.Fatal("tokenChanged = true, want false")
	}
	if n.Port != 9001 || n.Host != "10.0.0.5" {
		t.Fatalf("node = %+v, want port=9001 host=10.0.0.5", n)
	}

	// 提供的 api_token 会替换密钥。
	n, tokenChanged, err = svc.UpdateNode(context.Background(), 1, "pve1", "", "10.0.0.20", "root@pam!spark", "new-secret", nil)
	if err != nil {
		t.Fatalf("update node: %v", err)
	}
	if !tokenChanged || n.APITokenSecret != "new-secret" {
		t.Fatalf("token not replaced: changed=%v secret=%q", tokenChanged, n.APITokenSecret)
	}

	// 重命名为已有名称 -> conflict。
	nodeRepo.nodes = append(nodeRepo.nodes, model.PVENode{ID: 2, ZoneID: 1, Name: "pve2", Host: "10.0.0.30"})
	if _, _, err := svc.UpdateNode(context.Background(), 1, "pve2", "", "10.0.0.20", "root@pam!spark", "", nil); err == nil {
		t.Fatal("rename to existing name: want error")
	} else if err.(*Error).Kind != KindConflict {
		t.Fatalf("rename: kind = %v, want KindConflict", err.(*Error).Kind)
	}

	// 未知节点 -> not_found。
	if _, _, err := svc.UpdateNode(context.Background(), 99, "pve9", "", "10.0.0.9", "root@pam!spark", "", nil); err == nil {
		t.Fatal("unknown node: want error")
	} else if err.(*Error).Kind != KindNotFound {
		t.Fatalf("unknown node: kind = %v, want KindNotFound", err.(*Error).Kind)
	}
}

func TestListNodesByZone(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{{ID: 1, Name: "cn-east-1"}}}
	nodeRepo := &fakeNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "10.0.0.10", Enabled: true},
		{ID: 2, ZoneID: 2, Name: "other", Host: "10.0.0.20", Enabled: true},
	}}
	svc := NewZoneService(zoneRepo, nodeRepo)

	nodes, err := svc.ListNodesByZone(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListNodesByZone: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "pve1" {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}

	if _, err := svc.ListNodesByZone(context.Background(), 99); err == nil {
		t.Fatal("unknown zone: want error")
	} else if err.(*Error).Kind != KindNotFound {
		t.Fatalf("unknown zone: kind = %v, want KindNotFound", err.(*Error).Kind)
	}
}

// reachablePVEServer 以有效的 PVE 信封应答 GET /version。
func reachablePVEServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"release":"8.2","repoid":"xyz","version":"8.2.0"}}`))
	}))
}

func unreachablePVEServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"errors":{"root@pam":"permission denied"}}`))
	}))
}

func TestSelectReachableNodePicksFirstReachable(t *testing.T) {
	dead := unreachablePVEServer()
	defer dead.Close()
	alive := reachablePVEServer()
	defer alive.Close()

	// 按节点 host 键控的逐候选客户端工厂：节点 1 探测死服务器，节点 2 探测
	// 活服务器。
	servers := map[string]*httptest.Server{"h1": dead, "h2": alive}
	nodes := []model.PVENode{
		{ID: 1, Name: "dead", Host: "h1", APIUser: "root@pam!probe", APITokenSecret: "secret123"},
		{ID: 2, Name: "alive", Host: "h2", APIUser: "root@pam!probe", APITokenSecret: "secret123"},
	}
	newClient := func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		srv := servers[host]
		return pve.NewClient("localhost", apiUser, apiTokenSecret,
			pve.WithBaseURL(srv.URL), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(2*time.Second))
	}

	got, err := selectReachableNode(context.Background(), nodes, newClient)
	if err != nil {
		t.Fatalf("SelectReachableNode: %v", err)
	}
	if got.ID != 2 {
		t.Fatalf("node = %+v, want the second candidate", got)
	}
}

func TestSelectReachableNodeAllFail(t *testing.T) {
	dead := unreachablePVEServer()
	defer dead.Close()
	nodes := []model.PVENode{
		{ID: 1, Name: "a", Host: "h1", APIUser: "root@pam!probe", APITokenSecret: "secret123"},
		{ID: 2, Name: "b", Host: "h2", APIUser: "root@pam!probe", APITokenSecret: "secret123"},
	}
	newClient := func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("localhost", apiUser, apiTokenSecret,
			pve.WithBaseURL(dead.URL), pve.WithHTTPClient(dead.Client()), pve.WithTimeout(2*time.Second))
	}
	_, err := selectReachableNode(context.Background(), nodes, newClient)
	if err == nil {
		t.Fatal("all nodes dead: want error")
	}
	var serr *Error
	if !errors.As(err, &serr) || serr.Kind != KindNodeUnavailable {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}
}

func TestSelectReachableNodeEmptyCandidates(t *testing.T) {
	_, err := SelectReachableNode(context.Background(), nil)
	var serr *Error
	if !errors.As(err, &serr) || serr.Kind != KindNodeUnavailable {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}
}

// TestCreateNodePortParsing 验证 CreateNode 对 host 中 :port 后缀的解析：
// 带端口登记成功且 node.Port 正确、存储的 host 不含端口；无端口默认 8006；
// 非法端口（超范围、非数字、IPv6 多冒号）与以 / 开头的裸路径地址均以
// badRequest 拒绝且不落库。
func TestCreateNodePortParsing(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{{ID: 1, Name: "cn-east-1"}}}
	nodeRepo := &fakeNodeRepository{}
	svc := NewZoneService(zoneRepo, nodeRepo)
	// 成功登记路径会探测 PVE 集群节点名（任务 4.1）：桩化为返回后续创建
	// 的业务名，避免打真实网络；非法 host 在解析阶段即被拒绝，不会探测。
	svc.probeNodes = func(ctx context.Context, host string, port int, apiUser, apiTokenSecret string) ([]string, error) {
		return []string{"pve-port", "pve-default"}, nil
	}

	// host 带端口 -> 登记成功，Port=8007，host 只存纯地址。
	n, err := svc.CreateNode(context.Background(), 1, "pve-port", "117.177.33.8:8007", "root@pam!spark", "t", nil)
	if err != nil {
		t.Fatalf("create node with port: %v", err)
	}
	if n.Port != 8007 || n.Host != "117.177.33.8" {
		t.Fatalf("node = %+v, want port=8007 host=117.177.33.8", n)
	}

	// host 无端口 -> 默认 8006。
	n, err = svc.CreateNode(context.Background(), 1, "pve-default", "117.177.33.9", "root@pam!spark", "t", nil)
	if err != nil {
		t.Fatalf("create node without port: %v", err)
	}
	if n.Port != defaultNodePort || n.Host != "117.177.33.9" {
		t.Fatalf("node = %+v, want port=%d host=117.177.33.9", n, defaultNodePort)
	}

	// 非法输入 -> bad_request，且不落库（仓库中仍只有两个成功节点）。
	for _, host := range []string{"117.177.33.8:99999", "117.177.33.8:abc", "117.177.33.8:0", "117.177.33.8:", "117.177.33.8/", "::1", "/host:8007", "https:///host:8007", "/host"} {
		if _, err := svc.CreateNode(context.Background(), 1, "pve-bad", host, "root@pam!spark", "t", nil); err == nil {
			t.Fatalf("host %q: want error", host)
		} else {
			var serr *Error
			if !errors.As(err, &serr) || serr.Kind != KindBadRequest {
				t.Fatalf("host %q: err = %v, want KindBadRequest", host, err)
			}
		}
	}
	if len(nodeRepo.nodes) != 2 {
		t.Fatalf("persisted nodes = %d, want 2 (rejected hosts must not be saved)", len(nodeRepo.nodes))
	}
}

// TestCreateNodeRejectsClusterNameMismatch 验证业务名与 PVE 集群真实节点名
// 不一致时登记被拒（任务 4.1）：错误消息列出集群真实节点名且不落库；探测
// 本身失败（不可达/401 等）同样以 node_unavailable 拒绝，绝不静默落库。
func TestCreateNodeRejectsClusterNameMismatch(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{{ID: 1, Name: "cn-east-1"}}}
	nodeRepo := &fakeNodeRepository{}
	svc := NewZoneService(zoneRepo, nodeRepo)

	// 业务名 "aeolian" 不在集群节点名列表中 -> 拒绝并提示真实名 aeoliancloud。
	svc.probeNodes = func(ctx context.Context, host string, port int, apiUser, apiTokenSecret string) ([]string, error) {
		return []string{"aeoliancloud"}, nil
	}
	_, err := svc.CreateNode(context.Background(), 1, "aeolian", "117.177.33.8", "root@pam!spark", "t", nil)
	if err == nil {
		t.Fatal("mismatched name: want error")
	}
	var serr *Error
	if !errors.As(err, &serr) || serr.Kind != KindNodeUnavailable {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}
	if !strings.Contains(err.Error(), "aeoliancloud") {
		t.Fatalf("err = %q, want the cluster node name in the message", err)
	}
	if len(nodeRepo.nodes) != 0 {
		t.Fatalf("persisted nodes = %d, want 0 (rejected node must not be saved)", len(nodeRepo.nodes))
	}

	// 探测失败（PVE 不可达/401 等）-> 同样拒绝且不落库。
	svc.probeNodes = func(ctx context.Context, host string, port int, apiUser, apiTokenSecret string) ([]string, error) {
		return nil, errors.New("connection refused")
	}
	_, err = svc.CreateNode(context.Background(), 1, "aeoliancloud", "117.177.33.8", "root@pam!spark", "t", nil)
	if err == nil {
		t.Fatal("probe failure: want error")
	}
	if !errors.As(err, &serr) || serr.Kind != KindNodeUnavailable {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}
	if len(nodeRepo.nodes) != 0 {
		t.Fatalf("persisted nodes = %d, want 0 (probe failure must not save)", len(nodeRepo.nodes))
	}
}

func boolPtr(b bool) *bool { return &b }
