//go:build e2e

// End-to-end verification of the full VM lifecycle against a real PostgreSQL
// database and a fake PVE server (task 9.2): every step goes through the
// complete HTTP stack — gin router, handlers, services, repositories,
// real database, and an in-memory fake of the PVE JSON API pointed at via
// api.WithVMClientFactory (the pve client's base URL is injected with
// pve.WithBaseURL).
//
// Run with:
//
//	SPARK_E2E_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' \
//	  go test -tags=e2e ./e2e/ -count=1 -v
//
// The suite TRUNCATEs every business table (zones, nodes, ip pools, ips,
// storage types, images, vms) before and after itself, so it can share the
// database with the -tags=pg repository tests.
package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark/api"
	"spark/config"
	"spark/crypto"
	"spark/database"
	"spark/pve"
)

// ---------- fake PVE server ----------

// fakePVEVM is one VM as registered on the fake node. createBody keeps the
// raw POST /qemu payload so the test can assert the one-step create chain
// (scsi0 import-from, ide2 cloud-init, net0 vmbr0, cloud-init injection).
type fakePVEVM struct {
	vmid       int64
	name       string
	status     string
	config     map[string]string
	createBody map[string]any
}

// fakePVE is an in-memory implementation of the PVE JSON API endpoints the
// service uses. All state is guarded by mu: the app's detached provisioning
// goroutine and the test goroutine hit it concurrently.
type fakePVE struct {
	t *testing.T

	mu       sync.Mutex
	nextVMID int64
	vms      map[int64]*fakePVEVM
}

func newFakePVE(t *testing.T) *fakePVE {
	return &fakePVE{t: t, nextVMID: 100, vms: map[int64]*fakePVEVM{}}
}

// upid builds a parseable UPID whose node segment is the requesting node, so
// WaitTask polls /nodes/{node}/tasks/{upid}/status on the same fake.
func (f *fakePVE) upid(node, taskType string, vmid int64) string {
	return fmt.Sprintf("UPID:%s:00000E5B:01C9EC9E:5FAB1EC4:%s:%d:root@pam:", node, taskType, vmid)
}

// get returns a copy-safe pointer to the VM with the given vmid, or nil.
func (f *fakePVE) get(vmid int64) *fakePVEVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.vms[vmid]
	if !ok {
		return nil
	}
	cp := *vm
	cp.config = make(map[string]string, len(vm.config))
	for k, v := range vm.config {
		cp.config[k] = v
	}
	return &cp
}

