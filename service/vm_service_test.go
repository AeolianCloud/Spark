package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"spark/config"
	"spark/crypto"
	"spark/model"
	"spark/pve"
	"spark/repository"
)

var vmTestTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// ---------- fakes ----------

// fakeTx implements pgx.Tx minimally: the service tests only exercise
// Commit/Rollback directly; repo calls receive it as an opaque handle and
// ignore it.
type fakeTx struct {
	committed  bool
	rolledBack bool
}

func (f *fakeTx) Begin(ctx context.Context) (pgx.Tx, error) { return nil, errors.New("unused") }
func (f *fakeTx) Commit(ctx context.Context) error          { f.committed = true; return nil }
func (f *fakeTx) Rollback(ctx context.Context) error        { f.rolledBack = true; return nil }
func (f *fakeTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (f *fakeTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults { return nil }
func (f *fakeTx) LargeObjects() pgx.LargeObjects                               { return pgx.LargeObjects{} }
func (f *fakeTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (f *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}
func (f *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row { return nil }
func (f *fakeTx) Conn() *pgx.Conn                                               { return nil }

type fakeBeginner struct {
	tx *fakeTx
}

func (f *fakeBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	if f.tx == nil {
		f.tx = &fakeTx{}
	}
	return f.tx, nil
}

// fakeVMRepository is a scriptable VMRepository for service tests. It
// records the arguments of every mutation and serves a configurable GetVM.
//
// mu guards the fields written by the detached provisioning goroutine
// (UpdateVMPVEVMID, SetProvisionError) against the reads in the test
// goroutine, so -race stays clean.
type fakeVMRepository struct {
	mu               sync.Mutex
	created          *model.VM
	createErr        error
	get              *repository.VMWithIP
	getErr           error
	vms              []repository.VMWithIP
	listErr          error
	linkedIPID       int64
	linkedVMID       int64
	updateVmidDiskGB int64
	updateVmidErr    error
	provisionErrors  []string
	setProvisionErr  error
	updatedSpecs     []specUpdate
	updateSpecErr    error
	deleted          []int64
	deleteErr        error
}

// specUpdate records one UpdateSpec call: the optimistic lock baseline (old
// values read by the service) and the new values persisted.
type specUpdate struct {
	old spec
	new spec
}

type spec struct {
	cpu    int
	memMB  int64
	diskGB int64
}

// vmid returns the last VMID linked by the provisioning goroutine, safe for
// concurrent polling.
func (f *fakeVMRepository) vmid() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.linkedVMID
}

func (f *fakeVMRepository) CreateVMTx(ctx context.Context, tx pgx.Tx, vm model.VM) (*model.VM, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	created := vm
	created.ID = 1
	created.CreatedAt = vmTestTime
	created.UpdatedAt = vmTestTime
	f.created = &created
	return &created, nil
}

func (f *fakeVMRepository) GetVM(ctx context.Context, id int64) (*repository.VMWithIP, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.get == nil {
		return nil, pgx.ErrNoRows
	}
	return f.get, nil
}

func (f *fakeVMRepository) ListVMs(ctx context.Context) ([]repository.VMWithIP, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.vms, nil
}

func (f *fakeVMRepository) SetVMIPIDTx(ctx context.Context, tx pgx.Tx, id, ipID int64) error {
	f.linkedIPID = ipID
	return nil
}

func (f *fakeVMRepository) UpdateVMPVEVMID(ctx context.Context, id, vmid, diskGB int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateVmidErr != nil {
		return f.updateVmidErr
	}
	f.linkedVMID = vmid
	f.updateVmidDiskGB = diskGB
	return nil
}

func (f *fakeVMRepository) SetProvisionError(ctx context.Context, id int64, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setProvisionErr != nil {
		return f.setProvisionErr
	}
	f.provisionErrors = append(f.provisionErrors, message)
	return nil
}

func (f *fakeVMRepository) UpdateSpec(ctx context.Context, id int64, newCPU int, newMemMB, newDiskGB int64, oldCPU int, oldMemMB, oldDiskGB int64) error {
	if f.updateSpecErr != nil {
		return f.updateSpecErr
	}
	f.updatedSpecs = append(f.updatedSpecs, specUpdate{
		old: spec{cpu: oldCPU, memMB: oldMemMB, diskGB: oldDiskGB},
		new: spec{cpu: newCPU, memMB: newMemMB, diskGB: newDiskGB},
	})
	if f.get != nil {
		f.get.VM.CPU = newCPU
		f.get.VM.MemMB = newMemMB
		f.get.VM.DiskGB = newDiskGB
	}
	return nil
}

func (f *fakeVMRepository) DeleteVMTx(ctx context.Context, tx pgx.Tx, id int64) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

// fakeVMIPPoolRepository is a scriptable VMIPPoolRepository for service
// tests: scripted pool list/nodes and scripted claim results.
type fakeVMIPPoolRepository struct {
	pools        []model.IPPool
	poolNodes    map[int64][]model.PVENode
	claimResults []claimResult // consumed in order
	claimDefault error         // returned when the script is exhausted
	claimedVMIDs []int64
	released     []int64
}

type claimResult struct {
	ip  model.IP
	err error
}

func (f *fakeVMIPPoolRepository) ListPoolsByZone(ctx context.Context, zoneID int64) ([]model.IPPool, error) {
	return f.pools, nil
}

func (f *fakeVMIPPoolRepository) GetPoolNodes(ctx context.Context, poolID int64) ([]model.PVENode, error) {
	return f.poolNodes[poolID], nil
}

func (f *fakeVMIPPoolRepository) ClaimFreeIP(ctx context.Context, tx pgx.Tx, poolID int64, vmID *int64) (model.IP, error) {
	if vmID != nil {
		f.claimedVMIDs = append(f.claimedVMIDs, *vmID)
	}
	if len(f.claimResults) > 0 {
		r := f.claimResults[0]
		f.claimResults = f.claimResults[1:]
		return r.ip, r.err
	}
	if f.claimDefault != nil {
		return model.IP{}, f.claimDefault
	}
	return model.IP{}, pgx.ErrNoRows
}

func (f *fakeVMIPPoolRepository) ReleaseIPByVMTx(ctx context.Context, tx pgx.Tx, vmID int64) error {
	f.released = append(f.released, vmID)
	return nil
}

// fakeVMZoneRepository is a scriptable VMZoneRepository for service tests.
type fakeVMZoneRepository struct {
	zones []model.Zone
	err   error
}

func (f *fakeVMZoneRepository) GetZone(ctx context.Context, id int64) (*model.Zone, error) {
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

func (f *fakeVMZoneRepository) ListZones(ctx context.Context) ([]model.Zone, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]model.Zone, 0, len(f.zones))
	for i := range f.zones {
		out = append(out, f.zones[i])
	}
	return out, nil
}

// fakeVMNodeRepository is a scriptable VMNodeRepository for service tests.
type fakeVMNodeRepository struct {
	nodes []model.PVENode
	err   error
}

func (f *fakeVMNodeRepository) GetNode(ctx context.Context, id int64) (*model.PVENode, error) {
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

func (f *fakeVMNodeRepository) ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
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

// fakeVMImageRepository is a scriptable VMImageRepository for service tests.
type fakeVMImageRepository struct {
	images    []model.Image
	nodeNames []string
	err       error
}

func (f *fakeVMImageRepository) Get(ctx context.Context, id int64) (*model.Image, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.images {
		if f.images[i].ID == id {
			img := f.images[i]
			return &img, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeVMImageRepository) EnabledNodeNamesByZone(ctx context.Context, zoneID int64) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.nodeNames, nil
}

// fakeVMStorageTypeRepository is a scriptable VMStorageTypeRepository for
// service tests.
type fakeVMStorageTypeRepository struct {
	types []model.StorageType
}

func (f *fakeVMStorageTypeRepository) Get(ctx context.Context, id int64) (*model.StorageType, error) {
	for i := range f.types {
		if f.types[i].ID == id {
			st := f.types[i]
			return &st, nil
		}
	}
	return nil, pgx.ErrNoRows
}

// ---------- helpers ----------

// testCipher builds a cipher from a deterministic 32-byte key.
func testCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	cfg := config.Default()
	cfg.Crypto.EncryptionKey = base64.StdEncoding.EncodeToString(key)
	c, err := crypto.NewCipher(cfg)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

// newVMService wires a VMService with fakes and a fake transaction
// beginner; the node selector is stubbed to return the first candidate. The
// default PVE client points the detached provisioning chain at a full
// success-chain server, so tests that spawn the chain complete quietly and
// deterministically; tests that exercise the chain itself override
// newClient.
func newVMService(t *testing.T, vmRepo VMRepository, ipRepo VMIPPoolRepository,
	zoneRepo VMZoneRepository, nodeRepo VMNodeRepository, imageRepo VMImageRepository,
	stRepo VMStorageTypeRepository) *VMService {
	t.Helper()
	svc := NewVMService(&fakeBeginner{}, vmRepo, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo, testCipher(t))
	svc.selectNode = firstReachableCandidate
	srv := newScriptedProvisionServer(t, "15G")
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	return svc
}

// waitForProvision blocks until the detached provisioning goroutine has
// finished (the fake repo's UpdateVMPVEVMID was called) or a short timeout,
// so tests do not race with the goroutine and its server stays open.
func waitForProvision(t *testing.T, repo *fakeVMRepository) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if repo.vmid() != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("provisioning goroutine did not finish in time")
}

// firstReachableCandidate is a fake node selector: it returns the first
// candidate or a node_unavailable error for an empty list.
func firstReachableCandidate(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
	if len(nodes) == 0 {
		return model.PVENode{}, nodeUnavailablef("no candidates")
	}
	return nodes[0], nil
}

// scriptedSelectNode builds a fake node selector that returns the first
// candidate whose name is not in dead.
func scriptedSelectNode(dead map[string]bool) func(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
	return func(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
		for _, n := range nodes {
			if !dead[n.Name] {
				return n, nil
			}
		}
		return model.PVENode{}, nodeUnavailablef("no reachable candidate")
	}
}

// testPVENode is a node with valid PVE token credentials (split form
// "root@pam" + "spark=uuid" -> PVEAPIToken=root@pam!spark=uuid).
func testPVENode(id int64) model.PVENode {
	return model.PVENode{ID: id, ZoneID: 1, Name: "pve1", Host: "h",
		APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true}
}

// createEnv builds a fully valid fake environment for happy-path create
// tests: zone 1 with one enabled node, one pool whitelisting that node, one
// image present on the node and one storage type.
func createEnv() (*fakeVMZoneRepository, *fakeVMImageRepository, *fakeVMStorageTypeRepository, *fakeVMNodeRepository, *fakeVMIPPoolRepository) {
	node := testPVENode(1)
	zoneRepo := &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	imageRepo := &fakeVMImageRepository{
		images: []model.Image{{ID: 1, Name: "debian-12-cloud", DefaultUser: "debian",
			NodeImages: map[string]string{"pve1": "/templates/debian-12-cloud.qcow2"}}},
		nodeNames: []string{"pve1"},
	}
	stRepo := &fakeVMStorageTypeRepository{types: []model.StorageType{{ID: 1, Name: "ssd", PVEStorage: "local-ssd"}}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{node}}
	ipRepo := &fakeVMIPPoolRepository{
		pools:     []model.IPPool{{ID: 1, ZoneID: 1, Name: "p1", NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1", DNS: "1.1.1.1"}},
		poolNodes: map[int64][]model.PVENode{1: {node}},
	}
	return zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo
}

func validCreateRequest() CreateVMRequest {
	return CreateVMRequest{Name: "vm1", CPU: 2, MemMB: 2048, DiskGB: 10, ImageID: 1, StorageTypeID: 1, ZoneID: 1, Password: "s3cret"}
}

// ---------- validation tests ----------

func TestValidateCreateVMRequest(t *testing.T) {
	req := validCreateRequest()
	if err := validateCreateVMRequest(req); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*CreateVMRequest)
	}{
		{"empty name", func(r *CreateVMRequest) { r.Name = " " }},
		{"name with leading dash", func(r *CreateVMRequest) { r.Name = "-vm1" }},
		{"name with leading dot", func(r *CreateVMRequest) { r.Name = ".vm1" }},
		{"name with space", func(r *CreateVMRequest) { r.Name = "vm 1" }},
		{"name with slash", func(r *CreateVMRequest) { r.Name = "vm/1" }},
		{"name with colon", func(r *CreateVMRequest) { r.Name = "vm:1" }},
		{"empty password", func(r *CreateVMRequest) { r.Password = "" }},
		{"zero cpu", func(r *CreateVMRequest) { r.CPU = 0 }},
		{"zero mem", func(r *CreateVMRequest) { r.MemMB = 0 }},
		{"zero disk", func(r *CreateVMRequest) { r.DiskGB = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := req
			tc.mutate(&r)
			if err := validateCreateVMRequest(r); !isKind(err, KindBadRequest) {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	}
	// PVE-legal name variants (dash/dot/underscore after the first char).
	for _, name := range []string{"vm-1", "vm_1", "vm.1", "v1.2-3_x"} {
		r := req
		r.Name = name
		if err := validateCreateVMRequest(r); err != nil {
			t.Fatalf("name %q rejected: %v", name, err)
		}
	}
}

func TestValidateResizeSpec(t *testing.T) {
	current := model.VM{CPU: 2, MemMB: 2048, DiskGB: 10}
	cpu := 4
	mem := int64(4096)
	disk := int64(20)
	smallDisk := int64(5)

	// No field -> bad request.
	if err := validateResizeSpec(nil, nil, nil, current); !isKind(err, KindBadRequest) {
		t.Fatalf("no fields err = %v, want KindBadRequest", err)
	}
	// Disk shrink -> KindDiskShrinkNotAllowed.
	if err := validateResizeSpec(nil, nil, &smallDisk, current); !isKind(err, KindDiskShrinkNotAllowed) {
		t.Fatalf("shrink err = %v, want KindDiskShrinkNotAllowed", err)
	}
	// Disk equal -> allowed (documented no-op).
	if err := validateResizeSpec(nil, nil, &current.DiskGB, current); err != nil {
		t.Fatalf("equal disk err = %v, want nil (no-op)", err)
	}
	// Grow everything -> allowed.
	if err := validateResizeSpec(&cpu, &mem, &disk, current); err != nil {
		t.Fatalf("grow err = %v, want nil", err)
	}
	// Non-positive values -> bad request.
	zeroMem := int64(0)
	zeroCPU := 0
	if err := validateResizeSpec(nil, &zeroMem, nil, current); !isKind(err, KindBadRequest) {
		t.Fatalf("zero mem err = %v, want KindBadRequest", err)
	}
	if err := validateResizeSpec(&zeroCPU, nil, nil, current); !isKind(err, KindBadRequest) {
		t.Fatalf("zero cpu err = %v, want KindBadRequest", err)
	}
}

func TestPoolCandidates(t *testing.T) {
	poolNodes := []model.PVENode{
		{ID: 3, Name: "n3"},
		{ID: 1, Name: "n1"},
		{ID: 2, Name: "n2"},
	}
	enabled := []model.PVENode{
		{ID: 1, Name: "n1"},
		{ID: 2, Name: "n2"},
	}
	got := poolCandidates(poolNodes, enabled)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("candidates = %+v, want [n1 n2] in node id order", got)
	}
	if got := poolCandidates(poolNodes, nil); len(got) != 0 {
		t.Fatalf("no enabled nodes: candidates = %+v, want empty", got)
	}
}

// ---------- create flow tests ----------

func TestCreateVMHappyPath(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	vmRepo := &fakeVMRepository{}
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 7, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}}}
	svc := newVMService(t, vmRepo, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	vm, err := svc.CreateVM(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if vm.VM.ID != 1 || vm.VM.PVEVmid != 0 || vm.VM.Name != "vm1" {
		t.Fatalf("vm = %+v", vm.VM)
	}
	if vm.IP != "10.0.0.5" {
		t.Fatalf("ip = %q, want 10.0.0.5", vm.IP)
	}
	if vm.VM.IPID == nil || *vm.VM.IPID != 7 {
		t.Fatalf("ip_id = %v, want 7", vm.VM.IPID)
	}
	// The transaction order: claim linked the created vm id, then the
	// ip_id link was written.
	if len(ipRepo.claimedVMIDs) != 1 || ipRepo.claimedVMIDs[0] != 1 {
		t.Fatalf("claimed vm ids = %v, want [1]", ipRepo.claimedVMIDs)
	}
	if vmRepo.linkedIPID != 7 {
		t.Fatalf("linked ip id = %d, want 7", vmRepo.linkedIPID)
	}
	waitForProvision(t, vmRepo)
}

// TestCreateVMEncryptsPassword pins down that the password is encrypted
// before it is handed to the repository: the stored value differs from the
// plaintext and decrypts back to it.
func TestCreateVMEncryptsPassword(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	vmRepo := &fakeVMRepository{}
	ipRepo.claimResults = []claimResult{{ip: model.IP{ID: 7, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}}}
	svc := newVMService(t, vmRepo, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	const pw = "s3cret-pw-123"
	req := validCreateRequest()
	req.Password = pw
	if _, err := svc.CreateVM(context.Background(), req); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	enc := vmRepo.created.PasswordEncrypted
	if enc == "" || enc == pw {
		t.Fatalf("stored password = %q, want an encrypted value", enc)
	}
	plain, err := testCipher(t).Decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt stored password: %v", err)
	}
	if plain != pw {
		t.Fatalf("decrypted = %q, want %q", plain, pw)
	}
	waitForProvision(t, vmRepo)
}

func TestCreateVMValidationOrder(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	svc := newVMService(t, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	req := validCreateRequest()
	// Unknown zone -> not_found.
	r := req
	r.ZoneID = 99
	if _, err := svc.CreateVM(context.Background(), r); !isKind(err, KindNotFound) {
		t.Fatalf("unknown zone err = %v, want KindNotFound", err)
	}
	// Unknown image -> not_found.
	r = req
	r.ImageID = 99
	if _, err := svc.CreateVM(context.Background(), r); !isKind(err, KindNotFound) {
		t.Fatalf("unknown image err = %v, want KindNotFound", err)
	}
	// Unknown storage type -> not_found.
	r = req
	r.StorageTypeID = 99
	if _, err := svc.CreateVM(context.Background(), r); !isKind(err, KindNotFound) {
		t.Fatalf("unknown storage err = %v, want KindNotFound", err)
	}
	// Image not present on all enabled nodes -> KindImageNotAvailable.
	imgRepo := &fakeVMImageRepository{
		images: []model.Image{{ID: 1, Name: "debian-12-cloud", DefaultUser: "debian",
			NodeImages: map[string]string{"pve1": "/t.img"}}},
		nodeNames: []string{"pve1", "pve2"},
	}
	svc2 := newVMService(t, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo, imgRepo, stRepo)
	if _, err := svc2.CreateVM(context.Background(), req); !isKind(err, KindImageNotAvailable) {
		t.Fatalf("image not available err = %v, want KindImageNotAvailable", err)
	}
	// Empty password -> bad_request.
	r = req
	r.Password = ""
	if _, err := svc.CreateVM(context.Background(), r); !isKind(err, KindBadRequest) {
		t.Fatalf("empty password err = %v, want KindBadRequest", err)
	}
	// Zero cpu -> bad_request.
	r = req
	r.CPU = 0
	if _, err := svc.CreateVM(context.Background(), r); !isKind(err, KindBadRequest) {
		t.Fatalf("zero cpu err = %v, want KindBadRequest", err)
	}
}

func TestCreateVMClaimRetriesThenSucceeds(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	vmRepo := &fakeVMRepository{}
	ipRepo.claimResults = []claimResult{
		{err: repository.ErrAllocationRetry},
		{err: repository.ErrAllocationRetry},
		{ip: model.IP{ID: 7, PoolID: 1, IP: "10.0.0.5", Status: model.IPStatusUsed}},
	}
	svc := newVMService(t, vmRepo, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	vm, err := svc.CreateVM(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if vm.IP != "10.0.0.5" {
		t.Fatalf("ip = %q", vm.IP)
	}
	if len(ipRepo.claimResults) != 0 {
		t.Fatalf("claim script not fully consumed: %+v", ipRepo.claimResults)
	}
	waitForProvision(t, vmRepo)
}

func TestCreateVMClaimExhausted(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	ipRepo.claimResults = make([]claimResult, vmClaimRetries)
	for i := range ipRepo.claimResults {
		ipRepo.claimResults[i] = claimResult{err: repository.ErrAllocationRetry}
	}
	svc := newVMService(t, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	_, err := svc.CreateVM(context.Background(), validCreateRequest())
	if !isKind(err, KindIPExhausted) {
		t.Fatalf("err = %v, want KindIPExhausted", err)
	}
}

func TestCreateVMClaimNoFreeAddress(t *testing.T) {
	zoneRepo, imageRepo, stRepo, nodeRepo, ipRepo := createEnv()
	ipRepo.claimDefault = pgx.ErrNoRows
	svc := newVMService(t, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo, imageRepo, stRepo)

	_, err := svc.CreateVM(context.Background(), validCreateRequest())
	if !isKind(err, KindIPExhausted) {
		t.Fatalf("err = %v, want KindIPExhausted", err)
	}
}

// TestSelectPoolAndNodeSkipsUnreachablePools verifies D4: an unreachable
// pool is skipped in favor of the next one, and a zone with no reachable
// pool yields node_unavailable.
func TestSelectPoolAndNodeSkipsUnreachablePools(t *testing.T) {
	deadNode := model.PVENode{ID: 1, ZoneID: 1, Name: "pve-dead", Host: "h1", Enabled: true}
	aliveNode := model.PVENode{ID: 2, ZoneID: 1, Name: "pve-alive", Host: "h2", Enabled: true}
	zoneRepo := &fakeVMZoneRepository{zones: []model.Zone{{ID: 1, Name: "z1"}}}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{deadNode, aliveNode}}
	ipRepo := &fakeVMIPPoolRepository{
		pools: []model.IPPool{
			{ID: 1, ZoneID: 1, Name: "dead-pool", NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
			{ID: 2, ZoneID: 1, Name: "alive-pool", NetworkCIDR: "10.0.1.0/24", Gateway: "10.0.1.1"},
		},
		poolNodes: map[int64][]model.PVENode{1: {deadNode}, 2: {aliveNode}},
	}
	svc := NewVMService(&fakeBeginner{}, &fakeVMRepository{}, ipRepo, zoneRepo, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{}, testCipher(t))

	// Pool 1's only node is dead, pool 2's is reachable -> pool 2 wins.
	svc.selectNode = scriptedSelectNode(map[string]bool{"pve-dead": true})
	pool, node, err := svc.selectPoolAndNode(context.Background(), 1)
	if err != nil {
		t.Fatalf("selectPoolAndNode: %v", err)
	}
	if pool.ID != 2 || node.Name != "pve-alive" {
		t.Fatalf("pool = %+v node = %+v, want pool 2 / pve-alive", pool, node)
	}

	// Both pools' nodes are dead -> node_unavailable.
	svc.selectNode = scriptedSelectNode(map[string]bool{"pve-dead": true, "pve-alive": true})
	_, _, err = svc.selectPoolAndNode(context.Background(), 1)
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("err = %v, want KindNodeUnavailable", err)
	}

	// No pools at all -> node_unavailable (candidate set empty).
	ipRepo.pools = nil
	_, _, err = svc.selectPoolAndNode(context.Background(), 1)
	if !isKind(err, KindNodeUnavailable) {
		t.Fatalf("no pools err = %v, want KindNodeUnavailable", err)
	}
}

// ---------- provisioning chain tests ----------

// scriptedProvisionServer answers the whole successful provisioning chain
// (nextid, create, task status, config) and records what CreateVM was asked
// to do. requestDisk/configSize control the resize behavior.
type scriptedProvisionServer struct {
	t          *testing.T
	createBody map[string]any
	configSize string // size= value in the returned scsi0 config
	creates    int
	resizes    int
	destroyed  bool
	requests   map[string]int
}

func newScriptedProvisionServer(t *testing.T, configSize string) *scriptedProvisionServer {
	s := &scriptedProvisionServer{t: t, configSize: configSize, requests: map[string]int{}}
	return s
}

func (s *scriptedProvisionServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/cluster/nextid":
			fmt.Fprint(w, `{"data": "100"}`)
		case r.URL.Path == "/nodes/pve1/qemu" && r.Method == http.MethodPost:
			s.creates++
			s.createBody = map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&s.createBody); err != nil {
				s.t.Errorf("decode create body: %v", err)
			}
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmcreate:100:root@pam:"}`)
		case strings.HasPrefix(r.URL.Path, "/nodes/pve1/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprint(w, `{"data": {"upid": "UPID:pve1:0:0:0:qmcreate:100:root@pam:", "node": "pve1", "type": "qmcreate", "id": "100", "user": "root@pam", "status": "stopped", "exitstatus": "OK"}}`)
		case r.URL.Path == "/nodes/pve1/qemu/100/config" && r.Method == http.MethodGet:
			fmt.Fprintf(w, `{"data": {"bootdisk": "scsi0", "scsi0": "local-ssd:vm-100-disk-0,size=%s", "cores": "2", "memory": "2048"}}`, s.configSize)
		case r.URL.Path == "/nodes/pve1/qemu/100/resize":
			s.resizes++
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:resize:100:root@pam:"}`)
		case r.URL.Path == "/nodes/pve1/qemu/100" && r.Method == http.MethodDelete:
			s.destroyed = true
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmdestroy:100:root@pam:"}`)
		default:
			s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

// TestProvisionSuccessChain verifies the full single-step chain: create
// carries the import-from disk, cloud-init disk, vmbr0 net and cloud-init
// injection in one call, and the pve_vmid plus the final disk size land in
// the DB. The image is bigger than the request, so no resize happens and
// the actual size is persisted.
func TestProvisionSuccessChain(t *testing.T) {
	srv := newScriptedProvisionServer(t, "15G")
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	const pw = "pw-injected"
	err := svc.provision(context.Background(),
		model.VM{ID: 1, Name: "vm1", MemMB: 2048, CPU: 2, DiskGB: 10},
		model.PVENode{Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
		&model.Image{Name: "debian-12-cloud", DefaultUser: "debian", NodeImages: map[string]string{"pve1": "/templates/d.qcow2"}},
		&model.StorageType{PVEStorage: "local-ssd"},
		model.IPPool{NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1", DNS: "1.1.1.1"},
		pw, "10.0.0.5")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Single-step create payload (D5).
	body := srv.createBody
	if body["vmid"] != float64(100) || body["name"] != "vm1" ||
		body["memory"] != float64(2048) || body["cores"] != float64(2) ||
		body["scsi0"] != "local-ssd:0,import-from=/templates/d.qcow2" ||
		body["ide2"] != "local-ssd:cloudinit" ||
		body["net0"] != "virtio,bridge=vmbr0" ||
		body["bootdisk"] != "scsi0" || body["scsihw"] != "virtio-scsi-pci" ||
		body["ciuser"] != "debian" || body["cipassword"] != pw ||
		body["ipconfig0"] != "ip=10.0.0.5/24,gw=10.0.0.1" ||
		body["nameserver"] != "1.1.1.1" {
		t.Fatalf("create body = %v", body)
	}
	if srv.resizes != 0 {
		t.Fatalf("resizes = %d, want 0 (image already >= request)", srv.resizes)
	}
	// Metadata: vmid recorded, disk synced to the actual image size 15G.
	if vmRepo.linkedVMID != 100 {
		t.Fatalf("vmid = %d, want 100", vmRepo.linkedVMID)
	}
	if vmRepo.updateVmidDiskGB != 15 {
		t.Fatalf("disk_gb = %d, want 15 (actual image size)", vmRepo.updateVmidDiskGB)
	}
}

// TestProvisionSuccessChainWithResize covers the grow path: the imported
// image is smaller than the request, so a resize task runs and the requested
// size is persisted.
func TestProvisionSuccessChainWithResize(t *testing.T) {
	srv := newScriptedProvisionServer(t, "5G")
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	err := svc.provision(context.Background(),
		model.VM{ID: 1, Name: "vm1", MemMB: 2048, CPU: 2, DiskGB: 20},
		model.PVENode{Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
		&model.Image{Name: "img", DefaultUser: "debian", NodeImages: map[string]string{"pve1": "/t.img"}},
		&model.StorageType{PVEStorage: "local-ssd"},
		model.IPPool{NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
		"pw", "10.0.0.5")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if srv.resizes != 1 {
		t.Fatalf("resizes = %d, want 1", srv.resizes)
	}
	if vmRepo.updateVmidDiskGB != 20 {
		t.Fatalf("disk_gb = %d, want 20 (requested)", vmRepo.updateVmidDiskGB)
	}
}

// TestProvisionFailureSetsSanitizedError drives the failure branch of the
// detached chain: the PVE error message contains the plaintext password and
// the persisted provision_error must not leak it.
func TestProvisionFailureSetsSanitizedError(t *testing.T) {
	const pw = "s3cretpw-leak-check"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"errors": {"cipassword": "password %s rejected by policy"}}`, pw)
	}))
	defer srv.Close()

	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(srv.URL), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second))
	}

	err := svc.provision(context.Background(),
		model.VM{ID: 1, Name: "vm1", MemMB: 2048, CPU: 2, DiskGB: 10},
		model.PVENode{Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
		&model.Image{Name: "img", DefaultUser: "debian", NodeImages: map[string]string{"pve1": "/t.img"}},
		&model.StorageType{PVEStorage: "local-ssd"},
		model.IPPool{NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
		pw, "10.0.0.5")
	if err == nil {
		t.Fatal("provision succeeded, want failure")
	}
	if len(vmRepo.provisionErrors) != 1 {
		t.Fatalf("provision errors = %d, want 1", len(vmRepo.provisionErrors))
	}
	msg := vmRepo.provisionErrors[0]
	if strings.Contains(msg, pw) {
		t.Fatalf("provision_error leaks the password: %q", msg)
	}
	if !strings.Contains(msg, "rejected by policy") {
		t.Fatalf("provision_error = %q, want the PVE message", msg)
	}
}

// TestProvisionMissingImagePath records a provisioning failure when the
// image has no path on the selected node.
func TestProvisionMissingImagePath(t *testing.T) {
	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})

	err := svc.provision(context.Background(),
		model.VM{ID: 1, Name: "vm1", MemMB: 2048, CPU: 2, DiskGB: 10},
		model.PVENode{Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
		&model.Image{Name: "img", DefaultUser: "debian", NodeImages: map[string]string{"other": "/t.img"}},
		&model.StorageType{PVEStorage: "local-ssd"},
		model.IPPool{NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
		"pw", "10.0.0.5")
	if err == nil {
		t.Fatal("provision succeeded, want failure")
	}
	if len(vmRepo.provisionErrors) != 1 {
		t.Fatalf("provision errors = %d, want 1", len(vmRepo.provisionErrors))
	}
}

// TestProvisionFailureAfterCreateIncludesVMID covers the half-created state:
// the create task succeeded on PVE (vmid=100) but waiting for it failed, so
// the persisted provision_error must carry the vmid so operators can locate
// and clean up the orphaned VM.
func TestProvisionFailureAfterCreateIncludesVMID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/cluster/nextid":
			fmt.Fprint(w, `{"data": "100"}`)
		case r.URL.Path == "/nodes/pve1/qemu" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmcreate:100:root@pam:"}`)
		case strings.HasPrefix(r.URL.Path, "/nodes/pve1/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"errors": {"_": "task died"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(srv.URL), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second))
	}

	err := svc.provision(context.Background(),
		model.VM{ID: 1, Name: "vm1", MemMB: 2048, CPU: 2, DiskGB: 10},
		model.PVENode{Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"},
		&model.Image{Name: "img", DefaultUser: "debian", NodeImages: map[string]string{"pve1": "/t.img"}},
		&model.StorageType{PVEStorage: "local-ssd"},
		model.IPPool{NetworkCIDR: "10.0.0.0/24", Gateway: "10.0.0.1"},
		"pw", "10.0.0.5")
	if err == nil {
		t.Fatal("provision succeeded, want failure")
	}
	if len(vmRepo.provisionErrors) != 1 {
		t.Fatalf("provision errors = %d, want 1", len(vmRepo.provisionErrors))
	}
	msg := vmRepo.provisionErrors[0]
	if !strings.Contains(msg, "vmid=100") || !strings.Contains(msg, "create succeeded") {
		t.Fatalf("provision_error = %q, want it to carry vmid=100 and the create-succeeded marker", msg)
	}
}

// TestSanitizeProvisionError pins down redaction and length bounding.
func TestSanitizeProvisionError(t *testing.T) {
	msg := sanitizeProvisionError(errors.New("cipassword 'abc123' failed for 'abc123'"), "abc123")
	if strings.Contains(msg, "abc123") {
		t.Fatalf("message still carries the secret: %q", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Fatalf("message = %q, want [redacted] marker", msg)
	}
	long := strings.Repeat("x", maxProvisionErrorLen+100)
	msg = sanitizeProvisionError(errors.New(long), "secret")
	if len(msg) != maxProvisionErrorLen {
		t.Fatalf("length = %d, want %d", len(msg), maxProvisionErrorLen)
	}
}

// TestFailProvisionBoundsLength pins down the failProvision contract: the
// step prefix is assembled first and the whole message sanitized (redact
// then truncate), so a verbose PVE error cannot push the stored
// provision_error past maxProvisionErrorLen, the prefix is never truncated
// away, and a secret straddling the truncation boundary never leaks a
// fragment.
func TestFailProvisionBoundsLength(t *testing.T) {
	const secret = "s3cretpw-boundary"
	prefix := "create succeeded (vmid=100) but wait create failed: "
	vmRepo := &fakeVMRepository{}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})

	// The secret ends right after the truncation point of the prefixed
	// message, so a truncate-before-redact order would leave a fragment.
	err := svc.failProvision(context.Background(), 1, 100, "wait create",
		errors.New(strings.Repeat("a", maxProvisionErrorLen-len(prefix)-4)+secret), secret)
	if err == nil {
		t.Fatal("failProvision returned nil")
	}
	if len(vmRepo.provisionErrors) != 1 {
		t.Fatalf("provision errors = %d, want 1", len(vmRepo.provisionErrors))
	}
	msg := vmRepo.provisionErrors[0]
	if !strings.HasPrefix(msg, prefix) {
		t.Fatalf("provision_error = %q, want the step prefix preserved", msg)
	}
	if utf8.RuneCountInString(msg) != maxProvisionErrorLen {
		t.Fatalf("provision_error length = %d runes, want %d", utf8.RuneCountInString(msg), maxProvisionErrorLen)
	}
	if strings.Contains(msg, secret) || strings.Contains(msg, "s3cr") {
		t.Fatalf("provision_error leaks the secret or a fragment: %q", msg)
	}
	if !utf8.ValidString(msg) {
		t.Fatalf("provision_error is not valid UTF-8: %q", msg)
	}
}

// TestSanitizeProvisionErrorRedactsBeforeTruncate pins down the
// redact-then-truncate order: a secret straddling the length boundary must
// not leak a partial fragment, and the result must stay valid UTF-8 (a naive
// byte truncation would split multi-byte runes and the DB would reject the
// value, Postgres 22021).
func TestSanitizeProvisionErrorRedactsBeforeTruncate(t *testing.T) {
	secret := "PASSWORD-1234-5678"
	msg := strings.Repeat("a", maxProvisionErrorLen-8) + secret
	out := sanitizeProvisionError(errors.New(msg), secret)
	if strings.Contains(out, secret) {
		t.Fatalf("message leaks the secret across the truncation boundary: %q", out)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("message is not valid UTF-8: %q", out)
	}
}

// TestSanitizeProvisionErrorKeepsUTF8 checks the rune-safe truncation of a
// multi-byte message: a byte cut inside a UTF-8 character would produce an
// invalid sequence (Postgres rejects it, error 22021).
func TestSanitizeProvisionErrorKeepsUTF8(t *testing.T) {
	msg := strings.Repeat("界", maxProvisionErrorLen+3)
	out := sanitizeProvisionError(errors.New(msg))
	if !utf8.ValidString(out) {
		t.Fatalf("message is not valid UTF-8: %q", out)
	}
	if got := utf8.RuneCountInString(out); got != maxProvisionErrorLen {
		t.Fatalf("rune count = %d, want %d", got, maxProvisionErrorLen)
	}
}

func TestParseDiskSizeGB(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"local-ssd:vm-100-disk-0,size=10G", 10},
		{"local-ssd:vm-100-disk-0,size=5G,backup=0", 5},
		{"local:vm-100-disk-0,size=10737418240", 10},
		{"local:vm-100-disk-0,size=1.5G", 1},
	}
	for _, tc := range cases {
		got, err := parseDiskSizeGB(tc.in)
		if err != nil {
			t.Fatalf("parseDiskSizeGB(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("parseDiskSizeGB(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if _, err := parseDiskSizeGB("local:vm-100-disk-0"); err == nil {
		t.Fatal("disk string without size should fail")
	}
}

// ---------- lifecycle operation tests ----------

func provisionedVM() *repository.VMWithIP {
	return &repository.VMWithIP{
		VM: model.VM{ID: 1, NodeID: 1, PVEVmid: 100, CPU: 2, MemMB: 2048, DiskGB: 10, Name: "vm1"},
		IP: "10.0.0.5",
	}
}

// startStopRestartServer answers the three status endpoints with a UPID.
func startStopRestartServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := false
		for _, action := range []string{"/start", "/stop", "/reboot"} {
			if strings.HasSuffix(r.URL.Path, action) {
				ok = true
			}
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmstart:100:root@pam:"}`)
	}))
}

func TestStartStopRestart(t *testing.T) {
	ts := startStopRestartServer(t)
	defer ts.Close()

	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid", Enabled: true}}}
	svc := newVMService(t, &fakeVMRepository{get: provisionedVM()}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	ctx := context.Background()
	if err := svc.Start(ctx, 1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := svc.Stop(ctx, 1); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := svc.Restart(ctx, 1); err != nil {
		t.Fatalf("Restart: %v", err)
	}
}

func TestStartVMNotFound(t *testing.T) {
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if err := svc.Start(context.Background(), 404); !isKind(err, KindNotFound) {
		t.Fatalf("err = %v, want KindNotFound", err)
	}
}

func TestStartVMNotProvisioned(t *testing.T) {
	vm := provisionedVM()
	vm.VM.PVEVmid = 0
	svc := newVMService(t, &fakeVMRepository{get: vm}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if err := svc.Start(context.Background(), 1); !isKind(err, KindVMNotReady) {
		t.Fatalf("err = %v, want KindVMNotReady", err)
	}
}

// TestStartVMPVENotFound maps a PVE 404 (the VM no longer exists on the
// node) onto vm_not_ready.
func TestStartVMPVENotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors": {"_": "VM 100 not found on this node"}}`)
	}))
	defer ts.Close()

	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, &fakeVMRepository{get: provisionedVM()}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, nodeRepo, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}
	if err := svc.Start(context.Background(), 1); !isKind(err, KindVMNotReady) {
		t.Fatalf("err = %v, want KindVMNotReady", err)
	}
}

func TestDestroyFlow(t *testing.T) {
	srv := newScriptedProvisionServer(t, "10G")
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM()}
	ipRepo := &fakeVMIPPoolRepository{}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, vmRepo, ipRepo, &fakeVMZoneRepository{}, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	if err := svc.Destroy(context.Background(), 1); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !srv.destroyed {
		t.Fatal("PVE destroy was not invoked")
	}
	// IP released and row deleted in the same tx.
	if len(ipRepo.released) != 1 || ipRepo.released[0] != 1 {
		t.Fatalf("released = %v, want [1]", ipRepo.released)
	}
	if len(vmRepo.deleted) != 1 || vmRepo.deleted[0] != 1 {
		t.Fatalf("deleted = %v, want [1]", vmRepo.deleted)
	}
}

// TestDestroyUnprovisioned skips the PVE call (no pve_vmid yet) but still
// cleans up the local record and the IP.
func TestDestroyUnprovisioned(t *testing.T) {
	vm := provisionedVM()
	vm.VM.PVEVmid = 0
	vmRepo := &fakeVMRepository{get: vm}
	ipRepo := &fakeVMIPPoolRepository{}
	svc := newVMService(t, vmRepo, ipRepo, &fakeVMZoneRepository{}, &fakeVMNodeRepository{},
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	// newClient from newVMService answers 503 for everything; the PVE call
	// must never happen, so any invocation would fail the test below.

	if err := svc.Destroy(context.Background(), 1); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(ipRepo.released) != 1 || len(vmRepo.deleted) != 1 {
		t.Fatalf("released = %v, deleted = %v", ipRepo.released, vmRepo.deleted)
	}
}

func TestDestroyVMNotFound(t *testing.T) {
	svc := newVMService(t, &fakeVMRepository{}, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{}, &fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	if err := svc.Destroy(context.Background(), 404); !isKind(err, KindNotFound) {
		t.Fatalf("err = %v, want KindNotFound", err)
	}
}

// TestDestroyPVEFailureKeepsRecordAndIP verifies that a PVE failure aborts
// the destroy: neither the IP nor the row are touched.
func TestDestroyPVEFailureKeepsRecordAndIP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors": {"_": "cannot destroy"}}`)
	}))
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM()}
	ipRepo := &fakeVMIPPoolRepository{}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, vmRepo, ipRepo, &fakeVMZoneRepository{}, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	if err := svc.Destroy(context.Background(), 1); err == nil {
		t.Fatal("Destroy succeeded, want PVE failure")
	}
	if len(ipRepo.released) != 0 || len(vmRepo.deleted) != 0 {
		t.Fatalf("released = %v, deleted = %v, want untouched", ipRepo.released, vmRepo.deleted)
	}
}

// TestDestroyPVE404CleansUpLocal treats a PVE 404 on destroy (the VM was
// already removed on the node, e.g. by an operator) as "already destroyed":
// the local cleanup still runs and the destroy succeeds.
func TestDestroyPVE404CleansUpLocal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors": {"_": "VM 100 not found on this node"}}`)
	}))
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM()}
	ipRepo := &fakeVMIPPoolRepository{}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, vmRepo, ipRepo, &fakeVMZoneRepository{}, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	if err := svc.Destroy(context.Background(), 1); err != nil {
		t.Fatalf("Destroy with PVE 404: %v", err)
	}
	if len(ipRepo.released) != 1 || ipRepo.released[0] != 1 {
		t.Fatalf("released = %v, want [1]", ipRepo.released)
	}
	if len(vmRepo.deleted) != 1 || vmRepo.deleted[0] != 1 {
		t.Fatalf("deleted = %v, want [1]", vmRepo.deleted)
	}
}

