package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient 启动一个 httptest 服务器，并通过 WithBaseURL 创建一个指向
// 它的 Client。服务器 handler 可以检查请求（认证头、方法、路径、请求体），
// 且必须写出 PVE 风格的响应。
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient("pve1", "root@pam", "spark=uuid",
		WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()),
		WithTimeout(5*time.Second),
	)
	if c.initErr != nil {
		t.Fatalf("NewClient: %v", c.initErr)
	}
	return c, srv
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// TestAuthHeaderNormalization 覆盖可接受的凭证存储形式。
func TestAuthHeaderNormalization(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		secret  string
		want    string
		wantErr bool
	}{
		{"split form", "root@pam", "spark=uuid", "PVEAPIToken=root@pam!spark=uuid", false},
		{"full form", "root@pam", "root@pam!spark=uuid", "PVEAPIToken=root@pam!spark=uuid", false},
		{"tokenid in user", "root@pam!spark", "uuid", "PVEAPIToken=root@pam!spark=uuid", false},
		{"bare secret only", "root@pam", "uuid", "", true},
		{"exclamation in secret", "root@pam", "spark=uuid!x", "PVEAPIToken=root@pam!spark=uuid!x", false},
		{"full form with exclamation in secret", "root@pam", "root@pam!spark=uuid!x", "PVEAPIToken=root@pam!spark=uuid!x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient("pve1", tt.user, tt.secret)
			if tt.wantErr {
				if c.initErr == nil {
					t.Fatalf("NewClient(%q, %q) should fail to build a token header", tt.user, tt.secret)
				}
				// 失败消息绝不能泄露凭证的任何部分
				// （参见 NewClient 中的脱敏处理）。
				msg := c.initErr.Error()
				if strings.Contains(msg, tt.secret) {
					t.Fatalf("initErr %q leaks the token secret %q", msg, tt.secret)
				}
				for _, tok := range strings.FieldsFunc(tt.secret, func(r rune) bool { return r == '!' || r == '=' }) {
					if tok != "" && strings.Contains(msg, tok) {
						t.Fatalf("initErr %q leaks credential fragment %q", msg, tok)
					}
				}
				return
			}
			if c.initErr != nil {
				t.Fatalf("NewClient: %v", c.initErr)
			}
			if c.authHeader != tt.want {
				t.Fatalf("auth header = %q, want %q", c.authHeader, tt.want)
			}
		})
	}
}

// TestNewClientRedactsCredentialsInInitErr 钉住 NewClient 失败消息仅携带
// 主机名和一个被掩码的用户前缀，绝不携带原始 api_user 或令牌密钥，因此
// 日志和客户端可见的错误不会泄露已存储的凭证。
func TestNewClientRedactsCredentialsInInitErr(t *testing.T) {
	c := NewClient("pve1", "root@pam", "supersecretvalue")
	if c.initErr == nil {
		t.Fatal("NewClient should fail to build a token header")
	}
	msg := c.initErr.Error()
	if !strings.Contains(msg, "pve1") {
		t.Fatalf("initErr %q should carry the host", msg)
	}
	if !strings.Contains(msg, "root@pam!***") {
		t.Fatalf("initErr %q should carry the masked user prefix", msg)
	}
	for _, leak := range []string{"root@pam!supersecret", "supersecretvalue", "supersecret"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("initErr %q leaks %q", msg, leak)
		}
	}
}

