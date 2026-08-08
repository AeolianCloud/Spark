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
	"spark/service"
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
	// template 标记该 VM 是 PVE 模板（供克隆使用的基础镜像）：列表合并
	// 会排除它，handleList 以 template=1 输出供透传路径识别。
	template bool
}

// fakePVE 是服务所依赖的 PVE JSON API 端点的内存实现。
// 所有状态都由 mu 保护：应用的后台配置 goroutine 与测试 goroutine 会并发访问它。
type fakePVE struct {
	t *testing.T

	mu       sync.Mutex
	nextVMID int64
	vms      map[int64]*fakePVEVM
	// statusErrors 让指定 vmid 的 status 操作（start/stop/reboot）返回
	// PVE 错误（HTTP 500 + errors 封装）：模拟 PVE 拒绝操作，供"失败
	// 操作也写入审计记录"的端到端断言使用。值为 PVE 错误消息。
	statusErrors map[int64]string
	// importFiles 模拟各存储上已下载完成的镜像文件（键为存储名，如
	// "local"）。镜像重构后节点上的镜像存在性由 PVE 实时扫描
	// GET /nodes/{node}/storage/{storage}/content?content=import 得出，
	// 下载经 POST .../download-url 异步完成——fake 用该 map 模拟下载
	// 结果，创建 VM 时的节点选择（selectPoolAndNode 按文件名匹配）与
	// 镜像存在状态查询都依赖它。当前 fake 只有单节点 pve1，按存储名
	// 存即可；若将来模拟多节点，需改为按"节点名+存储名"存储（不同
	// 节点的存储互不可见）。
	importFiles map[string][]string
}

func newFakePVE(t *testing.T) *fakePVE {
	return &fakePVE{t: t, nextVMID: 100, vms: map[int64]*fakePVEVM{}, statusErrors: map[int64]string{}, importFiles: map[string][]string{}}
}

// addImportFile 向指定存储登记一个已下载完成的镜像文件（模拟 PVE 侧
// 下载任务完成后的结果，与 handleDownloadURL 写入同一份 importFiles
// 状态）。之后对 /content?content=import 的扫描即可看到该文件，创建 VM
// 的节点选择才会放行。
func (f *fakePVE) addImportFile(storage, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.importFiles[storage] = append(f.importFiles[storage], name)
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
	case len(parts) == 5 && parts[0] == "nodes" && parts[2] == "storage" && parts[4] == "content" && r.Method == http.MethodGet:
		f.handleStorageContent(w, r, parts[3])
	case len(parts) == 5 && parts[0] == "nodes" && parts[2] == "storage" && parts[4] == "download-url" && r.Method == http.MethodPost:
		f.handleDownloadURL(w, r, parts[1], parts[3])
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
// 实时状态字段（含 template 标记，供列表排除 PVE 模板）。
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
		template := 0
		if vm.template {
			template = 1
		}
		list = append(list, map[string]any{
			"vmid": vm.vmid, "name": vm.name, "status": vm.status,
			"cpus": cores, "maxmem": memMB << 20, "maxdisk": diskGB << 30,
			"template": template,
		})
	}
	f.mu.Unlock()
	f.writeJSON(w, list)
}

// handleStorageContent 实现 GET /nodes/{node}/storage/{storage}/content：
// content=import（或未指定）时返回该存储上"已下载完成"的镜像文件条目
// （importFiles 模拟的 PVE 下载结果，volid 为 {storage}:import/{name}），
// 其它 content 类型（iso/vztmpl 等）返回空数组——本仓库只关心 import
// 目录。响应条目按真实 PVE 格式返回（无 name 字段，已实测），文件名由
// pve 层从 volid 推导，以覆盖真实环境形态、防止回归。
func (f *fakePVE) handleStorageContent(w http.ResponseWriter, r *http.Request, storage string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ct := r.URL.Query().Get("content"); ct != "" && ct != "import" {
		f.writeJSON(w, []any{})
		return
	}
	items := make([]map[string]any, 0, len(f.importFiles[storage]))
	for _, name := range f.importFiles[storage] {
		items = append(items, map[string]any{
			"volid":   fmt.Sprintf("%s:import/%s", storage, name),
			"content": "import",
			"format":  "qcow2",
			"size":    0,
		})
	}
	f.writeJSON(w, items)
}

// handleDownloadURL 实现 POST /nodes/{node}/storage/{storage}/download-url：
// 解析 form 参数（content/filename/url），把 filename 加入 importFiles
// 模拟下载任务立即完成，并返回一个 download 类型的 UPID（WaitTask 轮询
// status 端点会立即得到 done）。filename 以 "e2e-fail-" 开头时返回
// HTTP 500 + errors 封装，模拟 PVE 拒绝受理，供下载失败路径的端到端
// 断言使用。
func (f *fakePVE) handleDownloadURL(w http.ResponseWriter, r *http.Request, node, storage string) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("fake pve: parse download-url form: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		f.writeJSON(w, map[string]any{"_": "bad form"})
		return
	}
	filename := r.FormValue("filename")
	if strings.HasPrefix(filename, "e2e-fail-") {
		w.WriteHeader(http.StatusInternalServerError)
		f.writeJSON(w, map[string]any{"errors": map[string]string{"_": "simulated download failure"}})
		return
	}
	if filename != "" {
		f.addImportFile(storage, filename)
	}
	f.writeJSON(w, f.upid(node, "download", 0))
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
	if msg, ok := f.statusErrors[id]; ok {
		f.mu.Unlock()
		// 模拟 PVE 拒绝操作（HTTP 500 + errors 封装）：服务层应写入
		// result=failed 的操作记录并对外返回 500。
		w.WriteHeader(http.StatusInternalServerError)
		f.writeJSON(w, map[string]any{"errors": map[string]string{"_": msg}})
		return
	}
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

// registerTemplate 直接向 fake 节点注册一台 PVE 模板 VM（template=1）。
// 模板是供克隆使用的基础镜像而非运行实体，列表合并应排除它。
func (f *fakePVE) registerTemplate(vmid int64, name string, config map[string]string) {
	f.mu.Lock()
	f.vms[vmid] = &fakePVEVM{vmid: vmid, name: name, status: "stopped", config: config, template: true}
	f.mu.Unlock()
}

// setStatusError 让后续对该 vmid 的 status 操作（start/stop/reboot）
// 返回 PVE 错误（HTTP 500），模拟 PVE 拒绝操作；配合 clearStatusError
// 恢复成功路径。
func (f *fakePVE) setStatusError(vmid int64, msg string) {
	f.mu.Lock()
	f.statusErrors[vmid] = msg
	f.mu.Unlock()
}

// clearStatusError 移除 setStatusError 注入的错误，恢复成功路径。
func (f *fakePVE) clearStatusError(vmid int64) {
	f.mu.Lock()
	delete(f.statusErrors, vmid)
	f.mu.Unlock()
}

// ---------- 辅助函数 ----------

func e2eDSN() string {
	if dsn := os.Getenv("SPARK_E2E_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://spark:spark@127.0.0.1:5432/spark_test"
}

// e2eJWTSecret 是 e2e 测试注入路由的固定 JWT 密钥（任务 9.1）：与
// api/router_test.go 的 testJWTSecret 完全一致（≥32 字符满足 config 校验），
// 保证登录/鉴权链路在测试中签发与校验一致的令牌。
const e2eJWTSecret = "test-jwt-secret-0123456789abcdefghijklmnopqrstuv"

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
// （storage_types、images）。用户体系落地后（迁移 0010）新增
// users/admins：vms.user_id 引用 users(id)，把两者列入同一语句后，
// CASCADE 会在单条 TRUNCATE 中自动纳入全部外键依赖方（含 vms、
// vm_operations），无需手工编排清空顺序。schema_migrations 和
// schema_probe 不会被触碰。
func truncateBusinessTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "TRUNCATE zones, storage_types, images, users, admins CASCADE"); err != nil {
		t.Fatalf("truncate business tables: %v", err)
	}
}

// seedAdmin 直接向 admins 表插入一个测试管理员：密码经 service.HashPassword
// 生成 bcrypt 哈希（明文绝不落库，与生产路径一致），返回其 id。须在
// truncateBusinessTables 之后调用以保证可重跑。
func seedAdmin(t *testing.T, ctx context.Context, pool *pgxpool.Pool, username, password string) int64 {
	t.Helper()
	hash, err := service.HashPassword(password)
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO admins (username, password_hash) VALUES ($1, $2) RETURNING id",
		username, hash).Scan(&id); err != nil {
		t.Fatalf("insert admin %s: %v", username, err)
	}
	return id
}

// e2eAdminToken 播种测试管理员并通过 POST /auth/admin/login 换取 admin
// Bearer token（任务 9.1）：走完整 HTTP 登录链路，登录端点一并被覆盖。
func e2eAdminToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, client *http.Client, base, username, password string) string {
	t.Helper()
	seedAdmin(t, ctx, pool, username, password)
	body := e2eObj(t, e2eDo(t, client, "", base, http.MethodPost, "/auth/admin/login",
		map[string]any{"username": username, "password": password}, http.StatusOK))
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("admin login response has no token: %+v", body)
	}
	return token
}

