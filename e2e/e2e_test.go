//go:build e2e

// 针对真实 PostgreSQL 数据库和 fake PVE 服务器（任务 9.2）的完整虚拟机生命周期端到端验证：
// 每个步骤都会走完完整的 HTTP 链路——gin 路由、处理器、服务层、仓储层、
// 真实数据库，以及通过 api.WithVMClientFactory 注入的内存版 PVE JSON API 模拟
// （节点以 host:port 登记，客户端通过 pve.WithPort 真实连接 fake PVE 的监听端口）。
//
// 运行方式：
//
//	SPARK_E2E_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' \
//	  go test -tags=e2e ./e2e/ -count=1 -v
//
// 该测试套件会在运行前后 TRUNCATE 所有业务表（zones、nodes、ip pools、ips、
// storage types、images、vms），因此它可以与 -tags=pg 仓储测试共享同一个数据库。
package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

// ---------- fake PVE 服务器 ----------

// fakePVEVM 是注册在 fake 节点上的一台虚拟机。createBody 保留了
// POST /qemu 的原始请求体，以便测试可以断言一步式创建链路
// （scsi0 import-from、ide2 cloud-init、net0 vmbr0、cloud-init 注入）。
type fakePVEVM struct {
	vmid       int64
	name       string
	status     string
	config     map[string]string
	createBody map[string]any
}

// fakePVE 是服务所依赖的 PVE JSON API 端点的内存实现。
// 所有状态都由 mu 保护：应用的后台配置 goroutine 与测试 goroutine 会并发访问它。
type fakePVE struct {
	t *testing.T

	mu       sync.Mutex
	nextVMID int64
	vms      map[int64]*fakePVEVM
}

func newFakePVE(t *testing.T) *fakePVE {
	return &fakePVE{t: t, nextVMID: 100, vms: map[int64]*fakePVEVM{}}
}

// upid 构造一个可解析的 UPID，其节点段为发起请求的节点，这样
// WaitTask 会在同一个 fake 上轮询 /nodes/{node}/tasks/{upid}/status。
func (f *fakePVE) upid(node, taskType string, vmid int64) string {
	return fmt.Sprintf("UPID:%s:00000E5B:01C9EC9E:5FAB1EC4:%s:%d:root@pam:", node, taskType, vmid)
}

// get 返回指定 vmid 对应虚拟机的拷贝安全指针，若不存在则返回 nil。
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