// TestNewClientStripsHostPort 验证 host 上的 ":port" 后缀会被移除，避免
// base URL 携带两次端口。
func TestNewClientStripsHostPort(t *testing.T) {
	c := NewClient("pve1:8006", "root@pam", "spark=uuid")
	if c.initErr != nil {
		t.Fatalf("NewClient: %v", c.initErr)
	}
	if want := "https://pve1:8006/api2/json"; c.baseURL != want {
		t.Fatalf("baseURL = %q, want %q", c.baseURL, want)
	}
	// 非数字后缀不是端口，必须原样保留。
	c2 := NewClient("pve1:host", "root@pam", "spark=uuid")
	if c2.baseURL != "https://pve1:host:8006/api2/json" {
		t.Fatalf("baseURL = %q", c2.baseURL)
	}
	// 空的端口后缀也会被剥离。
	c3 := NewClient("pve1:", "root@pam", "spark=uuid")
	if c3.initErr != nil {
		t.Fatalf("NewClient(pve1:): %v", c3.initErr)
	}
	if c3.baseURL != "https://pve1:8006/api2/json" {
		t.Fatalf("baseURL = %q, want %q", c3.baseURL, "https://pve1:8006/api2/json")
	}
	// IPv6 字面量不受支持，必须显式失败。
	for _, h := range []string{"::1", "2001:db8::1"} {
		c4 := NewClient(h, "root@pam", "spark=uuid")
		if c4.initErr == nil || !strings.Contains(c4.initErr.Error(), "IPv6") {
			t.Fatalf("NewClient(%q) initErr = %v, want explicit IPv6 error", h, c4.initErr)
		}
	}
}

// TestDoJSONSuccess 验证 2xx 响应返回 data 负载。
func TestDoJSONSuccess(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=root@pam!spark=uuid" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data": {"release": "8.4", "version": "8.4.20"}}`)
	})
	v, err := c.ProbeVersion(context.Background())
	if err != nil {
		t.Fatalf("ProbeVersion: %v", err)
	}
	if v.Release != "8.4" || v.Version != "8.4.20" {
		t.Fatalf("ProbeVersion = %+v", v)
	}
}

// TestUpstreamErrorHTTP200Errors 覆盖 PVE 的 HTTP 200 + errors 封装
// （例如 POST /nodes/{node}/qemu 的校验失败）。
func TestUpstreamErrorHTTP200Errors(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": null, "errors": {"vmid": "VMID 100 already exists"}}`)
	})
	_, err := c.CreateVM(context.Background(), "pve1", CreateVMParams{VMID: 100, Name: "vm1"})
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("err = %v (%T), want *UpstreamError", err, err)
	}
	if upErr.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", upErr.StatusCode)
	}
	if upErr.Errors["vmid"] != "VMID 100 already exists" {
		t.Fatalf("Errors = %v", upErr.Errors)
	}
	if !strings.Contains(err.Error(), "VMID 100 already exists") {
		t.Fatalf("error message %q does not carry the PVE error", err.Error())
	}
}

// TestUpstreamErrorHTTP400 覆盖 HTTP 400-500 + errors 封装。
func TestUpstreamErrorHTTP400(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors": {"memory": "value must be a number"}}`)
	})
	_, err := c.StartVM(context.Background(), "pve1", 100)
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("err = %v (%T), want *UpstreamError", err, err)
	}
	if upErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want 400", upErr.StatusCode)
	}
	if !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("error %q does not carry the status code", err.Error())
	}
}

// TestUpstreamErrorNonJSONBody 覆盖带非 JSON 响应体的 500 响应。
func TestUpstreamErrorNonJSONBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "Internal Server Error")
	})
	_, err := c.ListVMs(context.Background(), "pve1")
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("err = %v (%T), want *UpstreamError", err, err)
	}
	if upErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("StatusCode = %d, want 500", upErr.StatusCode)
	}
	if !strings.Contains(err.Error(), "Internal Server Error") {
		t.Fatalf("error %q does not carry the body", err.Error())
	}
}