// ---------- resize tests ----------

// noCallServer fails the test when the PVE client is used; used to assert
// that no PVE call happens on validation/no-op paths.
func noCallServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected PVE call: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
}

func TestResizeRejectsShrinkBeforePVE(t *testing.T) {
	ts := noCallServer(t)
	defer ts.Close()

	svc := newVMService(t, &fakeVMRepository{get: provisionedVM()}, &fakeVMIPPoolRepository{},
		&fakeVMZoneRepository{}, &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}},
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	disk := int64(5)
	if _, err := svc.Resize(context.Background(), 1, nil, nil, &disk); !isKind(err, KindDiskShrinkNotAllowed) {
		t.Fatalf("err = %v, want KindDiskShrinkNotAllowed", err)
	}
}

// TestResizeNoOpEqualDisk verifies the documented equal-disk no-op: no PVE
// call and no spec persistence.
func TestResizeNoOpEqualDisk(t *testing.T) {
	ts := noCallServer(t)
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM()}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{},
		&fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}},
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	disk := int64(10) // equals the current 10G
	vm, err := svc.Resize(context.Background(), 1, nil, nil, &disk)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if vm.VM.DiskGB != 10 {
		t.Fatalf("disk_gb = %d, want 10", vm.VM.DiskGB)
	}
	if len(vmRepo.updatedSpecs) != 0 {
		t.Fatalf("UpdateSpec called for a no-op: %+v", vmRepo.updatedSpecs)
	}
}