// writeJSON answers the standard PVE envelope {"data": ...}.
func (f *fakePVE) writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		f.t.Errorf("fake pve: encode response: %v", err)
	}
}

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The pve client is pointed at http://{host}:{port}/api2/json, so every
	// request path carries the /api2/json prefix; strip it and dispatch.
	p := strings.TrimPrefix(r.URL.Path, "/api2/json")
	parts := strings.Split(strings.Trim(p, "/"), "/")

	switch {
	case p == "/version" && r.Method == http.MethodGet:
		f.writeJSON(w, map[string]any{"version": "8.2.7", "release": "8.2", "repoid": "fake"})
	case p == "/cluster/nextid" && r.Method == http.MethodGet:
		f.mu.Lock()
		vmid := f.nextVMID
		f.nextVMID++
		f.mu.Unlock()
		f.writeJSON(w, strconv.FormatInt(vmid, 10))
	case len(parts) == 3 && parts[0] == "nodes" && parts[2] == "qemu" && r.Method == http.MethodPost:
		f.handleCreate(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "nodes" && parts[2] == "qemu" && r.Method == http.MethodGet:
		f.handleList(w, parts[1])
	case len(parts) == 5 && parts[0] == "nodes" && parts[2] == "qemu" && parts[4] == "config" && r.Method == http.MethodGet:
		f.handleGetConfig(w, parts[3])
	case len(parts) == 5 && parts[0] == "nodes" && parts[2] == "qemu" && parts[4] == "config" && r.Method == http.MethodPut:
		f.handlePutConfig(w, r, parts[3])
	case len(parts) == 5 && parts[0] == "nodes" && parts[2] == "qemu" && parts[4] == "resize" && r.Method == http.MethodPut:
		f.handleResize(w, r, parts[1], parts[3])
	case len(parts) == 6 && parts[0] == "nodes" && parts[2] == "qemu" && parts[4] == "status" && r.Method == http.MethodPost:
		f.handleStatus(w, parts[1], parts[3], parts[5])
	case len(parts) == 4 && parts[0] == "nodes" && parts[2] == "qemu" && r.Method == http.MethodDelete:
		f.handleDelete(w, parts[1], parts[3])
	case len(parts) == 5 && parts[0] == "nodes" && parts[2] == "tasks" && parts[4] == "status" && r.Method == http.MethodGet:
		// Every fake task completes immediately with exitstatus OK.
		f.writeJSON(w, map[string]any{
			"upid": parts[3], "node": parts[1], "type": "fake", "id": "0",
			"user": "root@pam", "status": "stopped", "exitstatus": "OK",
		})
	default:
		f.t.Errorf("fake pve: unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		f.writeJSON(w, map[string]any{"_": "not found"})
	}
}

// handleCreate implements POST /nodes/{node}/qemu: it records the create
// parameters verbatim (scsi0/net0/ide2/ciuser/cipassword/ipconfig0/
// nameserver/bootdisk/scsihw), registers the VM as stopped and returns a
// qmcreate UPID. The scsi0 disk string gets a "size=10G" matching the test
// VM's requested disk_gb, so provisioning needs no resize.
func (f *fakePVE) handleCreate(w http.ResponseWriter, r *http.Request, node string) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("fake pve: decode create body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		f.writeJSON(w, map[string]any{"_": "bad body"})
		return
	}
	vmid := int64(body["vmid"].(float64))
	storage := "local-lvm"
	if s, ok := body["scsi0"].(string); ok {
		if i := strings.Index(s, ":"); i > 0 {
			storage = s[:i]
		}
	}
	config := map[string]string{
		"bootdisk":  "scsi0",
		"scsihw":    "virtio-scsi-pci",
		"scsi0":     fmt.Sprintf("%s:vm-%d-disk-0,size=10G", storage, vmid),
		"ide2":      "local-lvm:cloudinit",
		"net0":      "virtio,bridge=vmbr0",
		"ciuser":    "debian",
		"ipconfig0": "ip=10.0.0.2/24,gw=10.0.0.1",
	}
	for _, key := range []string{"name", "memory", "cores", "ide2", "net0", "ciuser", "cipassword", "ipconfig0", "nameserver", "bootdisk", "scsihw", "scsi0"} {
		if v, ok := body[key]; ok {
			config[key] = fmt.Sprintf("%v", v)
		}
	}
	vm := &fakePVEVM{
		vmid: vmid, name: fmt.Sprintf("%v", body["name"]), status: "stopped",
		config: config, createBody: body,
	}
	f.mu.Lock()
	f.vms[vmid] = vm
	f.mu.Unlock()
	f.writeJSON(w, f.upid(node, "qmcreate", vmid))
}

// handleList implements GET /nodes/{node}/qemu: the registered VMs with
// their live status fields.
func (f *fakePVE) handleList(w http.ResponseWriter, node string) {
	f.mu.Lock()
	list := make([]map[string]any, 0, len(f.vms))
	for _, vm := range f.vms {
		cores, _ := strconv.ParseInt(vm.config["cores"], 10, 64)
		memMB, _ := strconv.ParseInt(vm.config["memory"], 10, 64)
		diskGB := int64(0)
		if s, ok := strings.CutPrefix(vm.config["scsi0"], "size="); ok {
			if d, err := strconv.ParseInt(strings.TrimSuffix(s, "G"), 10, 64); err == nil {
				diskGB = d
			}
		}
		list = append(list, map[string]any{
			"vmid": vm.vmid, "name": vm.name, "status": vm.status,
			"cpus": cores, "maxmem": memMB << 20, "maxdisk": diskGB << 30,
		})
	}
	f.mu.Unlock()
	f.writeJSON(w, list)
}

