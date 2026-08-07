package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
	"spark/service"
)

// handlerStatusNode 是 handler 测试的标准节点。
func handlerStatusNode() model.PVENode {
	return model.PVENode{ID: 7, ZoneID: 1, Name: "pve1", PveName: "aeolian1",
		Host: "10.0.0.1", Port: 8006, APIUser: "root@pam!spark", APITokenSecret: "s1",
		Enabled: true, CreatedAt: imageTestTime}
}

// fakeStatusNodeRepo 是 handler 测试的 NodeStatusRepository 替身。
type fakeStatusNodeRepo struct {
	nodes []model.PVENode
}

func (f *fakeStatusNodeRepo) GetNode(ctx context.Context, id int64) (*model.PVENode, error) {
	for i := range f.nodes {
		if f.nodes[i].ID == id {
			n := f.nodes[i]
			return &n, nil
		}
	}
	return nil, pgx.ErrNoRows
}

// handlerStatusPVE 是 handler 测试的可脚本化假 PVE 服务器：按节点 PVE 名
// 预置 status/network/rrddata 响应或注入 status 端点错误。rawStatus 用于
// 提供无法用 NodeStatusData 表达的原始 JSON 形态（如 PVE 7 的裸数字
// rootfs），优先于 status 使用。
type handlerStatusPVE struct {
	mu        sync.Mutex
	status    map[string]*pve.NodeStatusData
	rawStatus map[string]map[string]any
	network   map[string][]pve.NetIface
	rrddata   map[string][]map[string]any
	statusErr map[string]bool
}

func newHandlerStatusPVE() *handlerStatusPVE {
	return &handlerStatusPVE{
		status:    map[string]*pve.NodeStatusData{},
		rawStatus: map[string]map[string]any{},
		network:   map[string][]pve.NetIface{},
		rrddata:   map[string][]map[string]any{},
		statusErr: map[string]bool{},
	}
}