// resizeServer answers PUT /config (synchronous), PUT /resize (UPID) and
// the resize task status.
func resizeServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/nodes/pve1/qemu/100/config" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"data": {"bootdisk": "scsi0", "scsi0": "local-ssd:vm-100-disk-0,size=10G"}}`)
		case r.URL.Path == "/nodes/pve1/qemu/100/config" && r.Method == http.MethodPut:
			fmt.Fprint(w, `{"data": null}`)
		case r.URL.Path == "/nodes/pve1/qemu/100/resize":
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:resize:100:root@pam:"}`)
		case strings.HasPrefix(r.URL.Path, "/nodes/pve1/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprint(w, `{"data": {"upid": "UPID:pve1:0:0:0:resize:100:root@pam:", "node": "pve1", "type": "resize", "id": "100", "user": "root@pam", "status": "stopped", "exitstatus": "OK"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func TestResizeGrowAll(t *testing.T) {
	ts := resizeServer(t)
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM()}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{}, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	cpu := 4
	mem := int64(4096)
	disk := int64(20)
	vm, err := svc.Resize(context.Background(), 1, &cpu, &mem, &disk)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if vm.VM.CPU != 4 || vm.VM.MemMB != 4096 || vm.VM.DiskGB != 20 {
		t.Fatalf("vm = %+v", vm.VM)
	}
	if len(vmRepo.updatedSpecs) != 1 {
		t.Fatalf("updated specs = %+v, want 1", vmRepo.updatedSpecs)
	}
	u := vmRepo.updatedSpecs[0]
	if u.new.cpu != 4 || u.new.memMB != 4096 || u.new.diskGB != 20 {
		t.Fatalf("new spec = %+v, want cpu=4 mem=4096 disk=20", u.new)
	}
	if u.old.cpu != 2 || u.old.memMB != 2048 || u.old.diskGB != 10 {
		t.Fatalf("old spec (optimistic lock baseline) = %+v, want cpu=2 mem=2048 disk=10", u.old)
	}
}