// TestCreateVM 验证 POST /nodes/{node}/qemu 的请求形态以及响应的
// UPID 解析。
func TestCreateVM(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/nodes/pve1/qemu" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		if body["vmid"] != float64(100) || body["name"] != "vm1" ||
			body["memory"] != float64(2048) || body["cores"] != float64(2) ||
			body["cpu"] != "x86-64-v2-AES" ||
			body["net0"] != "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0" ||
			body["scsi0"] != "local-lvm:0,import-from=/var/lib/vz/template/iso/debian-12-cloud.qcow2" ||
			body["ide2"] != "local-lvm:cloudinit" ||
			body["bootdisk"] != "scsi0" || body["scsihw"] != "virtio-scsi-single" ||
			body["ciuser"] != "debian" || body["cipassword"] != "s3cret" ||
			body["ipconfig0"] != "ip=10.0.0.5/24,gw=10.0.0.1" ||
			body["nameserver"] != "10.0.0.1" || body["searchdomain"] != "example.test" ||
			body["ostype"] != "l26" {
			t.Errorf("body = %v", body)
		}
		fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmcreate:100:root@pam:"}`)
	})
	upid, err := c.CreateVM(context.Background(), "pve1", CreateVMParams{
		VMID:         100,
		Name:         "vm1",
		Memory:       2048,
		Cores:        2,
		CPU:          "x86-64-v2-AES",
		Scsi0:        "local-lvm:0,import-from=/var/lib/vz/template/iso/debian-12-cloud.qcow2",
		IDE2:         "local-lvm:cloudinit",
		Net0:         "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0",
		BootDisk:     "scsi0",
		ScsiHW:       "virtio-scsi-single",
		CIUser:       "debian",
		CIPassword:   "s3cret",
		IPConfig0:    "ip=10.0.0.5/24,gw=10.0.0.1",
		Nameserver:   "10.0.0.1",
		SearchDomain: "example.test",
		Extra:        map[string]string{"ostype": "l26"},
	})
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if upid != "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmcreate:100:root@pam:" {
		t.Fatalf("upid = %q", upid)
	}
}

// TestListVMs 验证 VM 列表响应的字段映射。
func TestListVMs(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [
			{"vmid": 100, "name": "vm1", "status": "running",
			 "cpu": 0.37, "cpus": 2,
			 "mem": 1073741824, "maxmem": 2147483648,
			 "disk": 536870912, "maxdisk": 10737418240, "uptime": 3600},
			{"vmid": 101, "name": "vm2", "status": "stopped"}
		]}`)
	})
	vms, err := c.ListVMs(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 2 {
		t.Fatalf("len = %d, want 2", len(vms))
	}
	a := vms[0]
	if a.VMID != 100 || a.Name != "vm1" || a.Status != "running" {
		t.Fatalf("vm1 = %+v", a)
	}
	if a.CPU != 0.37 || a.Mem != 1073741824 || a.MaxMem != 2147483648 ||
		a.Disk != 536870912 || a.MaxDisk != 10737418240 || a.Uptime != 3600 {
		t.Fatalf("vm1 metrics = %+v", a)
	}
	if a.Cpus != 2 {
		t.Fatalf("vm1 cpus = %d, want 2 (max usable CPU count)", a.Cpus)
	}
	if b := vms[1]; b.VMID != 101 || b.Status != "stopped" {
		t.Fatalf("vm2 = %+v", b)
	}
}

// TestGetVMConfig 验证配置解析及类型化访问器。
func TestGetVMConfig(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": {
			"cores": "2", "memory": "2048", "cpu": "x86-64-v2-AES",
			"bootdisk": "scsi0", "name": "vm1",
			"scsi0": "local-lvm:vm-100-disk-0,size=10G"
		}}`)
	})
	cfg, err := c.GetVMConfig(context.Background(), "pve1", 100)
	if err != nil {
		t.Fatalf("GetVMConfig: %v", err)
	}
	cores, err := cfg.Cores()
	if err != nil {
		t.Fatalf("Cores: %v", err)
	}
	if cores != 2 {
		t.Fatalf("cores = %d, want 2", cores)
	}
	mem, err := cfg.MemoryMB()
	if err != nil {
		t.Fatalf("MemoryMB: %v", err)
	}
	if mem != 2048 {
		t.Fatalf("memory = %d, want 2048", mem)
	}
	if cfg.CPUType() != "x86-64-v2-AES" || cfg.BootDisk() != "scsi0" {
		t.Fatalf("cpu = %q, bootdisk = %q", cfg.CPUType(), cfg.BootDisk())
	}
	if got := cfg.String("name"); got != "vm1" {
		t.Fatalf("name = %q, want vm1", got)
	}
}