func (s *handlerStatusPVE) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 3 || parts[0] != "nodes" {
			http.NotFound(w, r)
			return
		}
		node := parts[1]
		switch parts[2] {
		case "status":
			if s.statusErr[node] {
				writeStatusHandlerPVEError(w, http.StatusInternalServerError, "root", "status boom")
				return
			}
			if raw, ok := s.rawStatus[node]; ok {
				writeStatusHandlerPVEData(w, raw)
				return
			}
			st := s.status[node]
			if st == nil {
				writeStatusHandlerPVEData(w, map[string]any{})
				return
			}
			writeStatusHandlerPVEData(w, st)
		case "network":
			writeStatusHandlerPVEData(w, s.network[node])
		case "rrddata":
			writeStatusHandlerPVEData(w, s.rrddata[node])
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

func writeStatusHandlerPVEData(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writeStatusHandlerPVEError(w http.ResponseWriter, status int, key, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": map[string]string{key: message}})
}

// newStatusHandlerTestService 装配 handler 测试的 NodeStatusService：fake
// 节点仓库 + 假 PVE 服务器（SetClientFactory 指向它）。
func newStatusHandlerTestService(t *testing.T, nodeRepo *fakeStatusNodeRepo, srv *handlerStatusPVE) *service.NodeStatusService {
	t.Helper()
	svc := service.NewNodeStatusService(nodeRepo)
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	svc.SetClientFactory(func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient(host, apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	})
	return svc
}

// newStatusHandlerEngine 构建挂载 GET /nodes/:id/status 的 gin 引擎。
func newStatusHandlerEngine(svc *service.NodeStatusService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterNodeStatusRoutes(r.Group("/nodes"), svc)
	return r
}

// statusResponseBody 是 GET /nodes/:id/status 响应负载的测试解码结构。
type statusResponseBody struct {
	ID          int64  `json:"id"`
	ZoneID      int64  `json:"zone_id"`
	Name        string `json:"name"`
	PveName     string `json:"pve_name"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	APIUser     string `json:"api_user"`
	APITokenSet bool   `json:"api_token_set"`
	Enabled     bool   `json:"enabled"`
	Status      struct {
		Status        string `json:"status"`
		UptimeSeconds int64  `json:"uptime_seconds"`
		PveVersion    string `json:"pve_version"`
		KernelVersion string `json:"kernel_version"`
		CPU           struct {
			Cores   int      `json:"cores"`
			Usage   float64  `json:"usage"`
			Loadavg []string `json:"loadavg"`
		} `json:"cpu"`
		Memory struct {
			Total int64   `json:"total"`
			Used  int64   `json:"used"`
			Usage float64 `json:"usage"`
		} `json:"memory"`
		Disk struct {
			Total int64   `json:"total"`
			Used  int64   `json:"used"`
			Usage float64 `json:"usage"`
		} `json:"disk"`
		Network []struct {
			Iface   string `json:"iface"`
			Type    string `json:"type"`
			Address string `json:"address"`
			Active  *bool  `json:"active"`
		} `json:"network"`
		NetIO *struct {
			NetIn  float64 `json:"net_in"`
			NetOut float64 `json:"net_out"`
		} `json:"net_io"`
	} `json:"status"`
}

// TestGetNodeStatusHandlerPVE8 覆盖 GET /nodes/:id/status 的 200 完整负载
// （PVE 8 形态：rootfs 对象与旧字段并存）：配置字段平铺、嵌套 status 对象、
// usage 计算、网络接口列表与节点级吞吐 net_io。
func TestGetNodeStatusHandlerPVE8(t *testing.T) {
	srv := newHandlerStatusPVE()
	srv.status["aeolian1"] = &pve.NodeStatusData{
		Node: "aeolian1", Status: "online", CPU: 0.5, CPUs: 8, MaxCPU: 8,
		Mem: 8589934592, MaxMem: 17179869184, // 使用率 0.5
		// PVE 8：rootfs 为对象（total/used），maxrootfs 缺失（0），
		// disk.total 回退 rootfs.total。
		Rootfs: pve.RootfsInfo{Total: 21474836480, Used: 10737418240}, // 使用率 0.5
		Uptime: 3600, Version: "8.2.4", KVersion: "6.8.12-2-pve",
		Loadavg: []string{"0.12", "0.08"},
	}
	srv.network["aeolian1"] = []pve.NetIface{
		{Iface: "vmbr0", Type: "bridge", Address: "10.0.0.1/24"},
		{Iface: "eth0", Type: "eth", Address: "10.0.0.2/24"},
	}
	srv.rrddata["aeolian1"] = []map[string]any{
		{"time": 1786107660, "netin": 100.0, "netout": 50.0},
		{"time": 1786107720, "netin": 299570.925, "netout": 14786.975},
	}
	svc := newStatusHandlerTestService(t, &fakeStatusNodeRepo{nodes: []model.PVENode{handlerStatusNode()}}, srv)
	r := newStatusHandlerEngine(svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nodes/7/status", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var body statusResponseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// 配置字段平铺。
	if body.ID != 7 || body.ZoneID != 1 || body.Name != "pve1" || body.PveName != "aeolian1" ||
		body.Host != "10.0.0.1" || body.Port != 8006 || body.APIUser != "root@pam!spark" ||
		!body.APITokenSet || !body.Enabled {
		t.Fatalf("config fields = %+v", body)
	}
	// 嵌套状态字段。
	st := body.Status
	if st.Status != "online" || st.UptimeSeconds != 3600 ||
		st.PveVersion != "8.2.4" || st.KernelVersion != "6.8.12-2-pve" {
		t.Fatalf("status fields = %+v", st)
	}
	// CPU：cores 直取 cpus（无 cpuinfo 时回退）、usage 直取 PVE cpu、
	// loadavg 透传。
	if st.CPU.Cores != 8 || st.CPU.Usage != 0.5 || len(st.CPU.Loadavg) != 2 || st.CPU.Loadavg[0] != "0.12" {
		t.Fatalf("cpu = %+v", st.CPU)
	}
	// Memory/Disk：usage = used/total（此处均为 0.5）。
	if st.Memory.Total != 17179869184 || st.Memory.Used != 8589934592 || st.Memory.Usage != 0.5 {
		t.Fatalf("memory = %+v", st.Memory)
	}
	if st.Disk.Total != 21474836480 || st.Disk.Used != 10737418240 || st.Disk.Usage != 0.5 {
		t.Fatalf("disk = %+v", st.Disk)
	}
	// Network：仅接口信息（无 rx/tx 字段），active 为 nil（未返回）。
	if len(st.Network) != 2 {
		t.Fatalf("network = %+v, want 2 entries", st.Network)
	}
	if st.Network[0].Iface != "vmbr0" || st.Network[0].Type != "bridge" ||
		st.Network[0].Address != "10.0.0.1/24" || st.Network[0].Active != nil {
		t.Fatalf("network[0] = %+v", st.Network[0])
	}
	if st.Network[1].Iface != "eth0" || st.Network[1].Type != "eth" {
		t.Fatalf("network[1] = %+v", st.Network[1])
	}
	// NetIO：rrddata 最后一点的吞吐（bytes/s）。
	if st.NetIO == nil || st.NetIO.NetIn != 299570.925 || st.NetIO.NetOut != 14786.975 {
		t.Fatalf("net_io = %+v, want last point 299570.925/14786.975", st.NetIO)
	}
}

// TestGetNodeStatusHandlerPVE7 覆盖 PVE 7 形态：rootfs 为裸数字（已用
// 字节）、总量由 maxrootfs 提供 → disk.total 取 maxrootfs、disk.used 取
// rootfs 裸数字（PVE 7 → 8/9 的回退链端到端覆盖）。
func TestGetNodeStatusHandlerPVE7(t *testing.T) {
	srv := newHandlerStatusPVE()
	// PVE 7 原始 JSON：rootfs 是裸数字（已用字节），maxrootfs 是总量。
	srv.rawStatus["aeolian1"] = map[string]any{
		"node": "aeolian1", "status": "online", "cpu": 0.3,
		"cpus": 4, "maxcpu": 4,
		"mem": int64(4294967296), "maxmem": int64(8589934592),
		"rootfs": int64(10737418240), "maxrootfs": int64(21474836480),
		"uptime": 1800, "version": "7.4-19", "kversion": "5.15.152-1-pve",
		"loadavg": []string{"0.05", "0.02"},
	}
	srv.network["aeolian1"] = []pve.NetIface{{Iface: "vmbr0", Type: "bridge", Address: "10.0.0.1/24"}}
	srv.rrddata["aeolian1"] = []map[string]any{{"time": 1786107720, "netin": 10.0, "netout": 5.0}}
	svc := newStatusHandlerTestService(t, &fakeStatusNodeRepo{nodes: []model.PVENode{handlerStatusNode()}}, srv)
	r := newStatusHandlerEngine(svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nodes/7/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var body statusResponseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	st := body.Status
	// Memory：PVE 7 用 maxmem/mem。
	if st.Memory.Total != 8589934592 || st.Memory.Used != 4294967296 {
		t.Fatalf("memory = %+v, want maxmem/mem 8589934592/4294967296", st.Memory)
	}
	// Disk：total 取 maxrootfs（裸数字形态的 rootfs 无 total），used 取
	// rootfs 裸数字，usage = used/maxrootfs（0.5）。
	if st.Disk.Total != 21474836480 || st.Disk.Used != 10737418240 || st.Disk.Usage != 0.5 {
		t.Fatalf("disk = %+v, want total=maxrootfs 21474836480, used=rootfs 10737418240", st.Disk)
	}
	// PveVersion：PVE 7 用 version 字段。
	if st.PveVersion != "7.4-19" {
		t.Fatalf("pve_version = %q, want 7.4-19", st.PveVersion)
	}
	// NetIO 正常透传（rrddata 不受 PVE 7/8/9 差异影响）。
	if st.NetIO == nil || st.NetIO.NetIn != 10.0 || st.NetIO.NetOut != 5.0 {
		t.Fatalf("net_io = %+v, want 10.0/5.0", st.NetIO)
	}
}

// TestGetNodeStatusHandlerPVE9 覆盖 PVE 9 响应风格：cpuinfo/memory/
// pveversion 对象回退链、status 补默认 "online"、network 的 active 数字
// 1/0 转为 boolean 输出。
func TestGetNodeStatusHandlerPVE9(t *testing.T) {
	srv := newHandlerStatusPVE()
	// PVE 9：无 status/node/version/mem/maxmem/cpus/maxcpu 旧字段。
	srv.status["aeolian1"] = &pve.NodeStatusData{
		CPU:     0.0056,
		CPUInfo: &pve.CPUInfo{Cpus: 4, Cores: 2, Sockets: 1, Model: "Intel Xeon"},
		Memory:  &pve.MemoryInfo{Total: 12442832896, Used: 2228772864, Free: 8963547136, Available: 10214060032},
		Rootfs:  pve.RootfsInfo{Total: 22538600448, Used: 13589426176},
		Uptime:  6008485, PveVersion: "pve-manager/9.1.1/1", KVersion: "Linux 6.17.2-1-pve",
		Loadavg: []string{"0.02", "0.04", "0.00"},
	}
	srv.network["aeolian1"] = []pve.NetIface{
		{Iface: "vmbr0", Type: "bridge", Address: "10.0.0.251/24", Active: pveBoolPtr(true)},
		{Iface: "nic0", Type: "eth", Address: "", Active: pveBoolPtr(false)},
	}
	srv.rrddata["aeolian1"] = []map[string]any{{"time": 1786107720, "netin": 123.0, "netout": 45.0}}
	svc := newStatusHandlerTestService(t, &fakeStatusNodeRepo{nodes: []model.PVENode{handlerStatusNode()}}, srv)
	r := newStatusHandlerEngine(svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nodes/7/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var body statusResponseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	st := body.Status
	// PVE 9 无 status 字段 → service 补 "online"。
	if st.Status != "online" {
		t.Fatalf("status = %q, want online", st.Status)
	}
	// Cores 回退链：cpuinfo.cpus（PVE 9）→ cpus → maxcpu。
	if st.CPU.Cores != 4 || st.CPU.Usage != 0.0056 {
		t.Fatalf("cpu = %+v, want cores 4", st.CPU)
	}
	// Memory 回退链：memory 对象（PVE 9）→ maxmem/mem。
	if st.Memory.Total != 12442832896 || st.Memory.Used != 2228772864 {
		t.Fatalf("memory = %+v", st.Memory)
	}
	// Disk：rootfs 对象（PVE 9）。
	if st.Disk.Total != 22538600448 || st.Disk.Used != 13589426176 {
		t.Fatalf("disk = %+v", st.Disk)
	}
	// PveVersion 回退链：pveversion（PVE 9）→ version。
	if st.PveVersion != "pve-manager/9.1.1/1" {
		t.Fatalf("pve_version = %q, want pve-manager/9.1.1/1", st.PveVersion)
	}
	// network 的 active（数字 1/0 存储）输出为 boolean。
	if len(st.Network) != 2 {
		t.Fatalf("network = %+v, want 2 entries", st.Network)
	}
	if st.Network[0].Active == nil || !*st.Network[0].Active {
		t.Fatalf("network[0].active = %+v, want true", st.Network[0].Active)
	}
	if st.Network[1].Active == nil || *st.Network[1].Active {
		t.Fatalf("network[1].active = %+v, want false", st.Network[1].Active)
	}
	// NetIO 透传。
	if st.NetIO == nil || st.NetIO.NetIn != 123.0 || st.NetIO.NetOut != 45.0 {
		t.Fatalf("net_io = %+v", st.NetIO)
	}
}

// pveBoolPtr 构造 *pve.PveBool 指针（测试用）。
func pveBoolPtr(v bool) *pve.PveBool {
	b := pve.PveBool(v)
	return &b
}

// TestGetNodeStatusHandlerMissingTraffic 覆盖 rrddata 缺失（空数组）时
// net_io 为全零，接口列表仍完整返回（D2 降级决策）。
func TestGetNodeStatusHandlerMissingTraffic(t *testing.T) {
	srv := newHandlerStatusPVE()
	srv.status["aeolian1"] = &pve.NodeStatusData{Node: "aeolian1", Status: "online", CPU: 0.1, MaxCPU: 4, MaxMem: 1024}
	srv.network["aeolian1"] = []pve.NetIface{{Iface: "vmbr0", Type: "bridge", Address: "10.0.0.1/24"}}
	srv.rrddata["aeolian1"] = []map[string]any{}
	svc := newStatusHandlerTestService(t, &fakeStatusNodeRepo{nodes: []model.PVENode{handlerStatusNode()}}, srv)
	r := newStatusHandlerEngine(svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nodes/7/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var body statusResponseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Status.Network) != 1 || body.Status.Network[0].Iface != "vmbr0" {
		t.Fatalf("network = %+v, want vmbr0", body.Status.Network)
	}
	// rrddata 为空：net_io 返回全零（容错不报错）。
	if body.Status.NetIO == nil || body.Status.NetIO.NetIn != 0 || body.Status.NetIO.NetOut != 0 {
		t.Fatalf("net_io = %+v, want zero values", body.Status.NetIO)
	}
}

// TestGetNodeStatusHandlerCoresFallback 验证 Cores 回退链（无 cpuinfo、
// cpus 为 0 时回退 maxcpu）与 Memory 回退链（无 memory 对象时用
// maxmem/mem）。
func TestGetNodeStatusHandlerCoresFallback(t *testing.T) {
	srv := newHandlerStatusPVE()
	srv.status["aeolian1"] = &pve.NodeStatusData{
		Node: "aeolian1", Status: "online", CPU: 0.1, MaxCPU: 4, MaxMem: 1024, Mem: 512,
	}
	srv.network["aeolian1"] = []pve.NetIface{}
	srv.rrddata["aeolian1"] = []map[string]any{}
	svc := newStatusHandlerTestService(t, &fakeStatusNodeRepo{nodes: []model.PVENode{handlerStatusNode()}}, srv)
	r := newStatusHandlerEngine(svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nodes/7/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var body statusResponseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status.CPU.Cores != 4 {
		t.Fatalf("cores = %d, want 4 (maxcpu fallback)", body.Status.CPU.Cores)
	}
	if body.Status.Memory.Total != 1024 || body.Status.Memory.Used != 512 {
		t.Fatalf("memory = %+v, want maxmem/mem fallback 1024/512", body.Status.Memory)
	}
}

// TestGetNodeStatusHandlerErrors 覆盖 404（节点不存在）与 503（PVE 不可达，
// 错误码 node_unavailable）的错误契约。
func TestGetNodeStatusHandlerErrors(t *testing.T) {
	t.Run("node not found returns 404", func(t *testing.T) {
		svc := newStatusHandlerTestService(t, &fakeStatusNodeRepo{nodes: []model.PVENode{handlerStatusNode()}}, newHandlerStatusPVE())
		r := newStatusHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nodes/99/status", nil))
		assertHandlerError(t, w, http.StatusNotFound, CodeNotFound)
	})

	t.Run("pve unavailable returns 503 node_unavailable", func(t *testing.T) {
		srv := newHandlerStatusPVE()
		srv.statusErr["aeolian1"] = true
		svc := newStatusHandlerTestService(t, &fakeStatusNodeRepo{nodes: []model.PVENode{handlerStatusNode()}}, srv)
		r := newStatusHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nodes/7/status", nil))
		assertHandlerError(t, w, http.StatusServiceUnavailable, CodeNodeUnavailable)
	})

	t.Run("non-numeric id returns 400", func(t *testing.T) {
		svc := newStatusHandlerTestService(t, &fakeStatusNodeRepo{nodes: []model.PVENode{handlerStatusNode()}}, newHandlerStatusPVE())
		r := newStatusHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nodes/abc/status", nil))
		assertHandlerError(t, w, http.StatusBadRequest, CodeBadRequest)
	})
}

// assertHandlerError 断言统一错误契约的响应：HTTP 状态码、body 中的错误码
// 与 x-ms-error-code 头。
func assertHandlerError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, status, w.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != code {
		t.Fatalf("error code = %q, want %q", body.Error.Code, code)
	}
	if hdr := w.Header().Get("X-Ms-Error-Code"); hdr != code {
		t.Fatalf("x-ms-error-code = %q, want %q", hdr, code)
	}
}

// TestGetNodeStatusHandlerLeakFree 验证 503 响应消息不含 PVE 内部细节
// （内部 URL/主机地址）与节点 API token（测试节点 APITokenSecret "s1"），
// 符合错误消息脱敏红线。
func TestGetNodeStatusHandlerLeakFree(t *testing.T) {
	srv := newHandlerStatusPVE()
	srv.statusErr["aeolian1"] = true
	svc := newStatusHandlerTestService(t, &fakeStatusNodeRepo{nodes: []model.PVENode{handlerStatusNode()}}, srv)
	r := newStatusHandlerEngine(svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nodes/7/status", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	for _, leak := range []string{"10.0.0.1", "https://", "api2/json", "aeolian1/status", "s1"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("body %q leaks %q", w.Body.String(), leak)
		}
	}
}