// writeJSON 以标准的 PVE 信封格式 {"data": ...} 返回响应。
func (f *fakePVE) writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		f.t.Errorf("fake pve: encode response: %v", err)
	}
}

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// pve 客户端指向 http://{host}:{port}/api2/json，因此每个
	// 请求路径都带有 /api2/json 前缀；去掉该前缀后再进行分发。
	p := strings.TrimPrefix(r.URL.Path, "/api2/json")
	parts := strings.Split(strings.Trim(p, "/"), "/")

	switch {
	case p == "/version" && r.Method == http.MethodGet:
		f.writeJSON(w, map[string]any{"version": "8.2.7", "release": "8.2", "repoid": "fake"})
	case p == "/nodes" && r.Method == http.MethodGet:
		// 集群节点名列表（任务 4.1 探测入口）：fake 集群只有一个节点 pve1。
		f.writeJSON(w, []map[string]any{{"node": "pve1", "status": "online"}})
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
		// 所有 fake 任务都会立即以 exitstatus OK 完成。
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

// handleCreate 实现 POST /nodes/{node}/qemu：它会原样记录创建
// 参数（scsi0/net0/ide2/ciuser/cipassword/ipconfig0/
// nameserver/bootdisk/scsihw），将虚拟机注册为 stopped 状态并返回一个
// qmcreate UPID。scsi0 磁盘字符串带有与测试虚拟机请求的 disk_gb
// 匹配的 "size=10G"，因此配置过程无需执行 resize。
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

// handleList 实现 GET /nodes/{node}/qemu：返回已注册的虚拟机及其
// 实时状态字段。
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

// handlePutConfig 实现 PUT .../qemu/{vmid}/config：在 PVE 7/8/9 上该端点是
// 同步的，因此响应为 {"data": null}。
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

// handleResize 实现 PUT .../qemu/{vmid}/resize：磁盘字符串的 size 部分
// 会被替换，并返回一个 resize UPID（PVE 8/9 语义）。
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

// registerVM 直接向 fake 节点注册一台 stopped 状态的 VM，用于模拟 PVE 上
// 手工创建的已有虚拟机（"导入已有 VM"场景）。config 至少携带
// name/cores/memory/scsi0（带 size=..G 字段）与 ipconfig0（静态 IP 声明）；
// 与 handleCreate 不同，它不经过 POST /qemu 链路，也不推进 nextVMID，
// 因此注册的 vmid 需要调用方自行避开 100（一步式创建测试已使用）。
func (f *fakePVE) registerVM(vmid int64, name string, config map[string]string) {
	f.mu.Lock()
	f.vms[vmid] = &fakePVEVM{vmid: vmid, name: name, status: "stopped", config: config}
	f.mu.Unlock()
}

// ---------- 辅助函数 ----------

func e2eDSN() string {
	if dsn := os.Getenv("SPARK_E2E_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://spark:spark@127.0.0.1:5432/spark_test"
}

// e2eCipher 使用确定的 32 字节密钥构造 cipher（与 api/service 单元测试
// 相同的模式）。
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

// truncateBusinessTables 用一条语句清空所有业务表。
// CASCADE 会顺着外键链（zones -> ip_pools/pve_nodes/vms -> ips/
// ip_pool_nodes）级联删除，语句中还列出了 vms 所引用的外键根表
// （storage_types、images）；schema_migrations 和 schema_probe
// 不会被触碰。
func truncateBusinessTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "TRUNCATE zones, storage_types, images CASCADE"); err != nil {
		t.Fatalf("truncate business tables: %v", err)
	}
}

// e2eDo 对测试服务器执行一次 HTTP 调用并断言
// 状态码；解码后的 JSON body 以 any 类型返回。
func e2eDo(t *testing.T, client *http.Client, base, method, path string, body any, want int) any {
	t.Helper()
	// 使用 io.Reader（而非 *strings.Reader）：类型化的 nil 对 http.NewRequest
	// 来说是非 nil 的，它会调用 strings.Reader 的 Len() 从而引发 panic。
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
	if resp.StatusCode == http.StatusNoContent {
		// 按约定 204 无响应体；调用方将得到 nil。
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: decode body: %v", method, path, err)
	}
	return out
}

// e2eObj 断言解码后的 JSON body 是一个对象，并以 map 形式返回。
func e2eObj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("response is %T, want a JSON object", v)
	}
	return m
}

// ---------- 端到端场景 ----------