func (f *fakePVE) handleGetConfig(w http.ResponseWriter, vmid string) {
	id, _ := strconv.ParseInt(vmid, 10, 64)
	vm := f.get(id)
	if vm == nil {
		w.WriteHeader(http.StatusNotFound)
		f.writeJSON(w, map[string]any{"_": fmt.Sprintf("VM %d not found", id)})
		return
	}
	f.writeJSON(w, vm.config)
}

// handlePutConfig implements PUT .../qemu/{vmid}/config: the endpoint is
// synchronous on PVE 7/8/9, so the reply is {"data": null}.
func (f *fakePVE) handlePutConfig(w http.ResponseWriter, r *http.Request, vmid string) {
	id, _ := strconv.ParseInt(vmid, 10, 64)
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("fake pve: decode config body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	vm := f.vms[id]
	if vm != nil {
		for _, key := range []string{"cores", "memory"} {
			if v, ok := body[key]; ok {
				vm.config[key] = fmt.Sprintf("%v", v)
			}
		}
	}
	f.mu.Unlock()
	f.writeJSON(w, nil)
}

// handleResize implements PUT .../qemu/{vmid}/resize: the disk string's size
// is replaced and a resize UPID is returned (PVE 8/9 semantics).
func (f *fakePVE) handleResize(w http.ResponseWriter, r *http.Request, node, vmid string) {
	id, _ := strconv.ParseInt(vmid, 10, 64)
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("fake pve: decode resize body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	disk, _ := body["disk"].(string)
	size, _ := body["size"].(string)
	f.mu.Lock()
	vm := f.vms[id]
	if vm != nil {
		cur := vm.config[disk]
		storage := "local-lvm"
		if i := strings.Index(cur, ":"); i > 0 {
			storage = cur[:i]
		}
		vm.config[disk] = fmt.Sprintf("%s:vm-%d-disk-0,size=%s", storage, id, size)
	}
	f.mu.Unlock()
	f.writeJSON(w, f.upid(node, "resize", id))
}

func (f *fakePVE) handleStatus(w http.ResponseWriter, node, vmid, action string) {
	id, _ := strconv.ParseInt(vmid, 10, 64)
	taskType := "qm" + action
	newStatus := map[string]string{"start": "running", "stop": "stopped", "reboot": "running"}[action]
	f.mu.Lock()
	if vm := f.vms[id]; vm != nil && newStatus != "" {
		vm.status = newStatus
	}
	f.mu.Unlock()
	f.writeJSON(w, f.upid(node, taskType, id))
}

func (f *fakePVE) handleDelete(w http.ResponseWriter, node, vmid string) {
	id, _ := strconv.ParseInt(vmid, 10, 64)
	f.mu.Lock()
	delete(f.vms, id)
	f.mu.Unlock()
	f.writeJSON(w, f.upid(node, "qmdestroy", id))
}

// ---------- helpers ----------

func e2eDSN() string {
	if dsn := os.Getenv("SPARK_E2E_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://spark:spark@127.0.0.1:5432/spark_test"
}

// e2eCipher builds a cipher from a deterministic 32-byte key (same pattern
// as the api/service unit tests).
func e2eCipher(t *testing.T) *crypto.Cipher {
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

// truncateBusinessTables wipes every business table in one statement.
// CASCADE follows the FK chain (zones -> ip_pools/pve_nodes/vms -> ips/
// ip_pool_nodes) and the statement lists the FK-root tables that vms
// references (storage_types, images); schema_migrations and schema_probe
// are untouched.
func truncateBusinessTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "TRUNCATE zones, storage_types, images CASCADE"); err != nil {
		t.Fatalf("truncate business tables: %v", err)
	}
}

// e2eDo performs one HTTP call against the test server and asserts the
// status code; the decoded JSON body is returned as any.
func e2eDo(t *testing.T, client *http.Client, base, method, path string, body any, want int) any {
	t.Helper()
	// io.Reader (not *strings.Reader): a typed nil would be non-nil to
	// http.NewRequest, which calls Len() on the strings.Reader and panics.
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s body: %v", method, path, err)
		}
		reader = strings.NewReader(string(data))
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		raw := make([]byte, 4096)
		n, _ := resp.Body.Read(raw)
		t.Fatalf("%s %s: status %d, want %d (body: %s)", method, path, resp.StatusCode, want, strings.TrimSpace(string(raw[:n])))
	}
	var out any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: decode body: %v", method, path, err)
	}
	return out
}

