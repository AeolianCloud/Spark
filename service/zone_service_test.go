package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
)

// fakeZoneRepository is a scriptable ZoneRepository for tests.
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

// fakeNodeRepository is a scriptable NodeRepository for tests.
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

	zones, err := svc.ListZones(context.Background())
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	if len(zones) != 1 || len(zones[0].Nodes) != 2 {
		t.Fatalf("unexpected result: %+v", zones)
	}
}

func TestCreateNodeValidation(t *testing.T) {
	zoneRepo := &fakeZoneRepository{zones: []model.Zone{{ID: 1, Name: "cn-east-1"}}}
	nodeRepo := &fakeNodeRepository{nodes: []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", Host: "10.0.0.10", APIUser: "root@pam!spark", APITokenSecret: "s1", Enabled: true},
	}}
	svc := NewZoneService(zoneRepo, nodeRepo)

	// Unknown zone -> not_found.
	if _, err := svc.CreateNode(context.Background(), 99, "pve9", "10.0.0.9", "root@pam!spark", "t", nil); err == nil {
		t.Fatal("unknown zone: want error")
	} else if err.(*Error).Kind != KindNotFound {
		t.Fatalf("unknown zone: kind = %v, want KindNotFound", err.(*Error).Kind)
	}

	// Duplicate name in zone -> conflict.
	if _, err := svc.CreateNode(context.Background(), 1, "pve1", "10.0.0.9", "root@pam!spark", "t", nil); err == nil {
		t.Fatal("duplicate name: want error")
	} else if err.(*Error).Kind != KindConflict {
		t.Fatalf("duplicate name: kind = %v, want KindConflict", err.(*Error).Kind)
	}

	// Missing host -> bad_request.
	if _, err := svc.CreateNode(context.Background(), 1, "pve9", " ", "root@pam!spark", "t", nil); err == nil {
		t.Fatal("empty host: want error")
	} else if err.(*Error).Kind != KindBadRequest {
		t.Fatalf("empty host: kind = %v, want KindBadRequest", err.(*Error).Kind)
	}

	// Missing api_user -> bad_request.
	if _, err := svc.CreateNode(context.Background(), 1, "pve9", "10.0.0.9", "", "t", nil); err == nil {
		t.Fatal("empty api_user: want error")
	} else if err.(*Error).Kind != KindBadRequest {
		t.Fatalf("empty api_user: kind = %v, want KindBadRequest", err.(*Error).Kind)
	}

	// Success: enabled defaults to true when omitted.
	n, err := svc.CreateNode(context.Background(), 1, "pve9", "10.0.0.9", "root@pam!spark", "tok", nil)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if !n.Enabled || n.APITokenSecret != "tok" || n.ZoneID != 1 {
		t.Fatalf("unexpected node: %+v", n)
	}

	// enabled=false is honored.
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

	// Empty api_token keeps the stored secret.
	n, tokenChanged, err := svc.UpdateNode(context.Background(), 1, "pve1", "10.0.0.20", "root@pam!spark", "", nil)
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

	// A provided api_token replaces the secret.
	n, tokenChanged, err = svc.UpdateNode(context.Background(), 1, "pve1", "10.0.0.20", "root@pam!spark", "new-secret", nil)
	if err != nil {
		t.Fatalf("update node: %v", err)
	}
	if !tokenChanged || n.APITokenSecret != "new-secret" {
		t.Fatalf("token not replaced: changed=%v secret=%q", tokenChanged, n.APITokenSecret)
	}

	// Renaming onto an existing name -> conflict.
	nodeRepo.nodes = append(nodeRepo.nodes, model.PVENode{ID: 2, ZoneID: 1, Name: "pve2", Host: "10.0.0.30"})
	if _, _, err := svc.UpdateNode(context.Background(), 1, "pve2", "10.0.0.20", "root@pam!spark", "", nil); err == nil {
		t.Fatal("rename to existing name: want error")
	} else if err.(*Error).Kind != KindConflict {
		t.Fatalf("rename: kind = %v, want KindConflict", err.(*Error).Kind)
	}

	// Unknown node -> not_found.
	if _, _, err := svc.UpdateNode(context.Background(), 99, "pve9", "10.0.0.9", "root@pam!spark", "", nil); err == nil {
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

// reachablePVEServer answers GET /version with a valid PVE envelope.
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

	// Per-candidate client factory keyed by the node's host: node 1 probes
	// the dead server, node 2 the alive one.
	servers := map[string]*httptest.Server{"h1": dead, "h2": alive}
	nodes := []model.PVENode{
		{ID: 1, Name: "dead", Host: "h1", APIUser: "root@pam!probe", APITokenSecret: "secret123"},
		{ID: 2, Name: "alive", Host: "h2", APIUser: "root@pam!probe", APITokenSecret: "secret123"},
	}
	newClient := func(host, apiUser, apiTokenSecret string) *pve.Client {
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
	newClient := func(host, apiUser, apiTokenSecret string) *pve.Client {
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

func boolPtr(b bool) *bool { return &b }