// TestResizeDisk 验证扩容请求使用 PUT（标准 API 方法），携带以 GB 计的
// 绝对大小，并解析 PVE 8/9 返回的 UPID。
func TestResizeDisk(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/nodes/pve1/qemu/100/resize" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		if body["disk"] != "scsi0" || body["size"] != "20G" {
			t.Errorf("body = %v", body)
		}
		fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:resize:100:root@pam:"}`)
	})
	upid, err := c.ResizeDisk(context.Background(), "pve1", 100, "scsi0", 20)
	if err != nil {
		t.Fatalf("ResizeDisk: %v", err)
	}
	if !strings.HasPrefix(upid, "UPID:") {
		t.Fatalf("upid = %q", upid)
	}
}

// TestResizeDiskSynchronousNull 覆盖 PVE 7：它同步应用扩容并回复
// {"data": null} 而不是 UPID。
func TestResizeDiskSynchronousNull(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": null}`)
	})
	upid, err := c.ResizeDisk(context.Background(), "pve1", 100, "scsi0", 20)
	if err != nil {
		t.Fatalf("ResizeDisk: %v", err)
	}
	if upid != "" {
		t.Fatalf("upid = %q, want empty (synchronous completion)", upid)
	}
}

// TestSetVMConfig 验证 PUT /config：PVE 7/8/9 回复 {"data": null}
// （同步），因此成功时返回空任务 ID。
func TestSetVMConfig(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/nodes/pve1/qemu/100/config" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		if body["cores"] != float64(4) || body["memory"] != float64(4096) ||
			body["net0"] != "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0" {
			t.Errorf("body = %v", body)
		}
		fmt.Fprint(w, `{"data": null}`)
	})
	cores := 4
	mem := int64(4096)
	upid, err := c.SetVMConfig(context.Background(), "pve1", 100, VMConfigParams{
		Cores:    &cores,
		MemoryMB: &mem,
		Extra:    map[string]string{"net0": "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0"},
	})
	if err != nil {
		t.Fatalf("SetVMConfig: %v", err)
	}
	if upid != "" {
		t.Fatalf("upid = %q, want empty (synchronous null response)", upid)
	}
}