// e2eObj asserts that a decoded JSON body is an object and returns it as a
// map.
func e2eObj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("response is %T, want a JSON object", v)
	}
	return m
}

// ---------- the end-to-end scenario ----------

// TestE2EVMFullLifecycle runs the whole scenario of task 9.2 through the
// real HTTP stack: register zone/node/IP pool/storage type/image, create a
// VM (async provisioning against the fake PVE), exercise start/stop/restart,
// resize (shrink rejected, grow applied), verify the pass-through list and
// detail, destroy, and assert the IP was released and the row deleted.
func TestE2EVMFullLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	pool, err := database.New(ctx, e2eDSN())
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool, database.MigrationFS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Clean slate for a re-run after an aborted run, and again at the end.
	truncateBusinessTables(t, ctx, pool)
	defer truncateBusinessTables(t, ctx, pool)

	// The fake PVE server; the pve client's base URL is injected per node
	// via api.WithVMClientFactory + pve.WithBaseURL (the router otherwise
	// has no way to know the fake's address).
	fakePVE := newFakePVE(t)
	pveServer := httptest.NewServer(fakePVE)
	defer pveServer.Close()

	router := api.NewRouter(pool, e2eCipher(t),
		api.WithVMClientFactory(func(host, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient("fake-pve", apiUser, apiTokenSecret,
				pve.WithBaseURL(pveServer.URL+"/api2/json"),
				pve.WithHTTPClient(pveServer.Client()),
				pve.WithTimeout(5*time.Second))
		}))
	app := httptest.NewServer(router)
	defer app.Close()

	client := app.Client()
	base := app.URL

	// 1. Register the deployment: zone, node (host is only stored; the
	// client factory ignores it), IP pool + node whitelist, storage type,
	// image (present on the node).
	zone := e2eObj(t, e2eDo(t, client, base, http.MethodPost, "/zones", map[string]any{"name": "e2e-zone"}, http.StatusCreated))
	zoneID := int64(zone["id"].(float64))

	node := e2eObj(t, e2eDo(t, client, base, http.MethodPost,
		fmt.Sprintf("/zones/%d/nodes", zoneID),
		map[string]any{"name": "pve1", "host": "127.0.0.1", "api_user": "root@pam", "api_token": "spark=uuid"},
		http.StatusCreated))
	nodeID := int64(node["id"].(float64))

	poolRes := e2eObj(t, e2eDo(t, client, base, http.MethodPost, "/ip-pools", map[string]any{
		"zone_id": zoneID, "name": "e2e-pool", "network_cidr": "10.9.0.0/24",
		"gateway": "10.9.0.1", "dns": "1.1.1.1",
	}, http.StatusCreated))
	poolID := int64(poolRes["id"].(float64))

	e2eDo(t, client, base, http.MethodPut, fmt.Sprintf("/ip-pools/%d/nodes", poolID),
		map[string]any{"node_ids": []int64{nodeID}}, http.StatusOK)

	st := e2eObj(t, e2eDo(t, client, base, http.MethodPost, "/storage-types", map[string]any{
		"name": "ssd", "display_name": "SSD", "pve_storage": "local-lvm",
	}, http.StatusCreated))
	stID := int64(st["id"].(float64))

	img := e2eObj(t, e2eDo(t, client, base, http.MethodPost, "/images", map[string]any{
		"name": "debian-12-cloud", "default_user": "debian",
		"node_images": map[string]string{"pve1": "/var/lib/vz/images/debian-12-cloud.qcow2"},
	}, http.StatusCreated))
	imgID := int64(img["id"].(float64))

	// 2. Create the VM: 201, IP assigned, transitional "creating" status
	// (the PVE side does not exist yet).
	const (
		vmName  = "e2e-vm-1"
		vmPW    = "s3cret-pw"
		vmCPU   = 2
		vmMemMB = int64(2048)
		vmDisk  = int64(10)
	)
	created := e2eObj(t, e2eDo(t, client, base, http.MethodPost, "/vms", map[string]any{
		"name": vmName, "cpu": vmCPU, "mem_mb": vmMemMB, "disk_gb": vmDisk,
		"image_id": imgID, "storage_type_id": stID, "zone_id": zoneID, "password": vmPW,
	}, http.StatusCreated))
	vmID := int64(created["id"].(float64))
	vmIP, _ := created["ip"].(string)
	if vmIP == "" {
		t.Fatal("create response has no ip")
	}
	if created["status"] != "creating" {
		t.Fatalf("create status = %v, want creating", created["status"])
	}

	// 3. Poll until the detached provisioning chain finishes (status leaves
	// creating/failed, 15s budget); a failed provision is a test failure
	// with the recorded error printed.
	var detail map[string]any
	deadline := time.Now().Add(15 * time.Second)
	for {
		detail = e2eObj(t, e2eDo(t, client, base, http.MethodGet, fmt.Sprintf("/vms/%d", vmID), nil, http.StatusOK))
		switch detail["status"] {
		case "failed":
			t.Fatalf("provisioning failed: %v", detail["provision_error"])
		case "creating":
			if time.Now().After(deadline) {
				t.Fatalf("provisioning did not finish within 15s (last status %q)", detail["status"])
			}
			time.Sleep(200 * time.Millisecond)
		default:
			goto provisioned
		}
	}