// e2eDo 对测试服务器执行一次 HTTP 调用并断言
// 状态码；解码后的 JSON body 以 any 类型返回。token 非空时附带
// Authorization: Bearer 头（空串表示匿名请求，用于 401 断言）。
func e2eDo(t *testing.T, client *http.Client, token, base, method, path string, body any, want int) any {
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
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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

// opExpect 是一次虚拟机操作记录断言的期望形态：动作、结果、节点与
// PVE VMID（对应 GET /vms/{id}/operations 的响应字段）。
type opExpect struct {
	action string
	result string
	nodeID int64
	vmid   int64
}

// e2eOperations 请求 GET /vms/{ref}/operations（ref 为数字本地行 id 或
// ext- 合成标识），断言记录数等于 want 且按时间倒序逐条匹配动作/结果/
// 节点/VMID，并检查 created_at 非空。返回解析出的记录供调用方补充
// 断言（如失败操作的 error_message）。
func e2eOperations(t *testing.T, client *http.Client, token, base, ref string, want []opExpect) []any {
	t.Helper()
	body := e2eObj(t, e2eDo(t, client, token, base, http.MethodGet,
		fmt.Sprintf("/vms/%s/operations", ref), nil, http.StatusOK))
	ops, ok := body["operations"].([]any)
	if !ok {
		t.Fatalf("operations response of %s = %+v, want an operations array", ref, body)
	}
	if len(ops) != len(want) {
		t.Fatalf("operations of %s = %d records, want %d (all: %+v)", ref, len(ops), len(want), ops)
	}
	for i, w := range want {
		op := e2eObj(t, ops[i])
		if op["action"] != w.action || op["result"] != w.result {
			t.Fatalf("operation[%d] of %s = action=%v result=%v, want %s/%s",
				i, ref, op["action"], op["result"], w.action, w.result)
		}
		if op["node_id"] != float64(w.nodeID) || op["pve_vmid"] != float64(w.vmid) {
			t.Fatalf("operation[%d] of %s = node_id=%v pve_vmid=%v, want node %d vmid %d",
				i, ref, op["node_id"], op["pve_vmid"], w.nodeID, w.vmid)
		}
		if createdAt, ok := op["created_at"].(string); !ok || createdAt == "" {
			t.Fatalf("operation[%d] of %s = created_at %v, want a non-empty timestamp",
				i, ref, op["created_at"])
		}
	}
	return ops
}

// ---------- 端到端场景 ----------

// TestE2EVMFullLifecycle 通过完整的 HTTP 链路运行任务 9.2 的整个场景：
// 注册 zone/node/IP 池/存储类型/镜像，创建虚拟机（在 fake PVE 上异步配置），
// 执行 start/stop/restart，resize（缩小被 422 拒绝、扩容生效），
// 验证透传的列表与详情，销毁，并断言 IP 已被释放、数据库行已被删除。
// TestE2EVMNoPoolRejected 验证「区域无 IP 池」场景的错误语义：环境只有
// zone、节点、存储类型与镜像（镜像已存在于 fake PVE 的 import 目录，排除
// 镜像因素），刻意不创建 IP 池；创建 VM 被 400 no_available_ip_pool 拒绝
// （设计 3 优先级 a：池配置缺失优先于镜像检查暴露）。
func TestE2EVMNoPoolRejected(t *testing.T) {
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

	truncateBusinessTables(t, ctx, pool)
	defer truncateBusinessTables(t, ctx, pool)

	fakePVE := newFakePVE(t)
	pveServer := httptest.NewTLSServer(fakePVE)
	defer pveServer.Close()
	pvePort := pveServer.Listener.Addr().(*net.TCPAddr).Port

	router := api.NewRouter(pool, e2eCipher(t),
		api.WithJWTSecret(e2eJWTSecret),
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

	adminToken := e2eAdminToken(t, ctx, pool, client, base, "e2e-nopool-admin", "e2e-nopool-pass")

	// 注册部署环境：zone、节点、存储类型、镜像——刻意不创建 IP 池。
	zone := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/zones", map[string]any{"name": "e2e-nopool-zone"}, http.StatusCreated))
	zoneID := int64(zone["id"].(float64))

	e2eDo(t, client, adminToken, base, http.MethodPost,
		fmt.Sprintf("/zones/%d/nodes", zoneID),
		map[string]any{"name": "pve1", "host": fmt.Sprintf("127.0.0.1:%d", pvePort), "api_user": "root@pam", "api_token": "spark=uuid"},
		http.StatusCreated)

	st := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/storage-types", map[string]any{
		"name": "ssd", "display_name": "SSD", "pve_storage": "local-lvm",
	}, http.StatusCreated))
	stID := int64(st["id"].(float64))

	img := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/images", map[string]any{
		"name":         "debian-12-cloud",
		"default_user": "debian",
		"download_url": "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2",
	}, http.StatusCreated))
	imgID := int64(img["id"].(float64))

	// 镜像存在于 fake PVE 的 import 目录：节点选择扫描（storage content）
	// 能命中镜像，从而排除镜像因素，纯粹验证"区域无池"分支。
	fakePVE.addImportFile("local", "debian-12-genericcloud-amd64.qcow2")

	// 区域无池创建 VM -> 400 no_available_ip_pool（不落库、不占 IP）。
	rejected := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/vms", map[string]any{
		"name": "e2e-vm-nopool", "cpu": 2, "mem_mb": int64(2048), "disk_gb": int64(10),
		"image_id": imgID, "storage_type_id": stID, "zone_id": zoneID, "password": "s3cret-pw",
	}, http.StatusBadRequest))
	rejectedErr := e2eObj(t, rejected["error"])
	if code, _ := rejectedErr["code"].(string); code != "no_available_ip_pool" {
		t.Fatalf("create vm without ip pool: error code = %q, want no_available_ip_pool", code)
	}

	// 拒绝发生在落库之前：列表不得出现该测试创建的 VM 名称（不落库证明）。
	list := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet, "/vms", nil, http.StatusOK))
	for _, raw := range list["vms"].([]any) {
		item := raw.(map[string]any)
		if item["name"] == "e2e-vm-nopool" {
			t.Fatalf("GET /vms contains the rejected VM name %q: rejected create must not persist a row", item["name"])
		}
	}
}

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
		// 注入固定 JWT 密钥（任务 9.1）：与 api/router_test.go 的
		// testJWTSecret 相同的 e2eJWTSecret，否则 auth 服务构造会因空
		// 密钥 panic；后续登录/鉴权全部使用该密钥签发与校验。
		api.WithJWTSecret(e2eJWTSecret),
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

	// 认证已生效（任务 9.1）：不带 token 的请求一律 401 unauthorized——
	// 用户体系落地前 e2e 全部请求均无 Authorization 头，此处显式断言
	// requireAuth 中间件对业务路由的覆盖。
	anon := e2eObj(t, e2eDo(t, client, "", base, http.MethodGet, "/zones", nil, http.StatusUnauthorized))
	anonErr := e2eObj(t, anon["error"])
	if code, _ := anonErr["code"].(string); code != "unauthorized" {
		t.Fatalf("anonymous /zones error code = %q, want unauthorized", code)
	}

	// 种子管理员并走真实登录链路换取 admin Bearer token（任务 9.1）：
	// 直接操作 pool 向 admins 表 INSERT bcrypt 哈希（明文不落库），再经
	// POST /auth/admin/login 获取 JWT——登录接口本身也一并被覆盖。
	adminToken := e2eAdminToken(t, ctx, pool, client, base, "e2e-admin", "e2e-admin-pass")

	// 1. 注册部署环境：zone、node（host 携带 fake PVE 的监听端口）、
	// IP 池 + 节点白名单、存储类型、镜像（在节点上存在）。
	zone := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/zones", map[string]any{"name": "e2e-zone"}, http.StatusCreated))
	zoneID := int64(zone["id"].(float64))

	// 1a. 业务名与 fake 集群真实节点名（只有 pve1）不一致 -> 503 被拒，
	// 错误消息提示集群真实名；登记走的是生产默认探测实现（真实连接 fake
	// PVE 的 /nodes），因此同时验证了探测链路。
	mismatch := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost,
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

	node := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost,
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
	listed := e2eDo(t, client, adminToken, base, http.MethodGet, fmt.Sprintf("/zones/%d/nodes", zoneID), nil, http.StatusOK).([]any)
	if len(listed) != 1 {
		t.Fatalf("GET nodes = %+v, want 1 node", listed)
	}
	listedNode := e2eObj(t, listed[0])
	if listedNode["host"] != "127.0.0.1" || listedNode["port"] != float64(pvePort) {
		t.Fatalf("listed node = %+v, want host=127.0.0.1 port=%d", listedNode, pvePort)
	}

	poolRes := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/ip-pools", map[string]any{
		"zone_id": zoneID, "name": "e2e-pool", "network_cidr": "10.9.0.0/24",
		"gateway": "10.9.0.1", "dns": "1.1.1.1",
	}, http.StatusCreated))
	poolID := int64(poolRes["id"].(float64))

	e2eDo(t, client, adminToken, base, http.MethodPut, fmt.Sprintf("/ip-pools/%d/nodes", poolID),
		map[string]any{"node_ids": []int64{nodeID}}, http.StatusOK)

	st := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/storage-types", map[string]any{
		"name": "ssd", "display_name": "SSD", "pve_storage": "local-lvm",
	}, http.StatusCreated))
	stID := int64(st["id"].(float64))

	img := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/images", map[string]any{
		"name":         "debian-12-cloud",
		"default_user": "debian",
		// 镜像重构后登记改为 download_url：文件由节点代发下载，创建 VM 的
		// scsi0 import-from 使用下载出的文件名对应的 volid。
		"download_url": "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2",
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

	// 1b. 镜像尚未下载到节点上：创建 VM 被 400 image_not_available_in_zone
	// 拒绝（selectPoolAndNode 扫描节点 content 无匹配），此时不落库、不占 IP。
	notAvail := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/vms", map[string]any{
		"name": vmName, "cpu": vmCPU, "mem_mb": vmMemMB, "disk_gb": vmDisk,
		"image_id": imgID, "storage_type_id": stID, "zone_id": zoneID, "password": vmPW,
	}, http.StatusBadRequest))
	notAvailErr := e2eObj(t, notAvail["error"])
	if code, _ := notAvailErr["code"].(string); code != "image_not_available_in_zone" {
		t.Fatalf("create vm without image on node: error code = %q, want image_not_available_in_zone", code)
	}

	// 预置节点上的镜像文件（模拟 download 异步完成后 PVE 侧已存在，与 fake
	// download-url 端点写入同一份 importFiles 状态）：创建 VM 的节点选择
	// 扫描 local/import 才能命中镜像并生成 volid。
	fakePVE.addImportFile("local", "debian-12-genericcloud-amd64.qcow2")

	// 2. 创建虚拟机：201、已分配 IP、过渡状态 "creating"
	// （PVE 侧此时还不存在）。
	created := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/vms", map[string]any{
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
		detail = e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet, fmt.Sprintf("/vms/%d", vmID), nil, http.StatusOK))
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
	if !strings.HasPrefix(scsi0, "local-lvm:") || !strings.Contains(scsi0, "import-from=local:import/debian-12-genericcloud-amd64.qcow2") {
		t.Fatalf("scsi0 = %q, want local-lvm storage with import-from volid", scsi0)
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
	e2eDo(t, client, adminToken, base, http.MethodPost, fmt.Sprintf("/vms/%d/start", vmID), nil, http.StatusAccepted)
	if s := fakePVE.get(100); s == nil || s.status != "running" {
		t.Fatalf("fake pve status after start = %+v, want running", s)
	}
	e2eDo(t, client, adminToken, base, http.MethodPost, fmt.Sprintf("/vms/%d/stop", vmID), nil, http.StatusAccepted)
	if s := fakePVE.get(100); s == nil || s.status != "stopped" {
		t.Fatalf("fake pve status after stop = %+v, want stopped", s)
	}
	e2eDo(t, client, adminToken, base, http.MethodPost, fmt.Sprintf("/vms/%d/restart", vmID), nil, http.StatusAccepted)
	if s := fakePVE.get(100); s == nil || s.status != "running" {
		t.Fatalf("fake pve status after restart = %+v, want running", s)
	}

	// 5b. 操作记录（托管 VM）：start/stop/restart 受理后各写入一条
	// accepted 记录，GET /vms/{id}/operations 按时间倒序返回
	// 动作/结果/节点/时间。
	e2eOperations(t, client, adminToken, base, strconv.FormatInt(vmID, 10), []opExpect{
		{action: "reboot", result: "accepted", nodeID: nodeID, vmid: 100},
		{action: "stop", result: "accepted", nodeID: nodeID, vmid: 100},
		{action: "start", result: "accepted", nodeID: nodeID, vmid: 100},
	})

	// 6. Resize：缩小磁盘会被 422 拒绝；增大 cpu/mem/
	// disk 会成功，并在 PVE 侧生效（config + resize）。
	// 该操作是对虚拟机资源的 PATCH（JSON Merge Patch 语义：
	// 未出现的字段保持当前值）。
	e2eDo(t, client, adminToken, base, http.MethodPatch, fmt.Sprintf("/vms/%d", vmID),
		map[string]any{"disk_gb": 5}, http.StatusUnprocessableEntity)

	resized := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPatch, fmt.Sprintf("/vms/%d", vmID),
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
	list := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet, "/vms", nil, http.StatusOK))
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
	detail = e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet, fmt.Sprintf("/vms/%d", vmID), nil, http.StatusOK))
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
	pagedReq.Header.Set("Authorization", "Bearer "+adminToken)
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
	e2eDo(t, client, adminToken, base, http.MethodDelete, fmt.Sprintf("/vms/%d", vmID), nil, http.StatusNoContent)
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

	// 8b. destroy 受理后写入 accepted 记录；本地行虽已删除，操作记录是
	// 审计历史不随 VM 删除——经 ext-{nodeID}-100 合成标识仍可查询到
	// 全部 4 条（destroy/reboot/stop/start，倒序）。
	e2eOperations(t, client, adminToken, base, fmt.Sprintf("ext-%d-100", nodeID), []opExpect{
		{action: "destroy", result: "accepted", nodeID: nodeID, vmid: 100},
		{action: "reboot", result: "accepted", nodeID: nodeID, vmid: 100},
		{action: "stop", result: "accepted", nodeID: nodeID, vmid: 100},
		{action: "start", result: "accepted", nodeID: nodeID, vmid: 100},
	})

	// ---------- 全部 PVE 虚拟机可见 + 认领（feat/all-pve-vms-visible） ----------

	// 9. 预置外部 VM：在 fake PVE 上注册手工创建的 VM（绕过 POST /qemu
	// 链路，模拟 PVE 上已存在的虚拟机，vmid 避开已销毁的 100）：
	//   - vmid=200：后续做"不带 IP 的认领"（PVE 静态 IP 10.9.0.10 落在
	//     e2e-pool 网段内，但新语义下不传 ip 即不分配）；
	//   - vmid=201：PVE 模板（供克隆的基础镜像，不应出现在列表中）；
	//   - vmid=202：走 external 直接生命周期（start/stop/restart/destroy）
	//     与失败操作记录；
	//   - vmid=203：后续做"带 IP 认领"（10.9.0.11 从池占用）。
	fakePVE.registerVM(200, "imported-vm", map[string]string{
		"name":      "imported-vm",
		"cores":     "1",
		"memory":    "1024",
		"scsi0":     "local-lvm:vm-200-disk-0,size=20G",
		"ipconfig0": "ip=10.9.0.10/24,gw=10.9.0.1",
	})
	fakePVE.registerTemplate(201, "base-template", map[string]string{
		"name":   "base-template",
		"cores":  "1",
		"memory": "1024",
		"scsi0":  "local-lvm:vm-201-disk-0,size=5G",
	})
	fakePVE.registerVM(202, "external-vm", map[string]string{
		"name":   "external-vm",
		"cores":  "2",
		"memory": "2048",
		"scsi0":  "local-lvm:vm-202-disk-0,size=30G",
	})
	fakePVE.registerVM(203, "claimed-with-ip", map[string]string{
		"name":   "claimed-with-ip",
		"cores":  "2",
		"memory": "2048",
		"scsi0":  "local-lvm:vm-203-disk-0,size=30G",
	})
	extID := func(vmid int64) string { return fmt.Sprintf("ext-%d-%d", nodeID, vmid) }

	// 9a. unmanaged 接口已下线：不再有独立的 GET /vms/unmanaged 路由，
	// 请求落入 GET /vms/:id 通配并被 bad_request 拒绝（原"候选列表"
	// 语义彻底消失，认领入口改为基于列表中的 external 条目，spec：
	// 未托管虚拟机候选查询已移除）。
	unmanagedGone := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet,
		fmt.Sprintf("/vms/unmanaged?node_id=%d", nodeID), nil, http.StatusBadRequest))
	unmanagedGoneErr := e2eObj(t, unmanagedGone["error"])
	if code, _ := unmanagedGoneErr["code"].(string); code != "bad_request" {
		t.Fatalf("GET /vms/unmanaged error code = %q, want bad_request (接口已下线)", code)
	}

	// 9b. 列表并入 external 条目：vmid 200/202/203 以合成 id
	// ext-{nodeID}-{vmid} 出现，source=external、uuid/created_at/
	// updated_at 为空、规格取 PVE 摘要；PVE 模板（vmid=201）不出现。
	list = e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet, "/vms", nil, http.StatusOK))
	externalByVmid := map[int64]map[string]any{}
	for _, raw := range list["vms"].([]any) {
		item := e2eObj(t, raw)
		idStr, ok := item["id"].(string)
		if !ok {
			continue // 本地行：数字 id
		}
		parts := strings.Split(strings.TrimPrefix(idStr, "ext-"), "-")
		if len(parts) == 2 {
			if vmid, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				externalByVmid[vmid] = item
			}
		}
	}
	for _, vmid := range []int64{200, 202, 203} {
		item, ok := externalByVmid[vmid]
		if !ok {
			t.Fatalf("GET /vms has no external entry for vmid %d (vms: %+v)", vmid, list["vms"])
		}
		if item["id"] != extID(vmid) {
			t.Fatalf("external %d id = %v, want %s", vmid, item["id"], extID(vmid))
		}
		if item["source"] != "external" {
			t.Fatalf("external %d source = %v, want external", vmid, item["source"])
		}
		if item["uuid"] != "" || item["created_at"] != "" || item["updated_at"] != "" {
			t.Fatalf("external %d uuid/created_at/updated_at = %v/%v/%v, want all empty",
				vmid, item["uuid"], item["created_at"], item["updated_at"])
		}
		if item["pve_vmid"] != float64(vmid) || item["node_id"] != float64(nodeID) {
			t.Fatalf("external %d = pve_vmid=%v node_id=%v, want vmid %d on node %d",
				vmid, item["pve_vmid"], item["node_id"], vmid, nodeID)
		}
	}
	if tpl, ok := externalByVmid[201]; ok {
		t.Fatalf("template vm 201 leaked into list: %+v", tpl)
	}

	// 9c. 分页对 external 条目生效：X-Total-Count 是合并后总数
	//（3 = 200/202/203，模板排除）；按 (node_id, pve_vmid) 升序内存分页，
	// offset=1 应落在 vmid=202 的 external 条目上（顺序 [200, 202, 203]）。
	pagedReq2, err := http.NewRequest(http.MethodGet, base+"/vms?limit=1&offset=1", nil)
	if err != nil {
		t.Fatalf("build paginated external list request: %v", err)
	}
	pagedReq2.Header.Set("Authorization", "Bearer "+adminToken)
	pagedResp2, err := client.Do(pagedReq2)
	if err != nil {
		t.Fatalf("GET /vms?limit=1&offset=1: %v", err)
	}
	defer pagedResp2.Body.Close()
	if pagedResp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /vms?limit=1&offset=1: status %d, want 200", pagedResp2.StatusCode)
	}
	total2, err := strconv.Atoi(pagedResp2.Header.Get("X-Total-Count"))
	if err != nil || total2 != 3 {
		t.Fatalf("X-Total-Count with externals = %q, want 3", pagedResp2.Header.Get("X-Total-Count"))
	}
	var paged2 map[string]any
	if err := json.NewDecoder(pagedResp2.Body).Decode(&paged2); err != nil {
		t.Fatalf("decode paginated external list: %v", err)
	}
	pagedItems2, ok := paged2["vms"].([]any)
	if !ok || len(pagedItems2) != 1 {
		t.Fatalf("GET /vms?limit=1&offset=1 vms = %+v, want exactly 1 item", paged2["vms"])
	}
	if page2 := e2eObj(t, pagedItems2[0]); page2["id"] != extID(202) {
		t.Fatalf("paginated page offset=1 contains %v, want %s", page2["id"], extID(202))
	}

	// 9d. external 直接生命周期：ext- 标识的 start/stop/restart 直调 PVE
	//（无需本地记录）-> 202，fake 状态随之变化。
	e2eDo(t, client, adminToken, base, http.MethodPost, fmt.Sprintf("/vms/%s/start", extID(202)), nil, http.StatusAccepted)
	if s := fakePVE.get(202); s == nil || s.status != "running" {
		t.Fatalf("fake pve status of external vm after start = %+v, want running", s)
	}
	e2eDo(t, client, adminToken, base, http.MethodPost, fmt.Sprintf("/vms/%s/stop", extID(202)), nil, http.StatusAccepted)
	if s := fakePVE.get(202); s == nil || s.status != "stopped" {
		t.Fatalf("fake pve status of external vm after stop = %+v, want stopped", s)
	}
	e2eDo(t, client, adminToken, base, http.MethodPost, fmt.Sprintf("/vms/%s/restart", extID(202)), nil, http.StatusAccepted)
	if s := fakePVE.get(202); s == nil || s.status != "running" {
		t.Fatalf("fake pve status of external vm after restart = %+v, want running", s)
	}

	// 9d2. external 详情：GET /vms/ext-{nodeID}-202 -> 200，external 形态
	//（合成 id、uuid/created_at/updated_at 为空、source=external、规格取
	// PVE 摘要、实时指标透传）。fake 摘要只带 cpus/maxmem/maxdisk 且磁盘
	// 大小解析仅支持 size= 前缀形态（registerVM 的 scsi0 为
	// "local-lvm:vm-202-disk-0,size=30G"），因此 disk_gb=0 且
	// mem/maxdisk/cpu_usage/uptime 以零值省略——断言按 fake 实际输出。
	extDetail := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet,
		fmt.Sprintf("/vms/%s", extID(202)), nil, http.StatusOK))
	if extDetail["id"] != extID(202) || extDetail["source"] != "external" {
		t.Fatalf("external detail id/source = %v / %v, want %s / external", extDetail["id"], extDetail["source"], extID(202))
	}
	if extDetail["uuid"] != "" || extDetail["created_at"] != "" || extDetail["updated_at"] != "" {
		t.Fatalf("external detail uuid/created_at/updated_at = %v/%v/%v, want all empty",
			extDetail["uuid"], extDetail["created_at"], extDetail["updated_at"])
	}
	if extDetail["pve_vmid"] != float64(202) || extDetail["node_id"] != float64(nodeID) {
		t.Fatalf("external detail = pve_vmid=%v node_id=%v, want vmid 202 on node %d",
			extDetail["pve_vmid"], extDetail["node_id"], nodeID)
	}
	// 规格取 PVE 摘要：2 核 / 2048 MiB（磁盘大小 fake 不解析，为 0）。
	if extDetail["cpu"] != float64(2) || extDetail["mem_mb"] != float64(2048) || extDetail["disk_gb"] != float64(0) {
		t.Fatalf("external detail spec = %+v, want cpu=2 mem=2048 disk=0 from the PVE summary", extDetail)
	}
	if extDetail["status"] != "running" {
		t.Fatalf("external detail status = %v, want running (pass-through)", extDetail["status"])
	}
	if extDetail["maxmem"] != float64(2048<<20) {
		t.Fatalf("external detail maxmem = %v, want the byte value from the PVE summary", extDetail["maxmem"])
	}
	for _, key := range []string{"maxdisk", "mem", "cpu_usage", "uptime"} {
		if _, ok := extDetail[key]; ok {
			t.Fatalf("external detail %s = %v, want omitted (fake summary lacks it)", key, extDetail[key])
		}
	}

	// 9e. 失败操作：fake 注入 PVE 拒绝（HTTP 500）后对 external VM 发起
	// stop -> 500；随后清除注入，成功路径恢复。失败操作的审计记录断言
	// 在 9f 中进行。
	fakePVE.setStatusError(202, "simulated pve failure")
	e2eDo(t, client, adminToken, base, http.MethodPost, fmt.Sprintf("/vms/%s/stop", extID(202)), nil, http.StatusInternalServerError)
	fakePVE.clearStatusError(202)

	// 9f. 操作记录（external VM）：GET /vms/ext-{nodeID}-202/operations
	// 按时间倒序返回 4 条——[stop failed, restart accepted, stop accepted,
	// start accepted]，节点/VMID/时间齐全；失败记录的 error_message 已
	// 脱敏（保留失败原因、不含内部 host:port）。
	ops := e2eOperations(t, client, adminToken, base, extID(202), []opExpect{
		{action: "stop", result: "failed", nodeID: nodeID, vmid: 202},
		{action: "reboot", result: "accepted", nodeID: nodeID, vmid: 202},
		{action: "stop", result: "accepted", nodeID: nodeID, vmid: 202},
		{action: "start", result: "accepted", nodeID: nodeID, vmid: 202},
	})
	failedOp := e2eObj(t, ops[0])
	if msg, _ := failedOp["error_message"].(string); !strings.Contains(msg, "simulated pve failure") || strings.Contains(msg, "127.0.0.1") {
		t.Fatalf("failed operation error_message = %q, want sanitized message containing simulated pve failure", msg)
	}

	// 9g. external destroy：DELETE -> 204，fake 上的 VM 被删除；之后对
	// 已销毁的 ext- 标识再次操作 -> 404 vm_not_found_on_node（spec：对
	// 不存在的虚拟机执行操作返回资源不存在）。
	e2eDo(t, client, adminToken, base, http.MethodDelete, fmt.Sprintf("/vms/%s", extID(202)), nil, http.StatusNoContent)
	if got := fakePVE.get(202); got != nil {
		t.Fatalf("fake pve still has VM 202 after external destroy: %+v", got)
	}
	gone := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost,
		fmt.Sprintf("/vms/%s/start", extID(202)), nil, http.StatusNotFound))
	goneErr := e2eObj(t, gone["error"])
	if code, _ := goneErr["code"].(string); code != "vm_not_found_on_node" {
		t.Fatalf("operate destroyed external vm: error code = %q, want vm_not_found_on_node", code)
	}
	// 9g2. external 详情对已销毁 VM：GET /vms/ext-{nodeID}-202 -> 404
	// vm_not_found_on_node（节点可达但 VM 已从 PVE 移除）。
	goneDetail := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet,
		fmt.Sprintf("/vms/%s", extID(202)), nil, http.StatusNotFound))
	goneDetailErr := e2eObj(t, goneDetail["error"])
	if code, _ := goneDetailErr["code"].(string); code != "vm_not_found_on_node" {
		t.Fatalf("detail of destroyed external vm: error code = %q, want vm_not_found_on_node", code)
	}
	// 操作记录是审计历史，不随 VM 销毁而删除：destroy 受理后共 5 条，
	// 最新一条为 destroy/accepted（ext- 查询不校验 VM 当前是否存在）。
	e2eOperations(t, client, adminToken, base, extID(202), []opExpect{
		{action: "destroy", result: "accepted", nodeID: nodeID, vmid: 202},
		{action: "stop", result: "failed", nodeID: nodeID, vmid: 202},
		{action: "reboot", result: "accepted", nodeID: nodeID, vmid: 202},
		{action: "stop", result: "accepted", nodeID: nodeID, vmid: 202},
		{action: "start", result: "accepted", nodeID: nodeID, vmid: 202},
	})

	// 10. 认领（IP 可选——不传 ip）：POST /vms/import -> 201 + Location
	// + 完整 VMListItem；source=claimed、响应 ip 为空（本地 ip_id 保持
	// NULL，网络由 PVE 侧配置决定）。
	importReq, err := http.NewRequest(http.MethodPost, base+"/vms/import", strings.NewReader(
		fmt.Sprintf(`{"zone_id":%d,"node_id":%d,"pve_vmid":200}`, zoneID, nodeID)))
	if err != nil {
		t.Fatalf("build import request: %v", err)
	}
	importReq.Header.Set("Content-Type", "application/json")
	importReq.Header.Set("Authorization", "Bearer "+adminToken)
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
	if imported["source"] != "claimed" {
		t.Fatalf("import source = %v, want claimed", imported["source"])
	}
	if v, ok := imported["ip"]; ok && v != "" {
		t.Fatalf("import without ip: response ip = %v, want empty/absent", v)
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
	// 数据库断言：未携带 ip 的认领行 ip_id 保持 NULL。
	var importedIPID *int64
	if err := pool.QueryRow(ctx, "SELECT ip_id FROM vms WHERE id=$1", importedID).Scan(&importedIPID); err != nil {
		t.Fatalf("query ip_id of imported vm: %v", err)
	}
	if importedIPID != nil {
		t.Fatalf("imported vm %d ip_id = %v, want NULL (未分配 IP)", importedID, *importedIPID)
	}

	// 10b. 幂等：同一节点上的同一 pve_vmid 重复认领 -> 409
	// vm_already_managed。
	idem := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/vms/import",
		map[string]any{"zone_id": zoneID, "node_id": nodeID, "pve_vmid": 200},
		http.StatusConflict))
	idemErr := e2eObj(t, idem["error"])
	if code, _ := idemErr["code"].(string); code != "vm_already_managed" {
		t.Fatalf("idempotent import code = %q, want vm_already_managed", code)
	}

	// 10c. 认领（携带 ip）：10.9.0.11 落在 e2e-pool（10.9.0.0/24）网段
	// 内，从池按地址占用并记录到本地元数据；source=claimed。
	withIP := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/vms/import",
		map[string]any{"zone_id": zoneID, "node_id": nodeID, "pve_vmid": 203, "ip": "10.9.0.11"},
		http.StatusCreated))
	withIPID := int64(withIP["id"].(float64))
	if withIP["ip"] != "10.9.0.11" {
		t.Fatalf("import with ip: response ip = %v, want 10.9.0.11", withIP["ip"])
	}
	if withIP["source"] != "claimed" {
		t.Fatalf("import with ip: source = %v, want claimed", withIP["source"])
	}
	if err := pool.QueryRow(ctx, "SELECT status, vm_id FROM ips WHERE ip=$1", "10.9.0.11").Scan(&ipStatus, &ipVMID); err != nil {
		t.Fatalf("query claimed ip 10.9.0.11: %v", err)
	}
	if ipStatus != "used" || ipVMID == nil || *ipVMID != withIPID {
		t.Fatalf("claimed ip 10.9.0.11: status=%q vm_id=%v, want used by vm %d", ipStatus, ipVMID, withIPID)
	}

	// 11. 列表与详情：GET /vms 中 200/203 以 source=claimed 出现（不再
	// 以 external 呈现）；GET /vms/:id 的 image_id/storage_type_id 可空。
	list = e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet, "/vms", nil, http.StatusOK))
	claimedSources := map[int64]string{}
	for _, raw := range list["vms"].([]any) {
		item := e2eObj(t, raw)
		switch id := item["id"].(type) {
		case float64:
			src, _ := item["source"].(string)
			claimedSources[int64(id)] = src
		}
	}
	for _, id := range []int64{importedID, withIPID} {
		if src, ok := claimedSources[id]; !ok || src != "claimed" {
			t.Fatalf("claimed vm %d missing from list or source = %q, want claimed (sources: %+v)", id, src, claimedSources)
		}
	}
	importedDetail := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet, fmt.Sprintf("/vms/%d", importedID), nil, http.StatusOK))
	if importedDetail["status"] != "stopped" {
		t.Fatalf("detail status of imported vm = %v, want stopped", importedDetail["status"])
	}
	if importedDetail["source"] != "claimed" {
		t.Fatalf("detail source of imported vm = %v, want claimed", importedDetail["source"])
	}
	for _, key := range []string{"image_id", "storage_type_id"} {
		if v, ok := importedDetail[key]; ok && v != nil {
			t.Fatalf("detail %s = %v, want null/absent", key, v)
		}
	}

	// 11b. ext- 标识指向已托管 VM 时按本地形态返回（G1）：认领后的
	// vmid 200 经 GET /vms/ext-{nodeID}-200 得到数字行 id（含 uuid、
	// source=claimed），而非 external 形态（合成 id、空 uuid）——与列表
	// 差集、生命周期 resolveVMTarget 的路由语义一致。
	claimedViaExt := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet,
		fmt.Sprintf("/vms/%s", extID(200)), nil, http.StatusOK))
	if claimedViaExt["id"] != float64(importedID) {
		t.Fatalf("claimed vm via ext- id = %v, want local numeric id %d", claimedViaExt["id"], importedID)
	}
	uuid, _ := claimedViaExt["uuid"].(string)
	if uuid == "" {
		t.Fatalf("claimed vm via ext- uuid = %q, want the local row uuid", uuid)
	}
	if claimedViaExt["source"] != "claimed" || claimedViaExt["status"] != "stopped" {
		t.Fatalf("claimed vm via ext- source/status = %v / %v, want claimed / stopped",
			claimedViaExt["source"], claimedViaExt["status"])
	}

	// 12. 认领即托管：start/resize 直接生效——start -> PVE running；
	// PATCH cpu=2 -> PVE config cores=2。start 受理后经数字 id 查询到
	// 1 条 accepted 记录。
	e2eDo(t, client, adminToken, base, http.MethodPost, fmt.Sprintf("/vms/%d/start", importedID), nil, http.StatusAccepted)
	if s := fakePVE.get(200); s == nil || s.status != "running" {
		t.Fatalf("fake pve status of imported vm after start = %+v, want running", s)
	}
	e2eDo(t, client, adminToken, base, http.MethodPatch, fmt.Sprintf("/vms/%d", importedID),
		map[string]any{"cpu": 2}, http.StatusOK)
	if cfg := fakePVE.get(200).config; cfg["cores"] != "2" {
		t.Fatalf("pve config after import resize = %+v, want cores=2", cfg)
	}
	e2eOperations(t, client, adminToken, base, strconv.FormatInt(importedID, 10), []opExpect{
		{action: "start", result: "accepted", nodeID: nodeID, vmid: 200},
	})

	// 13. 销毁带 IP 的认领 VM：DELETE -> 204；PVE 虚拟机被删除，占用的
	// IP 10.9.0.11 释放回 free（vm_id 置空），vms 数据行消失。
	e2eDo(t, client, adminToken, base, http.MethodDelete, fmt.Sprintf("/vms/%d", withIPID), nil, http.StatusNoContent)
	if got := fakePVE.get(203); got != nil {
		t.Fatalf("fake pve still has VM 203 after destroy: %+v", got)
	}
	if err := pool.QueryRow(ctx, "SELECT status, vm_id FROM ips WHERE ip=$1", "10.9.0.11").Scan(&ipStatus, &ipVMID); err != nil {
		t.Fatalf("query released ip 10.9.0.11: %v", err)
	}
	if ipStatus != "free" || ipVMID != nil {
		t.Fatalf("released ip 10.9.0.11: status=%q vm_id=%v, want free/nil", ipStatus, ipVMID)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM vms WHERE id=$1", withIPID).Scan(&vmCount); err != nil {
		t.Fatalf("query vms after claimed destroy: %v", err)
	}
	if vmCount != 0 {
		t.Fatalf("vms row count = %d after claimed destroy, want 0", vmCount)
	}

	// 14. 销毁无 IP 的认领 VM：DELETE -> 204；PVE 虚拟机被删除、本地行
	// 消失（无 IP 可释放）。destroy 受理后经 ext-{nodeID}-200 仍可查询到
	// 操作记录：[destroy accepted, start accepted]（审计不随 VM 删除）。
	e2eDo(t, client, adminToken, base, http.MethodDelete, fmt.Sprintf("/vms/%d", importedID), nil, http.StatusNoContent)
	if got := fakePVE.get(200); got != nil {
		t.Fatalf("fake pve still has VM 200 after destroy: %+v", got)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM vms WHERE id=$1", importedID).Scan(&vmCount); err != nil {
		t.Fatalf("query vms after import destroy: %v", err)
	}
	if vmCount != 0 {
		t.Fatalf("vms row count = %d after import destroy, want 0", vmCount)
	}
	e2eOperations(t, client, adminToken, base, extID(200), []opExpect{
		{action: "destroy", result: "accepted", nodeID: nodeID, vmid: 200},
		{action: "start", result: "accepted", nodeID: nodeID, vmid: 200},
	})

	// 15. 查询本地不存在的 VM 的操作记录 -> 404 not_found（spec：查询
	// 不存在虚拟机的记录返回资源不存在）。
	noOpsVM := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet,
		"/vms/999999/operations", nil, http.StatusNotFound))
	noOpsErr := e2eObj(t, noOpsVM["error"])
	if code, _ := noOpsErr["code"].(string); code != "not_found" {
		t.Fatalf("operations of missing vm: error code = %q, want not_found", code)
	}

	// 16. external 详情 503：第二个 fake PVE 服务器（复用主 fake 的 TLS
	// 证书，探测与调用使用同一客户端工厂均能信任）注册节点后关闭其
	// 监听——节点行仍在但 PVE 不可达，GET /vms/ext-{nodeID}-100 ->
	// 503 node_unavailable，绝不伪造状态。
	zone2 := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/zones",
		map[string]any{"name": "e2e-zone2"}, http.StatusCreated))
	zone2ID := int64(zone2["id"].(float64))
	pve2Srv := httptest.NewUnstartedServer(newFakePVE(t))
	pve2Srv.TLS = pveServer.TLS // 共享同一证书：探测与后续调用都走 pveServer.Client()
	pve2Srv.StartTLS()
	node2 := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost,
		fmt.Sprintf("/zones/%d/nodes", zone2ID),
		map[string]any{"name": "pve1", "host": fmt.Sprintf("127.0.0.1:%d", pve2Srv.Listener.Addr().(*net.TCPAddr).Port), "api_user": "root@pam", "api_token": "spark=uuid"},
		http.StatusCreated))
	node2ID := int64(node2["id"].(float64))
	pve2Srv.Close()
	unavail := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet,
		fmt.Sprintf("/vms/ext-%d-100", node2ID), nil, http.StatusServiceUnavailable))
	unavailErr := e2eObj(t, unavail["error"])
	if code, _ := unavailErr["code"].(string); code != "node_unavailable" {
		t.Fatalf("detail of external vm on unreachable node: error code = %q, want node_unavailable", code)
	}

	// ---------- 用户体系端到端（feat/user-management-config） ----------

	// 17. 用户视角（任务 9.2）：admin 创建用户 u1/u2 -> 各自登录 -> u1 创建
	// 归属自己的 VM（不传 user_id 默认归属自身）-> 列表/详情仅见自己 ->
	// 操作他人 VM 403 -> external VM 对用户不可见且操作 403 -> 用户令牌
	// 访问管理员接口 403 -> 用户销毁自己的 VM 放行。环境复用本节已有
	// zone/node/IP 池/存储/镜像（镜像文件仍登记在 fake 节点上），置于
	// 全部既有断言之后以免影响其分页总数等口径。
	u1 := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/users", map[string]any{
		"username": "e2e-user-1", "password": "u1-pass-1", "name": "User One",
	}, http.StatusCreated))
	u1ID := int64(u1["id"].(float64))
	u2 := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/users", map[string]any{
		"username": "e2e-user-2", "password": "u2-pass-2", "name": "User Two",
	}, http.StatusCreated))
	u2ID := int64(u2["id"].(float64))

	// u1 登录（POST /auth/login）：user JWT + user_id 与创建响应一致。
	u1Login := e2eObj(t, e2eDo(t, client, "", base, http.MethodPost, "/auth/login",
		map[string]any{"username": "e2e-user-1", "password": "u1-pass-1"}, http.StatusOK))
	u1Token, _ := u1Login["token"].(string)
	if u1Token == "" {
		t.Fatalf("u1 login response has no token: %+v", u1Login)
	}
	if u1Login["user_id"] != float64(u1ID) {
		t.Fatalf("u1 login user_id = %v, want %d", u1Login["user_id"], u1ID)
	}
	// u2 登录仅作第二个用户凭证可用的冒烟断言（403 断言使用 u1 令牌）。
	u2Login := e2eObj(t, e2eDo(t, client, "", base, http.MethodPost, "/auth/login",
		map[string]any{"username": "e2e-user-2", "password": "u2-pass-2"}, http.StatusOK))
	if u2Login["user_id"] != float64(u2ID) {
		t.Fatalf("u2 login user_id = %v, want %d", u2Login["user_id"], u2ID)
	}

	// 等待供给完成的本地轮询（与步骤 3 同模式）：token 区分请求身份。
	waitProvisioned := func(token string, id int64) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			d := e2eObj(t, e2eDo(t, client, token, base, http.MethodGet,
				fmt.Sprintf("/vms/%d", id), nil, http.StatusOK))
			switch d["status"] {
			case "failed":
				t.Fatalf("provisioning failed: %v", d["provision_error"])
			case "creating":
				if time.Now().After(deadline) {
					t.Fatalf("provisioning did not finish within 15s (last status %q)", d["status"])
				}
				time.Sleep(200 * time.Millisecond)
			default:
				return
			}
		}
	}

	// u1 创建 VM：不传 user_id，默认归属自身（fake PVE 供给链真实跑，
	// vmid 取下一号 101——100 已被既有创建消耗，registerVM 不推进 nextID）。
	u1VM := e2eObj(t, e2eDo(t, client, u1Token, base, http.MethodPost, "/vms", map[string]any{
		"name": "e2e-user-vm-1", "cpu": 1, "mem_mb": 1024, "disk_gb": 10,
		"image_id": imgID, "storage_type_id": stID, "zone_id": zoneID, "password": "u1-vm-pw",
	}, http.StatusCreated))
	u1VMID := int64(u1VM["id"].(float64))
	waitProvisioned(u1Token, u1VMID)
	u1Detail := e2eObj(t, e2eDo(t, client, u1Token, base, http.MethodGet,
		fmt.Sprintf("/vms/%d", u1VMID), nil, http.StatusOK))
	if u1Detail["status"] != "stopped" || u1Detail["pve_vmid"] != float64(101) {
		t.Fatalf("u1 vm detail = %+v, want stopped with pve_vmid 101", u1Detail)
	}
	// 归属落库断言：vms.user_id 指向 u1（响应负载不含 user_id 字段，
	// 直接查库验证，与既有 ips/vms 直查风格一致）。
	var u1UserID *int64
	if err := pool.QueryRow(ctx, "SELECT user_id FROM vms WHERE id=$1", u1VMID).Scan(&u1UserID); err != nil {
		t.Fatalf("query user_id of u1 vm: %v", err)
	}
	if u1UserID == nil || *u1UserID != u1ID {
		t.Fatalf("u1 vm %d user_id = %v, want u1 (%d)", u1VMID, u1UserID, u1ID)
	}

	// admin 为 u2 创建 VM（admin 可指定任意归属用户）。
	u2VM := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/vms", map[string]any{
		"name": "e2e-user-vm-2", "cpu": 1, "mem_mb": 1024, "disk_gb": 10,
		"image_id": imgID, "storage_type_id": stID, "zone_id": zoneID,
		"password": "u2-vm-pw", "user_id": u2ID,
	}, http.StatusCreated))
	u2VMID := int64(u2VM["id"].(float64))
	waitProvisioned(adminToken, u2VMID)

	// u1 列表分流：仅含归属自己的 VM（u1VM 在列、u2VM 不在列），
	// external 条目对用户一律剔除。
	assertUserListOnlyOwn := func() {
		t.Helper()
		userList := e2eObj(t, e2eDo(t, client, u1Token, base, http.MethodGet, "/vms", nil, http.StatusOK))
		var numeric []int64
		for _, raw := range userList["vms"].([]any) {
			item := e2eObj(t, raw)
			switch id := item["id"].(type) {
			case float64:
				numeric = append(numeric, int64(id))
			case string:
				t.Fatalf("user list must not contain external entry %s (vms: %+v)", id, userList["vms"])
			}
		}
		if len(numeric) != 1 || numeric[0] != u1VMID {
			t.Fatalf("user list vms = %v, want only own vm %d", numeric, u1VMID)
		}
	}
	assertUserListOnlyOwn()

	// u1 操作他人 VM -> 403 forbidden（归属校验在触碰 PVE 之前拦截）；
	// 详情同理 403。
	forb := e2eObj(t, e2eDo(t, client, u1Token, base, http.MethodPost,
		fmt.Sprintf("/vms/%d/start", u2VMID), nil, http.StatusForbidden))
	forbErr := e2eObj(t, forb["error"])
	if code, _ := forbErr["code"].(string); code != "forbidden" {
		t.Fatalf("u1 start u2's vm: error code = %q, want forbidden", code)
	}
	e2eDo(t, client, u1Token, base, http.MethodGet,
		fmt.Sprintf("/vms/%d", u2VMID), nil, http.StatusForbidden)
	// 归属校验拦截在 PVE 调用之前：u2 的 VM 不应被 u1 触碰（fake 状态不变）。
	if s := fakePVE.get(102); s == nil || s.status != "stopped" {
		t.Fatalf("fake pve state of u2 vm after forbidden start = %+v, want untouched stopped", s)
	}

	// external 对用户不可见：fake 登记一台未导入 VM（vmid=204 避开已用
	// 编号），用户列表不出现、详情与操作一律 403。
	fakePVE.registerVM(204, "user-invisible-external", map[string]string{
		"name":   "user-invisible-external",
		"cores":  "2",
		"memory": "2048",
		"scsi0":  "local-lvm:vm-204-disk-0,size=30G",
	})
	assertUserListOnlyOwn()
	e2eDo(t, client, u1Token, base, http.MethodGet,
		fmt.Sprintf("/vms/%s", extID(204)), nil, http.StatusForbidden)
	e2eDo(t, client, u1Token, base, http.MethodPost,
		fmt.Sprintf("/vms/%s/start", extID(204)), nil, http.StatusForbidden)

	// u1 访问管理员接口（/users）-> 403 forbidden。
	e2eDo(t, client, u1Token, base, http.MethodGet, "/users", nil, http.StatusForbidden)

	// u1 销毁自己的 VM -> 204（归属校验放行），PVE 实体被删除。
	e2eDo(t, client, u1Token, base, http.MethodDelete,
		fmt.Sprintf("/vms/%d", u1VMID), nil, http.StatusNoContent)
	if got := fakePVE.get(101); got != nil {
		t.Fatalf("fake pve still has VM 101 after user destroy: %+v", got)
	}

	// 18. 禁用用户（任务 9.3 部分）：admin 创建 u3 -> 登录拿 token ->
	// 禁用 -> 重新登录 401、已签发 token 请求 401（requireAuth 每次请求
	// 查库校验启用状态）。
	u3 := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/users", map[string]any{
		"username": "e2e-user-3", "password": "u3-pass-3", "name": "User Three",
	}, http.StatusCreated))
	u3ID := int64(u3["id"].(float64))
	u3Login := e2eObj(t, e2eDo(t, client, "", base, http.MethodPost, "/auth/login",
		map[string]any{"username": "e2e-user-3", "password": "u3-pass-3"}, http.StatusOK))
	u3Token, _ := u3Login["token"].(string)
	if u3Token == "" {
		t.Fatalf("u3 login response has no token: %+v", u3Login)
	}
	disabled := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPut,
		fmt.Sprintf("/users/%d/status", u3ID), map[string]any{"status": "disabled"}, http.StatusOK))
	if disabled["status"] != "disabled" {
		t.Fatalf("disable u3: status = %v, want disabled", disabled["status"])
	}
	e2eDo(t, client, "", base, http.MethodPost, "/auth/login",
		map[string]any{"username": "e2e-user-3", "password": "u3-pass-3"}, http.StatusUnauthorized)
	e2eDo(t, client, u3Token, base, http.MethodGet, "/vms", nil, http.StatusUnauthorized)
	// 恢复启用，供后续删除流程收尾（非语义必需）。
	e2eDo(t, client, adminToken, base, http.MethodPut,
		fmt.Sprintf("/users/%d/status", u3ID), map[string]any{"status": "enabled"}, http.StatusOK)

	// 19. 用户 CRUD 全路径（任务 9.3）：创建 201 + Location、
	// 列表 X-Total-Count、详情 200、修改 200、重复创建 409。
	u4Req, err := http.NewRequest(http.MethodPost, base+"/users", strings.NewReader(
		`{"username":"e2e-user-4","password":"u4-pass-4","name":"User Four"}`))
	if err != nil {
		t.Fatalf("build create user request: %v", err)
	}
	u4Req.Header.Set("Content-Type", "application/json")
	u4Req.Header.Set("Authorization", "Bearer "+adminToken)
	u4Resp, err := client.Do(u4Req)
	if err != nil {
		t.Fatalf("POST /users (u4): %v", err)
	}
	if u4Resp.StatusCode != http.StatusCreated {
		raw := make([]byte, 4096)
		n, _ := u4Resp.Body.Read(raw)
		t.Fatalf("POST /users (u4): status %d, want 201 (body: %s)", u4Resp.StatusCode, strings.TrimSpace(string(raw[:n])))
	}
	var u4Body any
	if err := json.NewDecoder(u4Resp.Body).Decode(&u4Body); err != nil {
		t.Fatalf("POST /users (u4): decode body: %v", err)
	}
	u4Resp.Body.Close()
	u4Obj := e2eObj(t, u4Body)
	u4ID := int64(u4Obj["id"].(float64))
	if loc := u4Resp.Header.Get("Location"); loc != fmt.Sprintf("/users/%d", u4ID) {
		t.Fatalf("create user Location = %q, want /users/%d", loc, u4ID)
	}
	// 列表：X-Total-Count 为当前全部用户数（u1-u4 共 4 个，无外部数据）。
	uListReq, err := http.NewRequest(http.MethodGet, base+"/users", nil)
	if err != nil {
		t.Fatalf("build list users request: %v", err)
	}
	uListReq.Header.Set("Authorization", "Bearer "+adminToken)
	uListResp, err := client.Do(uListReq)
	if err != nil {
		t.Fatalf("GET /users: %v", err)
	}
	if uListResp.StatusCode != http.StatusOK {
		raw := make([]byte, 4096)
		n, _ := uListResp.Body.Read(raw)
		t.Fatalf("GET /users: status %d, want 200 (body: %s)", uListResp.StatusCode, strings.TrimSpace(string(raw[:n])))
	}
	listTotal, err := strconv.Atoi(uListResp.Header.Get("X-Total-Count"))
	if err != nil || listTotal < 4 {
		t.Fatalf("GET /users X-Total-Count = %q, want an integer >= 4", uListResp.Header.Get("X-Total-Count"))
	}
	var uListBody []any
	if err := json.NewDecoder(uListResp.Body).Decode(&uListBody); err != nil {
		t.Fatalf("GET /users: decode body: %v", err)
	}
	uListResp.Body.Close()
	foundU4 := false
	for _, raw := range uListBody {
		if u := e2eObj(t, raw); int64(u["id"].(float64)) == u4ID {
			foundU4 = true
		}
	}
	if !foundU4 {
		t.Fatalf("GET /users does not contain u4 (%d): %+v", u4ID, uListBody)
	}
	u4Detail := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet,
		fmt.Sprintf("/users/%d", u4ID), nil, http.StatusOK))
	if u4Detail["username"] != "e2e-user-4" || u4Detail["name"] != "User Four" || u4Detail["status"] != "enabled" {
		t.Fatalf("u4 detail = %+v, want e2e-user-4 / User Four / enabled", u4Detail)
	}
	updated := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPut,
		fmt.Sprintf("/users/%d", u4ID), map[string]any{"name": "User Four Renamed"}, http.StatusOK))
	if updated["name"] != "User Four Renamed" {
		t.Fatalf("updated u4 name = %v, want User Four Renamed", updated["name"])
	}
	dupe := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/users",
		map[string]any{"username": "e2e-user-4", "password": "u4-pass-4"}, http.StatusConflict))
	dupeErr := e2eObj(t, dupe["error"])
	if code, _ := dupeErr["code"].(string); code != "conflict" {
		t.Fatalf("duplicate create user code = %q, want conflict", code)
	}

	// 20. 有资源禁删（任务 9.3）：u2 名下仍有 VM（u2VM）-> DELETE 409
	// user_has_resources；销毁 VM 后删除 -> 204；其余无资源用户（u1 已
	// 自毁 VM、u3/u4 无资源）删除 -> 204；全部删除后列表为空。
	blocked := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodDelete,
		fmt.Sprintf("/users/%d", u2ID), nil, http.StatusConflict))
	blockedErr := e2eObj(t, blocked["error"])
	if code, _ := blockedErr["code"].(string); code != "user_has_resources" {
		t.Fatalf("delete u2 with vm: error code = %q, want user_has_resources", code)
	}
	e2eDo(t, client, adminToken, base, http.MethodDelete,
		fmt.Sprintf("/vms/%d", u2VMID), nil, http.StatusNoContent)
	for _, id := range []int64{u2ID, u1ID, u3ID, u4ID} {
		e2eDo(t, client, adminToken, base, http.MethodDelete,
			fmt.Sprintf("/users/%d", id), nil, http.StatusNoContent)
	}
	emptyReq, err := http.NewRequest(http.MethodGet, base+"/users", nil)
	if err != nil {
		t.Fatalf("build empty users list request: %v", err)
	}
	emptyReq.Header.Set("Authorization", "Bearer "+adminToken)
	emptyResp, err := client.Do(emptyReq)
	if err != nil {
		t.Fatalf("GET /users (empty): %v", err)
	}
	defer emptyResp.Body.Close()
	if emptyResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /users (empty): status %d, want 200", emptyResp.StatusCode)
	}
	if totalRaw := emptyResp.Header.Get("X-Total-Count"); totalRaw != "0" {
		t.Fatalf("GET /users X-Total-Count after deletes = %q, want 0", totalRaw)
	}
}