// TestResizeSpecConflict covers the optimistic-lock branch: a concurrent
// resize committed between the read and the persist, so UpdateSpec reports
// ErrSpecConflict and the caller surfaces KindConflict.
func TestResizeSpecConflict(t *testing.T) {
	ts := resizeServer(t)
	defer ts.Close()

	vmRepo := &fakeVMRepository{get: provisionedVM(), updateSpecErr: repository.ErrSpecConflict}
	nodeRepo := &fakeVMNodeRepository{nodes: []model.PVENode{{ID: 1, ZoneID: 1, Name: "pve1", Host: "h", APIUser: "root@pam", APITokenSecret: "spark=uuid"}}}
	svc := newVMService(t, vmRepo, &fakeVMIPPoolRepository{}, &fakeVMZoneRepository{}, nodeRepo,
		&fakeVMImageRepository{}, &fakeVMStorageTypeRepository{})
	svc.newClient = func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient("pve1", apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	}

	cpu := 4
	_, err := svc.Resize(context.Background(), 1, &cpu, nil, nil)
	if !isKind(err, KindConflict) {
		t.Fatalf("err = %v, want KindConflict", err)
	}
	if !strings.Contains(err.Error(), "规格已被并发修改") {
		t.Fatalf("err = %q, want the concurrent-modification message", err)
	}
}