provisioned:
	if detail["pve_vmid"] != float64(100) {
		t.Fatalf("pve_vmid = %v, want 100", detail["pve_vmid"])
	}
	if detail["status"] != "stopped" {
		t.Fatalf("status = %v, want stopped (freshly created)", detail["status"])
	}

	// 4. Assert the PVE-side create parameters recorded by the fake: the
	// one-step chain (scsi0 import-from + storage, ide2 cloudinit, net0
	// vmbr0, cloud-init injection with the image's default_user, the
	// allocated IP and the pool's gateway/DNS, bootdisk/scsihw).
	registered := fakePVE.get(100)
	if registered == nil {
		t.Fatal("fake pve has no VM 100")
	}
	body := registered.createBody
	assertCreate := func(key, want string) {
		t.Helper()
		if got := fmt.Sprintf("%v", body[key]); got != want {
			t.Fatalf("create %s = %q, want %q", key, got, want)
		}
	}
	scsi0 := fmt.Sprintf("%v", body["scsi0"])
	if !strings.HasPrefix(scsi0, "local-lvm:") || !strings.Contains(scsi0, "import-from=/var/lib/vz/images/debian-12-cloud.qcow2") {
		t.Fatalf("scsi0 = %q, want local-lvm storage with import-from", scsi0)
	}
	assertCreate("ide2", "local-lvm:cloudinit")
	if net0 := fmt.Sprintf("%v", body["net0"]); !strings.Contains(net0, "vmbr0") {
		t.Fatalf("net0 = %q, want vmbr0 bridge", net0)
	}
	assertCreate("ciuser", "debian")
	assertCreate("cipassword", vmPW)
	ipconfig0 := fmt.Sprintf("%v", body["ipconfig0"])
	if !strings.Contains(ipconfig0, "ip="+vmIP+"/24") || !strings.Contains(ipconfig0, "gw=10.9.0.1") {
		t.Fatalf("ipconfig0 = %q, want ip=%s/24 with gw=10.9.0.1", ipconfig0, vmIP)
	}
	assertCreate("nameserver", "1.1.1.1")
	assertCreate("bootdisk", "scsi0")
	assertCreate("scsihw", "virtio-scsi-pci")

	// 5. Lifecycle: start -> PVE running, stop -> stopped, restart ->
	// running.
	e2eDo(t, client, base, http.MethodPost, fmt.Sprintf("/vms/%d/start", vmID), nil, http.StatusAccepted)
	if s := fakePVE.get(100); s == nil || s.status != "running" {
		t.Fatalf("fake pve status after start = %+v, want running", s)
	}
	e2eDo(t, client, base, http.MethodPost, fmt.Sprintf("/vms/%d/stop", vmID), nil, http.StatusAccepted)
	if s := fakePVE.get(100); s == nil || s.status != "stopped" {
		t.Fatalf("fake pve status after stop = %+v, want stopped", s)
	}
	e2eDo(t, client, base, http.MethodPost, fmt.Sprintf("/vms/%d/restart", vmID), nil, http.StatusAccepted)
	if s := fakePVE.get(100); s == nil || s.status != "running" {
		t.Fatalf("fake pve status after restart = %+v, want running", s)
	}

	// 6. Resize: shrinking the disk is refused with 422; growing cpu/mem/
	// disk succeeds and is applied on the PVE side (config + resize).
	e2eDo(t, client, base, http.MethodPost, fmt.Sprintf("/vms/%d/resize", vmID),
		map[string]any{"disk_gb": 5}, http.StatusUnprocessableEntity)

	resized := e2eObj(t, e2eDo(t, client, base, http.MethodPost, fmt.Sprintf("/vms/%d/resize", vmID),
		map[string]any{"cpu": 4, "mem_mb": 4096, "disk_gb": 20}, http.StatusOK))
	if resized["cpu"] != float64(4) || resized["mem_mb"] != float64(4096) || resized["disk_gb"] != float64(20) {
		t.Fatalf("resized vm = %+v, want cpu=4 mem=4096 disk=20", resized)
	}
	if resized["status"] != "running" {
		t.Fatalf("resized status = %v, want running (pass-through)", resized["status"])
	}
	cfg := fakePVE.get(100).config
	if cfg["cores"] != "4" || cfg["memory"] != "4096" {
		t.Fatalf("pve config after resize = %+v, want cores=4 memory=4096", cfg)
	}
	if !strings.HasSuffix(cfg["scsi0"], "size=20G") {
		t.Fatalf("pve scsi0 after resize = %q, want size=20G", cfg["scsi0"])
	}

	// 7. Pass-through list and detail: the VM appears with the live PVE
	// status (running after the restart).
	list := e2eObj(t, e2eDo(t, client, base, http.MethodGet, "/vms", nil, http.StatusOK))
	found := false
	for _, raw := range list["vms"].([]any) {
		item := raw.(map[string]any)
		if int64(item["id"].(float64)) == vmID {
			found = true
			if item["status"] != "running" {
				t.Fatalf("list status = %v, want running (pass-through)", item["status"])
			}
		}
	}
	if !found {
		t.Fatal("GET /vms does not contain the created VM")
	}
	detail = e2eObj(t, e2eDo(t, client, base, http.MethodGet, fmt.Sprintf("/vms/%d", vmID), nil, http.StatusOK))
	if detail["status"] != "running" {
		t.Fatalf("detail status = %v, want running", detail["status"])
	}

	// 8. Destroy: PVE VM removed, IP released back to free, vms row gone.
	e2eDo(t, client, base, http.MethodPost, fmt.Sprintf("/vms/%d/destroy", vmID), nil, http.StatusOK)
	if got := fakePVE.get(100); got != nil {
		t.Fatalf("fake pve still has VM 100 after destroy: %+v", got)
	}

	var ipStatus string
	var ipVMID *int64
	if err := pool.QueryRow(ctx, "SELECT status, vm_id FROM ips WHERE ip=$1", vmIP).Scan(&ipStatus, &ipVMID); err != nil {
		t.Fatalf("query released ip %s: %v", vmIP, err)
	}
	if ipStatus != "free" || ipVMID != nil {
		t.Fatalf("released ip %s: status=%q vm_id=%v, want free/nil", vmIP, ipStatus, ipVMID)
	}

	var vmCount int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM vms WHERE id=$1", vmID).Scan(&vmCount); err != nil {
		t.Fatalf("query vms after destroy: %v", err)
	}
	if vmCount != 0 {
		t.Fatalf("vms row count = %d after destroy, want 0", vmCount)
	}
}