// TestE2EImageDownloadLifecycle 覆盖镜像重构后的登记-下载-调度链路
// （镜像任务 8.1）：注册镜像（download_url，不再有 node_images）后节点上
// 尚未存在该文件（nodes-status downloaded=false、区域可用镜像列表为空），
// POST download（zone 模式）受理后轮询 operations 直至 success，节点状态
// 翻转为 downloaded=true（volid 可见），随后创建 VM 被调度到持有镜像的
// 节点（scsi0 import-from 使用 volid 而非路径）；下载失败路径（fake 注入
// download-url 500）落 failed 记录且错误消息经脱敏（不含内部 host:port）。
func TestE2EImageDownloadLifecycle(t *testing.T) {
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

	truncateBusinessTables(t, ctx, pool)
	defer truncateBusinessTables(t, ctx, pool)

	fakePVE := newFakePVE(t)
	pveServer := httptest.NewTLSServer(fakePVE)
	defer pveServer.Close()
	pvePort := pveServer.Listener.Addr().(*net.TCPAddr).Port

	// 镜像服务同样需要指向 fake PVE 的客户端工厂（WithImageClientFactory），
	// 否则存在性扫描与下载编排会连接节点登记的默认端口（8006）。
	newFakeClient := func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient(host, apiUser, apiTokenSecret,
			pve.WithPort(port),
			pve.WithHTTPClient(pveServer.Client()),
			pve.WithTimeout(5*time.Second))
	}
	router := api.NewRouter(pool, e2eCipher(t),
		// 注入固定 JWT 密钥（任务 9.1）：与 api/router_test.go 的
		// testJWTSecret 相同的 e2eJWTSecret，否则 auth 服务构造会因空
		// 密钥 panic；登录/鉴权链路使用该密钥签发与校验测试令牌。
		api.WithJWTSecret(e2eJWTSecret),
		api.WithVMClientFactory(newFakeClient),
		api.WithImageClientFactory(newFakeClient))
	app := httptest.NewServer(router)
	defer app.Close()

	client := app.Client()
	base := app.URL

	// 认证已生效（任务 9.1）：不带 token 的请求一律 401 unauthorized。
	anon := e2eObj(t, e2eDo(t, client, "", base, http.MethodGet, "/zones", nil, http.StatusUnauthorized))
	anonErr := e2eObj(t, anon["error"])
	if code, _ := anonErr["code"].(string); code != "unauthorized" {
		t.Fatalf("anonymous /zones error code = %q, want unauthorized", code)
	}

	// 种子管理员并走真实登录链路换取 admin Bearer token（任务 9.1）。
	adminToken := e2eAdminToken(t, ctx, pool, client, base, "e2e-img-admin", "e2e-img-admin-pass")

	// 注册部署环境：zone、node、IP 池 + 节点白名单、存储类型。
	zone := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/zones", map[string]any{"name": "e2e-img-zone"}, http.StatusCreated))
	zoneID := int64(zone["id"].(float64))
	node := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost,
		fmt.Sprintf("/zones/%d/nodes", zoneID),
		map[string]any{"name": "pve1", "host": fmt.Sprintf("127.0.0.1:%d", pvePort), "api_user": "root@pam", "api_token": "spark=uuid"},
		http.StatusCreated))
	nodeID := int64(node["id"].(float64))
	poolRes := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/ip-pools", map[string]any{
		"zone_id": zoneID, "name": "e2e-img-pool", "network_cidr": "10.8.0.0/24",
		"gateway": "10.8.0.1", "dns": "1.1.1.1",
	}, http.StatusCreated))
	poolID := int64(poolRes["id"].(float64))
	e2eDo(t, client, adminToken, base, http.MethodPut, fmt.Sprintf("/ip-pools/%d/nodes", poolID),
		map[string]any{"node_ids": []int64{nodeID}}, http.StatusOK)
	st := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/storage-types", map[string]any{
		"name": "ssd", "display_name": "SSD", "pve_storage": "local-lvm",
	}, http.StatusCreated))
	stID := int64(st["id"].(float64))

	// 1. 登记镜像（download_url）：登记本身不产生任何节点上的文件。
	const imgURL = "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
	img := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/images", map[string]any{
		"name": "e2e-image-download", "default_user": "debian", "download_url": imgURL,
	}, http.StatusCreated))
	imgID := int64(img["id"].(float64))

	// 2. 登记后节点状态：pve1 上 downloaded=false（PVE 实时扫描 import 目录）。
	statuses := e2eDo(t, client, adminToken, base, http.MethodGet,
		fmt.Sprintf("/images/%d/nodes-status?zone_id=%d", imgID, zoneID), nil, http.StatusOK).([]any)
	if len(statuses) != 1 {
		t.Fatalf("nodes-status = %+v, want 1 status", statuses)
	}
	st0 := e2eObj(t, statuses[0])
	if st0["node_id"] != float64(nodeID) || st0["node_name"] != "pve1" || st0["downloaded"] != false {
		t.Fatalf("nodes-status[0] = %+v, want pve1 not downloaded", st0)
	}

	// 3. 区域可用镜像列表为空（没有任何节点持有该镜像）。
	if zoneImgs := e2eDo(t, client, adminToken, base, http.MethodGet,
		fmt.Sprintf("/images?zone_id=%d", zoneID), nil, http.StatusOK).([]any); len(zoneImgs) != 0 {
		t.Fatalf("GET /images?zone_id=%d = %+v, want empty (image not downloaded yet)", zoneID, zoneImgs)
	}

	// 4. 受理下载（zone 模式）：202 + Location 指向 operations + 一条
	// running 记录（每节点一条）。
	dlReq, err := http.NewRequest(http.MethodPost, base+fmt.Sprintf("/images/%d/download", imgID),
		strings.NewReader(fmt.Sprintf(`{"zone_id":%d}`, zoneID)))
	if err != nil {
		t.Fatalf("build image download request: %v", err)
	}
	dlReq.Header.Set("Content-Type", "application/json")
	dlReq.Header.Set("Authorization", "Bearer "+adminToken)
	dlResp, err := client.Do(dlReq)
	if err != nil {
		t.Fatalf("POST /images/%d/download: %v", imgID, err)
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusAccepted {
		raw := make([]byte, 4096)
		n, _ := dlResp.Body.Read(raw)
		t.Fatalf("POST /images/%d/download: status %d, want 202 (body: %s)", imgID, dlResp.StatusCode, strings.TrimSpace(string(raw[:n])))
	}
	if loc := dlResp.Header.Get("Location"); loc != fmt.Sprintf("/images/%d/operations", imgID) {
		t.Fatalf("download Location = %q, want /images/%d/operations", loc, imgID)
	}
	var dlOps []any
	if err := json.NewDecoder(dlResp.Body).Decode(&dlOps); err != nil {
		t.Fatalf("decode download response: %v", err)
	}
	if len(dlOps) != 1 {
		t.Fatalf("download response ops = %+v, want 1 running record", dlOps)
	}
	dlOp := e2eObj(t, dlOps[0])
	if dlOp["node_id"] != float64(nodeID) || dlOp["action"] != "download" || dlOp["result"] != "running" {
		t.Fatalf("download op = %+v, want node %d download running", dlOp, nodeID)
	}

	// 5. 轮询 operations 直至 download 终态 success（后台 goroutine 先
	// 受理再 WaitTask，fake 任务立即 done）。
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw := e2eDo(t, client, adminToken, base, http.MethodGet,
			fmt.Sprintf("/images/%d/operations", imgID), nil, http.StatusOK)
		ops := raw.([]any)
		if len(ops) > 0 {
			op := e2eObj(t, ops[0])
			if op["result"] == "success" {
				if op["node_id"] != float64(nodeID) || op["action"] != "download" {
					t.Fatalf("success op = %+v, want node %d download", op, nodeID)
				}
				if upid, _ := op["upid"].(string); !strings.HasPrefix(upid, "UPID:") {
					t.Fatalf("success op upid = %v, want a UPID", op["upid"])
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("image download did not finish within 10s (ops: %+v)", raw)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 6. 节点状态翻转：downloaded=true 且 volid 为 local:import/... 卷 ID
	//（fake 的 download-url 端点把文件写入了 importFiles）。
	statuses = e2eDo(t, client, adminToken, base, http.MethodGet,
		fmt.Sprintf("/images/%d/nodes-status?zone_id=%d", imgID, zoneID), nil, http.StatusOK).([]any)
	if len(statuses) != 1 {
		t.Fatalf("nodes-status after download = %+v, want 1 status", statuses)
	}
	st0 = e2eObj(t, statuses[0])
	if st0["downloaded"] != true || st0["volid"] != "local:import/debian-12-genericcloud-amd64.qcow2" {
		t.Fatalf("nodes-status[0] after download = %+v, want downloaded=true with volid", st0)
	}

	// 7. 创建 VM：调度到持有镜像的节点（pve1），scsi0 import-from 使用
	// volid 而非文件路径（与旧版 node_images 路径语义不同）。
	created := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/vms", map[string]any{
		"name": "e2e-img-vm", "cpu": 1, "mem_mb": 1024, "disk_gb": 10,
		"image_id": imgID, "storage_type_id": stID, "zone_id": zoneID, "password": "s3cret-pw",
	}, http.StatusCreated))
	vmID := int64(created["id"].(float64))
	deadline = time.Now().Add(15 * time.Second)
	for {
		detail := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet, fmt.Sprintf("/vms/%d", vmID), nil, http.StatusOK))
		if detail["status"] == "failed" {
			t.Fatalf("provisioning failed: %v", detail["provision_error"])
		}
		if detail["status"] != "creating" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provisioning did not finish within 15s")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if vm := fakePVE.get(100); vm == nil {
		t.Fatal("fake pve has no VM 100 after image-scheduled create")
	} else {
		scsi0 := fmt.Sprintf("%v", vm.createBody["scsi0"])
		if !strings.Contains(scsi0, "import-from=local:import/debian-12-genericcloud-amd64.qcow2") {
			t.Fatalf("scsi0 = %q, want import-from volid of the downloaded image", scsi0)
		}
	}
	detail := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodGet, fmt.Sprintf("/vms/%d", vmID), nil, http.StatusOK))
	if detail["node_id"] != float64(nodeID) || detail["pve_vmid"] != float64(100) {
		t.Fatalf("scheduled vm = node_id=%v pve_vmid=%v, want node %d vmid 100",
			detail["node_id"], detail["pve_vmid"], nodeID)
	}
	// 销毁，保持数据干净。
	e2eDo(t, client, adminToken, base, http.MethodDelete, fmt.Sprintf("/vms/%d", vmID), nil, http.StatusNoContent)

	// 8. 下载失败路径：fake 对 e2e-fail- 前缀的文件名拒绝受理
	//（HTTP 500 + errors），操作落 failed 且 upid 为空、错误消息脱敏
	//（不含内部 host:port）。
	failImg := e2eObj(t, e2eDo(t, client, adminToken, base, http.MethodPost, "/images", map[string]any{
		"name": "e2e-image-fail", "default_user": "debian",
		"download_url": "https://cloud.debian.org/images/cloud/bookworm/latest/e2e-fail-image.qcow2",
	}, http.StatusCreated))
	failImgID := int64(failImg["id"].(float64))
	e2eDo(t, client, adminToken, base, http.MethodPost, fmt.Sprintf("/images/%d/download", failImgID),
		map[string]any{"node_ids": []int64{nodeID}}, http.StatusAccepted)
	deadline = time.Now().Add(10 * time.Second)
	var failedOp map[string]any
	for {
		raw := e2eDo(t, client, adminToken, base, http.MethodGet,
			fmt.Sprintf("/images/%d/operations", failImgID), nil, http.StatusOK)
		ops := raw.([]any)
		if len(ops) > 0 {
			failedOp = e2eObj(t, ops[0])
			if failedOp["result"] == "failed" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed image download did not settle within 10s (ops: %+v)", raw)
		}
		time.Sleep(100 * time.Millisecond)
	}
	msg, _ := failedOp["error_message"].(string)
	if !strings.Contains(msg, "simulated download failure") || strings.Contains(msg, "127.0.0.1") {
		t.Fatalf("failed op error_message = %q, want sanitized message containing simulated download failure", msg)
	}
	if upid, ok := failedOp["upid"]; ok && upid != "" {
		t.Fatalf("failed op upid = %v, want empty (受理失败)", upid)
	}
}