// TestDestroyVMVerifiesPurgeAndWait 确保 DELETE 以查询参数发送 purge，
// 并且会等待销毁任务结束。
func TestDestroyVMVerifiesPurgeAndWait(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/nodes/pve1/qemu/100":
			if r.Method != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", r.Method)
			}
			if got := r.URL.Query().Get("purge"); got != "1" {
				t.Errorf("purge = %q, want 1", got)
			}
			fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmdestroy:100:root@pam:"}`)
		case "/nodes/pve1/tasks/UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmdestroy:100:root@pam:/status":
			if r.Method != http.MethodGet {
				t.Errorf("task status method = %s", r.Method)
			}
			fmt.Fprint(w, `{"data": {"upid": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmdestroy:100:root@pam:",
				"node": "pve1", "type": "qmdestroy", "id": "100", "user": "root@pam",
				"status": "stopped", "exitstatus": "OK"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	upid, err := c.DestroyVM(context.Background(), "pve1", 100, true)
	if err != nil {
		t.Fatalf("DestroyVM: %v", err)
	}
	if !strings.HasPrefix(upid, "UPID:") {
		t.Fatalf("upid = %q", upid)
	}
	if calls.Load() != 2 {
		t.Fatalf("handler calls = %d, want 2 (delete + task status)", calls.Load())
	}
}

// TestWaitTaskEmptyUPID 在任何轮询开始前拒绝空任务 ID。
func TestWaitTaskEmptyUPID(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request expected for empty upid, got %s %s", r.Method, r.URL.Path)
	})
	_, err := c.WaitTask(context.Background(), "pve1", "", 10*time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "empty upid") {
		t.Fatalf("WaitTask(\"\") err = %v, want empty-upid error", err)
	}
}

// TestWaitTaskSuccess 轮询直至任务停止并返回最终状态。
func TestWaitTaskSuccess(t *testing.T) {
	var polls atomic.Int32
	// 调用方传入 "pve1"，但 UPID 携带的是 pve2；WaitTask 必须轮询
	// UPID 中的节点。
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/nodes/pve2/tasks/") {
			t.Errorf("path = %s, want /nodes/pve2/tasks/... (node from UPID)", r.URL.Path)
		}
		status := `"running"`
		if polls.Load() >= 3 {
			status = `"stopped"`
		}
		fmt.Fprintf(w, `{"data": {"status": %s, "exitstatus": "OK", "upid": "UPID:pve2:0:0:0:qmstart:100:root@pam:", "node": "pve2"}}`, status)
	})
	st, err := c.WaitTask(context.Background(), "pve1", "UPID:pve2:0:0:0:qmstart:100:root@pam:", 10*time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("WaitTask: %v", err)
	}
	if st.Status != "stopped" || st.ExitStatus != "OK" {
		t.Fatalf("final status = %+v", st)
	}
}

// TestWaitTaskFailure 在错误中呈现 PVE 的退出状态。
func TestWaitTaskFailure(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": {"status": "stopped", "exitstatus": "qmstart failed: no such VM", "upid": "UPID:pve1:0:0:0:qmstart:100:root@pam:"}}`)
	})
	_, err := c.WaitTask(context.Background(), "pve1", "UPID:pve1:0:0:0:qmstart:100:root@pam:", 10*time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("WaitTask succeeded, want failure")
	}
	if !strings.Contains(err.Error(), "qmstart failed: no such VM") {
		t.Fatalf("error %q does not carry exit status", err.Error())
	}
}

// TestWaitTaskTimeout 验证超时路径。
func TestWaitTaskTimeout(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": {"status": "running", "upid": "UPID:pve1:0:0:0:qmstart:100:root@pam:"}}`)
	})
	start := time.Now()
	_, err := c.WaitTask(context.Background(), "pve1", "UPID:pve1:0:0:0:qmstart:100:root@pam:", 5*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Fatal("WaitTask succeeded, want timeout")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("error %q does not mention the deadline", err.Error())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("WaitTask took %s, want ~50ms", elapsed)
	}
}

// TestDiskImportString 验证在创建期间导入云镜像的 scsi0 磁盘字符串
// （PVE 7/8/9 没有 REST 形式的 importdisk 端点）。
func TestDiskImportString(t *testing.T) {
	got := DiskImportString("local-lvm", "/var/lib/vz/template/iso/debian-12-cloud.qcow2")
	if want := "local-lvm:0,import-from=/var/lib/vz/template/iso/debian-12-cloud.qcow2"; got != want {
		t.Fatalf("DiskImportString = %q, want %q", got, want)
	}
}

// TestClientOptionErrors 检查失败的 Option 会在首次使用时浮现。
func TestClientOptionErrors(t *testing.T) {
	c := NewClient("pve1", "root@pam", "spark=uuid", WithCAFile("/nonexistent/ca.pem"))
	if c.initErr == nil {
		t.Fatal("NewClient with bad CA file should record an error")
	}
	if _, err := c.ProbeVersion(context.Background()); err == nil {
		t.Fatal("ProbeVersion should fail when an option failed")
	}
	if !strings.Contains(c.initErr.Error(), "WithCAFile") {
		t.Fatalf("initErr = %v", c.initErr)
	}
}