// TestE2EVMFullLifecycle 通过完整的 HTTP 链路运行任务 9.2 的整个场景：
// 注册 zone/node/IP 池/存储类型/镜像，创建虚拟机（在 fake PVE 上异步配置），
// 执行 start/stop/restart，resize（缩小被 422 拒绝、扩容生效），
// 验证透传的列表与详情，销毁，并断言 IP 已被释放、数据库行已被删除。
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

	// 为中断后的重跑清空数据，结束时再清空一次。
	truncateBusinessTables(t, ctx, pool)
	defer truncateBusinessTables(t, ctx, pool)

	// fake PVE 服务器：节点以 host:port 登记，客户端工厂通过
	// pve.WithPort 将请求真实打到 fake 的监听端口（自定义端口链路，任务 6.3）。
	fakePVE := newFakePVE(t)
	pveServer := httptest.NewTLSServer(fakePVE)
	defer pveServer.Close()
	pvePort := pveServer.Listener.Addr().(*net.TCPAddr).Port

	router := api.NewRouter(pool, e2eCipher(t),
		// 工厂签名携带 host/port（任务 4.3）：host 是节点登记的纯地址，
		// port 是节点的 API 端口。客户端按默认 https 构造 base URL 后由
		// WithPort 覆盖端口，请求真实打到 fake PVE 的监听端口（任务 6.3）。
		api.WithVMClientFactory(func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient(host, apiUser, apiTokenSecret,
				pve.WithPort(port),
				pve.WithHTTPClient(pveServer.Client()),
				pve.WithTimeout(5*time.Second))
		}))
	app := httptest.NewServer(router)
	defer app.Close()

	client := app.Client()
	base := app.URL

	// 1. 注册部署环境：zone、node（host 携带 fake PVE 的监听端口）、
	// IP 池 + 节点白名单、存储类型、镜像（在节点上存在）。
	zone := e2eObj(t, e2eDo(t, client, base, http.MethodPost, "/zones", map[string]any{"name": "e2e-zone"}, http.StatusCreated))
	zoneID := int64(zone["id"].(float64))

	// 1a. 业务名与 fake 集群真实节点名（只有 pve1）不一致 -> 503 被拒，
	// 错误消息提示集群真实名；登记走的是生产默认探测实现（真实连接 fake
	// PVE 的 /nodes），因此同时验证了探测链路。
	mismatch := e2eObj(t, e2eDo(t, client, base, http.MethodPost,
		fmt.Sprintf("/zones/%d/nodes", zoneID),
		map[string]any{"name": "aeolian", "host": fmt.Sprintf("127.0.0.1:%d", pvePort), "api_user": "root@pam", "api_token": "spark=uuid"},
		http.StatusServiceUnavailable))
	mismatchErr := e2eObj(t, mismatch["error"])
	if code, _ := mismatchErr["code"].(string); code != "node_unavailable" {
		t.Fatalf("mismatch rejection code = %q, want node_unavailable", code)
	}
	if msg, _ := mismatchErr["message"].(string); !strings.Contains(msg, "pve1") {
		t.Fatalf("mismatch rejection message = %q, want the cluster node name pve1", msg)
	}

	node := e2eObj(t, e2eDo(t, client, base, http.MethodPost,
		fmt.Sprintf("/zones/%d/nodes", zoneID),
		map[string]any{"name": "pve1", "host": fmt.Sprintf("127.0.0.1:%d", pvePort), "api_user": "root@pam", "api_token": "spark=uuid"},
		http.StatusCreated))
	nodeID := int64(node["id"].(float64))

	// host:port 登记后，创建响应回显剥离端口后的 host 与解析出的 port，
	// 且 pve_name 与业务名一致（集群名探测匹配，任务 4.1）。
	if node["host"] != "127.0.0.1" || node["port"] != float64(pvePort) {
		t.Fatalf("node = %+v, want host=127.0.0.1 port=%d", node, pvePort)
	}
	if node["pve_name"] != "pve1" {
		t.Fatalf("node pve_name = %v, want pve1 (matched against the cluster)", node["pve_name"])
	}

	// 节点列表同样回显 port（请求确实打到该端口，由后续 VM 生命周期链路
	// 经同一客户端工厂隐式验证）。
	listed := e2eDo(t, client, base, http.MethodGet, fmt.Sprintf("/zones/%d/nodes", zoneID), nil, http.StatusOK).([]any)
	if len(listed) != 1 {
		t.Fatalf("GET nodes = %+v, want 1 node", listed)
	}
	listedNode := e2eObj(t, listed[0])
	if listedNode["host"] != "127.0.0.1" || listedNode["port"] != float64(pvePort) {
		t.Fatalf("listed node = %+v, want host=127.0.0.1 port=%d", listedNode, pvePort)
	}

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

	// 2. 创建虚拟机：201、已分配 IP、过渡状态 "creating"
	// （PVE 侧此时还不存在）。
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

	// 3. 轮询直至后台配置链路完成（状态离开 creating/failed，
	// 预算 15 秒）；配置失败即为测试失败，并打印记录的错误。
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

	// 4. 断言 fake 记录下来的 PVE 侧创建参数：一步式链路
	// （scsi0 import-from + 存储、ide2 cloudinit、net0 vmbr0、
	// 使用镜像 default_user 的 cloud-init 注入、分配的 IP 及池的
	// gateway/DNS、bootdisk/scsihw）。
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

	// 5. 生命周期：start -> PVE running、stop -> stopped、restart ->
	// running。
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

	// 6. Resize：缩小磁盘会被 422 拒绝；增大 cpu/mem/
	// disk 会成功，并在 PVE 侧生效（config + resize）。
	// 该操作是对虚拟机资源的 PATCH（JSON Merge Patch 语义：
	// 未出现的字段保持当前值）。
	e2eDo(t, client, base, http.MethodPatch, fmt.Sprintf("/vms/%d", vmID),
		map[string]any{"disk_gb": 5}, http.StatusUnprocessableEntity)

	resized := e2eObj(t, e2eDo(t, client, base, http.MethodPatch, fmt.Sprintf("/vms/%d", vmID),
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

	// 7. 透传的列表与详情：虚拟机以实时 PVE
	// 状态出现（restart 后为 running）。
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

	// 7b. 分页：GET /vms?limit=1&offset=0 最多返回一个
	// 条目，并携带 X-Total-Count 响应头，其值为真实总数（在当前场景
	// 阶段 >= 1，因为已创建的虚拟机在数据库中）。
	pagedReq, err := http.NewRequest(http.MethodGet, base+"/vms?limit=1&offset=0", nil)
	if err != nil {
		t.Fatalf("build paginated list request: %v", err)
	}
	pagedResp, err := client.Do(pagedReq)
	if err != nil {
		t.Fatalf("GET /vms?limit=1&offset=0: %v", err)
	}
	defer pagedResp.Body.Close()
	if pagedResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /vms?limit=1&offset=0: status %d, want 200", pagedResp.StatusCode)
	}
	totalRaw := pagedResp.Header.Get("X-Total-Count")
	total, err := strconv.Atoi(totalRaw)
	if err != nil || total < 1 {
		t.Fatalf("X-Total-Count = %q, want an integer >= 1", totalRaw)
	}
	var paged map[string]any
	if err := json.NewDecoder(pagedResp.Body).Decode(&paged); err != nil {
		t.Fatalf("decode paginated list: %v", err)
	}
	pagedItems, ok := paged["vms"].([]any)
	if !ok {
		t.Fatalf("paginated list response = %+v, want a vms array", paged)
	}
	if len(pagedItems) > 1 {
		t.Fatalf("GET /vms?limit=1&offset=0 returned %d items, want <= 1", len(pagedItems))
	}
	if len(pagedItems) == 1 {
		item := pagedItems[0].(map[string]any)
		if int64(item["id"].(float64)) != vmID {
			t.Fatalf("paginated page contains vm %v, want %d", item["id"], vmID)
		}
	}

	// 8. 销毁：DELETE /vms/:id 返回 204 且无响应体；PVE 虚拟机被
	// 删除，IP 释放回 free，vms 数据行消失。
	e2eDo(t, client, base, http.MethodDelete, fmt.Sprintf("/vms/%d", vmID), nil, http.StatusNoContent)
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

	// ---------- 导入已有 VM（feat/import-existing-vms） ----------

	// 9. 预置：在 fake PVE 上注册一台手工创建的 VM（vmid=200，避开
	// 已创建并销毁的 100）。静态 IP 10.9.0.10 落在 e2e-pool 网段内：
	// 池创建时 expandPoolIPs 会物化 10.9.0.0/24 除网络地址、广播地址
	// 与网关（10.9.0.1）外的全部地址，因此导入时应精确复用该地址。
	fakePVE.registerVM(200, "imported-vm", map[string]string{
		"name":      "imported-vm",
		"cores":     "1",
		"memory":    "1024",
		"scsi0":     "local-lvm:vm-200-disk-0,size=20G",
		"ipconfig0": "ip=10.9.0.10/24,gw=10.9.0.1",
	})

	// 10. 候选查询：GET /vms/unmanaged 返回未托管候选，vmid=200
	// 应出现（此前无托管 VM，故无过滤）。
	unmanaged := e2eObj(t, e2eDo(t, client, base, http.MethodGet,
		fmt.Sprintf("/vms/unmanaged?node_id=%d", nodeID), nil, http.StatusOK))
	unmanagedVMs := unmanaged["vms"].([]any)
	foundImport := false
	for _, raw := range unmanagedVMs {
		item := e2eObj(t, raw)
		if int64(item["vmid"].(float64)) == 200 {
			foundImport = true
			if item["name"] != "imported-vm" || item["status"] != "stopped" {
				t.Fatalf("unmanaged candidate 200 = %+v, want name=imported-vm status=stopped", item)
			}
		}
	}
	if !foundImport {
		t.Fatalf("GET /vms/unmanaged candidates = %+v, want vmid 200", unmanagedVMs)
	}

	// 11. 导入：POST /vms/import -> 201 + Location + 完整 VMListItem。
	// 需要读取响应头，故不走 e2eDo 而手动构造请求。
	importReq, err := http.NewRequest(http.MethodPost, base+"/vms/import", strings.NewReader(
		fmt.Sprintf(`{"zone_id":%d,"node_id":%d,"pve_vmid":200}`, zoneID, nodeID)))
	if err != nil {
		t.Fatalf("build import request: %v", err)
	}
	importReq.Header.Set("Content-Type", "application/json")
	importResp, err := client.Do(importReq)
	if err != nil {
		t.Fatalf("POST /vms/import: %v", err)
	}
	defer importResp.Body.Close()
	if importResp.StatusCode != http.StatusCreated {
		raw := make([]byte, 4096)
		n, _ := importResp.Body.Read(raw)
		t.Fatalf("POST /vms/import: status %d, want 201 (body: %s)", importResp.StatusCode, strings.TrimSpace(string(raw[:n])))
	}
	var importBody any
	if err := json.NewDecoder(importResp.Body).Decode(&importBody); err != nil {
		t.Fatalf("POST /vms/import: decode body: %v", err)
	}
	imported := e2eObj(t, importBody)
	importedID := int64(imported["id"].(float64))
	if loc := importResp.Header.Get("Location"); loc != fmt.Sprintf("/vms/%d", importedID) {
		t.Fatalf("import Location = %q, want /vms/%d", loc, importedID)
	}
	if imported["pve_vmid"] != float64(200) {
		t.Fatalf("import pve_vmid = %v, want 200", imported["pve_vmid"])
	}
	if imported["ip"] != "10.9.0.10" {
		t.Fatalf("import ip = %v, want 10.9.0.10 (静态 IP 精确复用)", imported["ip"])
	}
	// 导入的 VM 没有本地镜像/存储绑定：image_id、storage_type_id
	// 应为 null（不出现）而非任意数值。
	for _, key := range []string{"image_id", "storage_type_id"} {
		if v, ok := imported[key]; ok && v != nil {
			t.Fatalf("import %s = %v, want null/absent", key, v)
		}
	}
	// 规格来自 PVE config 解析（vmConfigSpec：cores/memory/scsi0 size）。
	if imported["cpu"] != float64(1) || imported["mem_mb"] != float64(1024) || imported["disk_gb"] != float64(20) {
		t.Fatalf("import spec = cpu=%v mem_mb=%v disk_gb=%v, want 1/1024/20", imported["cpu"], imported["mem_mb"], imported["disk_gb"])
	}
	// 导入是同步的，状态从 PVE 实时透传：已存在的 stopped VM 即 stopped。
	if imported["status"] != "stopped" {
		t.Fatalf("import status = %v, want stopped (透传)", imported["status"])
	}

	// 12. 幂等：同一节点上的同一 pve_vmid 重复导入 -> 409
	// vm_already_managed。
	idem := e2eObj(t, e2eDo(t, client, base, http.MethodPost, "/vms/import",
		map[string]any{"zone_id": zoneID, "node_id": nodeID, "pve_vmid": 200},
		http.StatusConflict))
	idemErr := e2eObj(t, idem["error"])
	if code, _ := idemErr["code"].(string); code != "vm_already_managed" {
		t.Fatalf("idempotent import code = %q, want vm_already_managed", code)
	}

	// 13. 候选过滤：导入后 vmid=200 不再出现在未托管列表。
	unmanaged = e2eObj(t, e2eDo(t, client, base, http.MethodGet,
		fmt.Sprintf("/vms/unmanaged?node_id=%d", nodeID), nil, http.StatusOK))
	for _, raw := range unmanaged["vms"].([]any) {
		item := e2eObj(t, raw)
		if int64(item["vmid"].(float64)) == 200 {
			t.Fatalf("unmanaged candidates after import = %+v, want vmid 200 filtered out", unmanaged["vms"])
		}
	}

	// 14. 列表与详情：GET /vms 出现新导入的 VM（id 匹配、status
	// 透传为 stopped）；GET /vms/:id 的 image_id/storage_type_id 可空。
	list = e2eObj(t, e2eDo(t, client, base, http.MethodGet, "/vms", nil, http.StatusOK))
	found = false
	for _, raw := range list["vms"].([]any) {
		item := raw.(map[string]any)
		if int64(item["id"].(float64)) == importedID {
			found = true
			if item["status"] != "stopped" {
				t.Fatalf("list status of imported vm = %v, want stopped (透传)", item["status"])
			}
		}
	}
	if !found {
		t.Fatal("GET /vms does not contain the imported VM")
	}
	importedDetail := e2eObj(t, e2eDo(t, client, base, http.MethodGet, fmt.Sprintf("/vms/%d", importedID), nil, http.StatusOK))
	if importedDetail["status"] != "stopped" {
		t.Fatalf("detail status of imported vm = %v, want stopped", importedDetail["status"])
	}
	for _, key := range []string{"image_id", "storage_type_id"} {
		if v, ok := importedDetail[key]; ok && v != nil {
			t.Fatalf("detail %s = %v, want null/absent", key, v)
		}
	}

	// 15. 生命周期：导入即托管，start/resize 直接生效——
	// start -> PVE running；PATCH cpu=2 -> PVE config cores=2。
	e2eDo(t, client, base, http.MethodPost, fmt.Sprintf("/vms/%d/start", importedID), nil, http.StatusAccepted)
	if s := fakePVE.get(200); s == nil || s.status != "running" {
		t.Fatalf("fake pve status of imported vm after start = %+v, want running", s)
	}
	e2eDo(t, client, base, http.MethodPatch, fmt.Sprintf("/vms/%d", importedID),
		map[string]any{"cpu": 2}, http.StatusOK)
	if cfg := fakePVE.get(200).config; cfg["cores"] != "2" {
		t.Fatalf("pve config after import resize = %+v, want cores=2", cfg)
	}

	// 16. 销毁：DELETE /vms/:id -> 204；PVE 虚拟机被删除，静态复用
	// 的 IP 10.9.0.10 释放回 free，vms 数据行消失。
	e2eDo(t, client, base, http.MethodDelete, fmt.Sprintf("/vms/%d", importedID), nil, http.StatusNoContent)
	if got := fakePVE.get(200); got != nil {
		t.Fatalf("fake pve still has VM 200 after destroy: %+v", got)
	}
	if err := pool.QueryRow(ctx, "SELECT status, vm_id FROM ips WHERE ip=$1", "10.9.0.10").Scan(&ipStatus, &ipVMID); err != nil {
		t.Fatalf("query released ip 10.9.0.10: %v", err)
	}
	if ipStatus != "free" || ipVMID != nil {
		t.Fatalf("released ip 10.9.0.10: status=%q vm_id=%v, want free/nil", ipStatus, ipVMID)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM vms WHERE id=$1", importedID).Scan(&vmCount); err != nil {
		t.Fatalf("query vms after import destroy: %v", err)
	}
	if vmCount != 0 {
		t.Fatalf("vms row count = %d after import destroy, want 0", vmCount)
	}
}
