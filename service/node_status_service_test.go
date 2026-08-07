package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
)

// fakeNodeStatusRepo 是供服务测试使用的可脚本化 NodeStatusRepository。
type fakeNodeStatusRepo struct {
	nodes []model.PVENode
	err   error
}

func (f *fakeNodeStatusRepo) GetNode(ctx context.Context, id int64) (*model.PVENode, error) {
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

// scriptedStatusPVE 是可脚本化的假 PVE 服务器：按节点 PVE 名预置
// status/network/rrddata 三个端点的响应（或注入错误），并记录收到的请求
// 供断言。
type scriptedStatusPVE struct {
	mu        sync.Mutex
	status    map[string]*pve.NodeStatusData // 键：节点 PVE 名
	network   map[string][]pve.NetIface
	rrddata   map[string][]rrdTestPoint // rrddata 时间序列点
	statusErr map[string]bool           // 键：节点 PVE 名 -> status 端点返回 500
	rrdErr    map[string]bool           // 键：节点 PVE 名 -> rrddata 端点返回 500
	statusMsg map[string]string         // 键：节点 PVE 名 -> status 错误体消息（覆盖默认 "status boom"）
	requests  []string                  // 收到的路径（断言用）
}

// rrdTestPoint 是假服务器 rrddata 响应的时间序列点（netin/netout 单位
// bytes/s，与 PVE 一致）。
type rrdTestPoint struct {
	Time   int64   `json:"time"`
	NetIn  float64 `json:"netin"`
	NetOut float64 `json:"netout"`
}

func newScriptedStatusPVE() *scriptedStatusPVE {
	return &scriptedStatusPVE{
		status:    map[string]*pve.NodeStatusData{},
		network:   map[string][]pve.NetIface{},
		rrddata:   map[string][]rrdTestPoint{},
		statusErr: map[string]bool{},
		rrdErr:    map[string]bool{},
		statusMsg: map[string]string{},
	}
}

func (s *scriptedStatusPVE) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// 期望形如 /nodes/{node}/status、/nodes/{node}/network、
		// /nodes/{node}/rrddata。
		if len(parts) != 3 || parts[0] != "nodes" {
			http.NotFound(w, r)
			return
		}
		node := parts[1]
		s.requests = append(s.requests, r.URL.Path)
		switch parts[2] {
		case "status":
			if s.statusErr[node] {
				msg := "status boom"
				if m, ok := s.statusMsg[node]; ok {
					msg = m
				}
				writeStatusPVEError(w, http.StatusInternalServerError, "root", msg)
				return
			}
			st := s.status[node]
			if st == nil {
				writeStatusPVEData(w, map[string]any{})
				return
			}
			writeStatusPVEData(w, st)
		case "network":
			writeStatusPVEData(w, s.network[node])
		case "rrddata":
			if s.rrdErr[node] {
				writeStatusPVEError(w, http.StatusInternalServerError, "root", "rrddata boom")
				return
			}
			writeStatusPVEData(w, s.rrddata[node])
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

func writeStatusPVEData(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writeStatusPVEError(w http.ResponseWriter, status int, key, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": map[string]string{key: message}})
}

// newNodeStatusTestService 装配 NodeStatusService：fake 节点仓库 + 假 PVE
// 服务器（SetClientFactory 指向它）。
func newNodeStatusTestService(t *testing.T, nodeRepo *fakeNodeStatusRepo, srv *scriptedStatusPVE) *NodeStatusService {
	t.Helper()
	svc := NewNodeStatusService(nodeRepo)
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	svc.SetClientFactory(func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient(host, apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	})
	return svc
}

// statusTestNode 是服务测试的标准节点：业务名 pve1、PVE 集群名 aeolian1。
func statusTestNode() model.PVENode {
	return model.PVENode{ID: 7, ZoneID: 1, Name: "pve1", PveName: "aeolian1",
		Host: "10.0.0.1", Port: 8006, APIUser: "root@pam!spark", APITokenSecret: "s1", Enabled: true}
}

// fullStatus 是聚合成功用例的标准 status 负载（PVE 8 旧字段风格）。
func fullStatus() *pve.NodeStatusData {
	return &pve.NodeStatusData{
		Node: "aeolian1", Status: "online", CPU: 0.42, CPUs: 8, MaxCPU: 8,
		Mem: 8589934592, MaxMem: 17179869184,
		Rootfs: pve.RootfsInfo{Total: 21474836480, Used: 10737418240}, MaxRootfs: 21474836480,
		Uptime: 86400, Version: "8.2.4", KVersion: "6.8.12-2-pve",
		Loadavg: []string{"0.12", "0.08", "0.05"},
	}
}

// fullNetIO 是聚合成功用例的标准 rrddata 时间序列：取最后一个点作为
// 当前吞吐。
func fullNetIO() []rrdTestPoint {
	return []rrdTestPoint{
		{Time: 1786107660, NetIn: 100.0, NetOut: 50.0},
		{Time: 1786107720, NetIn: 299570.925, NetOut: 14786.975},
	}
}

// TestGetStatusNodeNotFound 验证节点不存在时返回 KindNotFound。
func TestNodeStatusGetNodeNotFound(t *testing.T) {
	svc := newNodeStatusTestService(t, &fakeNodeStatusRepo{nodes: []model.PVENode{statusTestNode()}}, newScriptedStatusPVE())
	_, err := svc.GetStatus(context.Background(), 99)
	assertServiceErrorKind(t, err, KindNotFound)
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *Error", err)
	}
	if !strings.Contains(se.Message, "not found") {
		t.Fatalf("err message = %q, want not found", se.Message)
	}
}

// TestGetStatusNodeRepoError 验证仓库非 ErrNoRows 错误原样包装返回（不透传
// not_found 语义）。
func TestNodeStatusNodeRepoError(t *testing.T) {
	repo := &fakeNodeStatusRepo{err: pgx.ErrTxClosed}
	svc := newNodeStatusTestService(t, repo, newScriptedStatusPVE())
	_, err := svc.GetStatus(context.Background(), 1)
	if err == nil {
		t.Fatal("GetStatus succeeded, want repo error")
	}
	var se *Error
	if errors.As(err, &se) {
		t.Fatalf("err = %v, want plain wrapped error, not *Error", err)
	}
	if !strings.Contains(err.Error(), "get node 1") {
		t.Fatalf("err = %q, want wrapped context", err.Error())
	}
}

// TestGetStatusPVEUnavailable 验证任一 PVE 调用失败（500 / 服务器关闭）
// 整体降级为 KindNodeUnavailable，且消息经脱敏不泄露内部细节。
func TestNodeStatusPVEUnavailable(t *testing.T) {
	t.Run("upstream 500 on status endpoint", func(t *testing.T) {
		srv := newScriptedStatusPVE()
		srv.statusErr["aeolian1"] = true
		svc := newNodeStatusTestService(t, &fakeNodeStatusRepo{nodes: []model.PVENode{statusTestNode()}}, srv)
		_, err := svc.GetStatus(context.Background(), 7)
		assertServiceErrorKind(t, err, KindNodeUnavailable)
		var se *Error
		errors.As(err, &se)
		// 消息只含脱敏摘要与节点名，不含内部 base URL / 请求路径。
		if strings.Contains(se.Message, "http") || strings.Contains(se.Message, "api2/json") {
			t.Fatalf("err message %q leaks internal details", se.Message)
		}
		if !strings.Contains(se.Message, "aeolian1") || !strings.Contains(se.Message, "status boom") {
			t.Fatalf("err message = %q, want node name + sanitized reason", se.Message)
		}
	})

	t.Run("network error when server closed", func(t *testing.T) {
		repo := &fakeNodeStatusRepo{nodes: []model.PVENode{statusTestNode()}}
		svc := NewNodeStatusService(repo)
		ts := httptest.NewServer(newScriptedStatusPVE().handler())
		// 服务器立即关闭，模拟节点不可达。
		ts.Close()
		svc.SetClientFactory(func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient(host, apiUser, apiTokenSecret,
				pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
		})
		_, err := svc.GetStatus(context.Background(), 7)
		assertServiceErrorKind(t, err, KindNodeUnavailable)
		var se *Error
		errors.As(err, &se)
		// 网络层错误脱敏：不泄露 host:port / URL / API token（测试节点
		// APITokenSecret 为 "s1"）。
		if strings.Contains(se.Message, "10.0.0.1") || strings.Contains(se.Message, "https://") ||
			strings.Contains(se.Message, "s1") {
			t.Fatalf("err message %q leaks internal details", se.Message)
		}
	})

	t.Run("huge upstream error body is truncated", func(t *testing.T) {
		srv := newScriptedStatusPVE()
		srv.statusErr["aeolian1"] = true
		// 构造超长错误体：PVE 错误体最大可达 1MiB，脱敏后必须按 rune
		// 截断，绝不让超长消息原样进入降级错误。
		long := strings.Repeat("大", 3000) // 3000 个中文字符（> 500 上限）
		srv.statusMsg["aeolian1"] = long
		svc := newNodeStatusTestService(t, &fakeNodeStatusRepo{nodes: []model.PVENode{statusTestNode()}}, srv)
		_, err := svc.GetStatus(context.Background(), 7)
		assertServiceErrorKind(t, err, KindNodeUnavailable)
		var se *Error
		errors.As(err, &se)
		if got := len([]rune(se.Message)); got > maxNodeStatusErrorLen+50 {
			t.Fatalf("err message length = %d runes, want <= %d (truncated)", got, maxNodeStatusErrorLen)
		}
	})
}

// TestGetStatusAggregates 验证聚合成功：Status/Network/NetIO 全部返回，
// NetIO 取 rrddata 最后一个点，并发请求三个端点各一次。
func TestNodeStatusAggregates(t *testing.T) {
	srv := newScriptedStatusPVE()
	srv.status["aeolian1"] = fullStatus()
	srv.network["aeolian1"] = []pve.NetIface{
		{Iface: "vmbr0", Type: "bridge", Address: "10.0.0.1/24"},
		{Iface: "eth0", Type: "eth", Address: "10.0.0.2/24"},
	}
	srv.rrddata["aeolian1"] = fullNetIO()
	svc := newNodeStatusTestService(t, &fakeNodeStatusRepo{nodes: []model.PVENode{statusTestNode()}}, srv)

	res, err := svc.GetStatus(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if res.Node == nil || res.Node.ID != 7 || res.Node.Name != "pve1" {
		t.Fatalf("node = %+v, want pve1 id 7", res.Node)
	}
	if res.Status == nil || res.Status.Status != "online" || res.Status.CPU != 0.42 ||
		res.Status.MaxMem != 17179869184 || len(res.Status.Loadavg) != 3 {
		t.Fatalf("status = %+v", res.Status)
	}
	if len(res.Network) != 2 || res.Network[0].Iface != "vmbr0" || res.Network[1].Iface != "eth0" {
		t.Fatalf("network = %+v", res.Network)
	}
	// NetIO 取 rrddata 最后一个点（bytes/s）。
	if res.NetIO == nil || res.NetIO.NetIn != 299570.925 || res.NetIO.NetOut != 14786.975 {
		t.Fatalf("netio = %+v, want last point 299570.925/14786.975", res.NetIO)
	}
	// 三个端点各请求一次。
	if len(srv.requests) != 3 {
		t.Fatalf("requests = %v, want 3 endpoint hits", srv.requests)
	}
}

// TestNodeStatusRRDDataFailDegrades 验证 rrddata 失败（500）时 GetStatus
// 仍成功返回：status/network 照常聚合，NetIO 降级为零值（非 nil，契约
// 要求 net_io 恒为非空对象）。设计决策 D3：rrddata 为增强字段（需
// Sys.Audit 权限），权限不足或临时失败不应拖垮整个状态查询。
func TestNodeStatusRRDDataFailDegrades(t *testing.T) {
	srv := newScriptedStatusPVE()
	srv.status["aeolian1"] = fullStatus()
	srv.network["aeolian1"] = []pve.NetIface{{Iface: "vmbr0", Type: "bridge", Address: "10.0.0.1/24"}}
	srv.rrdErr["aeolian1"] = true
	svc := newNodeStatusTestService(t, &fakeNodeStatusRepo{nodes: []model.PVENode{statusTestNode()}}, srv)

	res, err := svc.GetStatus(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetStatus: %v, want success despite rrddata failure", err)
	}
	if res.Status == nil || res.Status.Status != "online" || res.Status.CPU != 0.42 {
		t.Fatalf("status = %+v, want online with cpu 0.42", res.Status)
	}
	if len(res.Network) != 1 || res.Network[0].Iface != "vmbr0" {
		t.Fatalf("network = %+v, want vmbr0", res.Network)
	}
	// NetIO 降级为零值对象（rrddata 500 → net_in/net_out 全零）。
	if res.NetIO == nil || res.NetIO.NetIn != 0 || res.NetIO.NetOut != 0 {
		t.Fatalf("netio = %+v, want zero values", res.NetIO)
	}
}

// TestNodeStatusCoreFailWholeDegrade 验证 status 失败时即使 rrddata 正常
// 仍整体 503（核心组失败即降级，增强字段的成功无法挽回，NetIO 无关）。
func TestNodeStatusCoreFailWholeDegrade(t *testing.T) {
	srv := newScriptedStatusPVE()
	srv.statusErr["aeolian1"] = true
	srv.rrddata["aeolian1"] = fullNetIO() // rrddata 正常
	svc := newNodeStatusTestService(t, &fakeNodeStatusRepo{nodes: []model.PVENode{statusTestNode()}}, srv)

	_, err := svc.GetStatus(context.Background(), 7)
	assertServiceErrorKind(t, err, KindNodeUnavailable)
}

// TestGetStatusPVE9Defaults 验证 PVE 9 兼容：status 无 status/node/version
// 等旧字段时，服务层把 Status 补为 "online"（请求成功即在线），
// cpuinfo/memory/pveversion 对象正常透传。
func TestNodeStatusPVE9Defaults(t *testing.T) {
	srv := newScriptedStatusPVE()
	srv.status["aeolian1"] = &pve.NodeStatusData{
		CPU:     0.0056,
		CPUInfo: &pve.CPUInfo{Cpus: 4, Cores: 2, Sockets: 1, Model: "Intel Xeon"},
		Memory:  &pve.MemoryInfo{Total: 12442832896, Used: 2228772864, Free: 8963547136, Available: 10214060032},
		Rootfs:  pve.RootfsInfo{Total: 22538600448, Used: 13589426176},
		Uptime:  6008485, PveVersion: "pve-manager/9.1.1/1", KVersion: "Linux 6.17.2-1-pve",
		Loadavg: []string{"0.02", "0.04", "0.00"},
	}
	srv.network["aeolian1"] = []pve.NetIface{{Iface: "vmbr0", Type: "bridge", Address: "10.0.0.251/24"}}
	srv.rrddata["aeolian1"] = []rrdTestPoint{{Time: 1786107720, NetIn: 123.0, NetOut: 45.0}}
	svc := newNodeStatusTestService(t, &fakeNodeStatusRepo{nodes: []model.PVENode{statusTestNode()}}, srv)

	res, err := svc.GetStatus(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	// PVE 9 无 status 字段 → 服务层补 "online"。
	if res.Status == nil || res.Status.Status != "online" {
		t.Fatalf("status.status = %+v, want online", res.Status)
	}
	if res.Status.CPUInfo == nil || res.Status.CPUInfo.Cpus != 4 {
		t.Fatalf("cpuinfo = %+v", res.Status.CPUInfo)
	}
	if res.Status.Memory == nil || res.Status.Memory.Total != 12442832896 {
		t.Fatalf("memory = %+v", res.Status.Memory)
	}
	if res.Status.PveVersion != "pve-manager/9.1.1/1" || res.Status.Version != "" {
		t.Fatalf("version fields = %+v", res.Status)
	}
	if res.NetIO == nil || res.NetIO.NetIn != 123.0 || res.NetIO.NetOut != 45.0 {
		t.Fatalf("netio = %+v", res.NetIO)
	}
}

// TestGetStatusMissingFields 验证 PVE 响应缺字段（如 loadavg、maxcpu）时
// 按零值容错聚合，不报错；rrddata 为空时 NetIO 为全零。
func TestNodeStatusMissingFields(t *testing.T) {
	srv := newScriptedStatusPVE()
	srv.status["aeolian1"] = &pve.NodeStatusData{Node: "aeolian1", Status: "online", CPU: 0.1}
	srv.network["aeolian1"] = []pve.NetIface{{Iface: "lo", Type: "lo", Address: "127.0.0.1/8"}}
	// rrddata 缺配置：假服务器返回 null，NodeNetIO 容错为全零。
	srv.rrddata["aeolian1"] = nil
	svc := newNodeStatusTestService(t, &fakeNodeStatusRepo{nodes: []model.PVENode{statusTestNode()}}, srv)

	res, err := svc.GetStatus(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if res.Status.CPUs != 0 || res.Status.MaxCPU != 0 || res.Status.MaxMem != 0 || res.Status.Loadavg != nil {
		t.Fatalf("status = %+v, want zero values for missing fields", res.Status)
	}
	if res.Status.Status != "online" {
		t.Fatalf("status = %q, want online", res.Status.Status)
	}
	if res.NetIO == nil || res.NetIO.NetIn != 0 || res.NetIO.NetOut != 0 {
		t.Fatalf("netio = %+v, want zero values", res.NetIO)
	}
}

// TestGetStatusDisabledNode 验证禁用节点同样返回实时状态（状态查询不受
// enabled 影响，与镜像扫描的启用过滤不同）。
func TestNodeStatusDisabledNode(t *testing.T) {
	node := statusTestNode()
	node.Enabled = false
	srv := newScriptedStatusPVE()
	srv.status["aeolian1"] = fullStatus()
	srv.network["aeolian1"] = []pve.NetIface{}
	srv.rrddata["aeolian1"] = []rrdTestPoint{}
	svc := newNodeStatusTestService(t, &fakeNodeStatusRepo{nodes: []model.PVENode{node}}, srv)

	res, err := svc.GetStatus(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if res.Node == nil || res.Node.Enabled {
		t.Fatalf("node = %+v, want disabled node returned", res.Node)
	}
	if res.Status == nil || res.Status.Status != "online" {
		t.Fatalf("status = %+v", res.Status)
	}
	if res.NetIO == nil || res.NetIO.NetIn != 0 || res.NetIO.NetOut != 0 {
		t.Fatalf("netio = %+v, want zero values for empty rrddata", res.NetIO)
	}
}
