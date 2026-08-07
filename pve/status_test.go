package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestNodeStatusPVE9 验证 PVE 9 的 status 完整结构解析：cpuinfo/memory/
// pveversion 对象、rootfs 对象格式、无 PVE 7/8 旧字段（status/node/version/
// mem/maxmem/cpus/maxcpu/maxrootfs 全部缺失，按零值容错）。
func TestNodeStatusPVE9(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/nodes/pve1/status" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data": {
			"cpu": 0.0056,
			"cpuinfo": {"cpus": 4, "cores": 2, "sockets": 1, "model": "Intel Xeon"},
			"memory": {"total": 12442832896, "used": 2228772864, "free": 8963547136, "available": 10214060032},
			"rootfs": {"total": 22538600448, "used": 13589426176, "avail": 7778336768, "percent": 60.3},
			"loadavg": ["0.02", "0.04", "0.00"],
			"kversion": "Linux 6.17.2-1-pve",
			"pveversion": "pve-manager/9.1.1/1",
			"uptime": 6008485
		}}`)
	})
	st, err := c.NodeStatus(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("NodeStatus: %v", err)
	}
	if st.CPU != 0.0056 {
		t.Fatalf("cpu = %v, want 0.0056", st.CPU)
	}
	if st.CPUInfo == nil || st.CPUInfo.Cpus != 4 || st.CPUInfo.Cores != 2 ||
		st.CPUInfo.Sockets != 1 || st.CPUInfo.Model != "Intel Xeon" {
		t.Fatalf("cpuinfo = %+v", st.CPUInfo)
	}
	if st.Memory == nil || st.Memory.Total != 12442832896 || st.Memory.Used != 2228772864 ||
		st.Memory.Free != 8963547136 || st.Memory.Available != 10214060032 {
		t.Fatalf("memory = %+v", st.Memory)
	}
	if st.Rootfs.Total != 22538600448 || st.Rootfs.Used != 13589426176 {
		t.Fatalf("rootfs = %+v", st.Rootfs)
	}
	if st.Uptime != 6008485 || st.PveVersion != "pve-manager/9.1.1/1" ||
		st.KVersion != "Linux 6.17.2-1-pve" {
		t.Fatalf("uptime/version fields = %+v", st)
	}
	if len(st.Loadavg) != 3 || st.Loadavg[0] != "0.02" {
		t.Fatalf("loadavg = %v", st.Loadavg)
	}
	// PVE 9 无旧字段：按零值容错，不报错。
	if st.Status != "" || st.Node != "" || st.Version != "" ||
		st.CPUs != 0 || st.MaxCPU != 0 || st.Mem != 0 || st.MaxMem != 0 || st.MaxRootfs != 0 {
		t.Fatalf("PVE 9 should not have legacy fields, got %+v", st)
	}
}

// TestNodeStatusPVE7 验证 PVE 7 兼容结构解析：status/node/version/cpus/
// maxcpu/mem/maxmem/maxrootfs 旧字段 + rootfs 裸数字格式。
func TestNodeStatusPVE7(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": {
			"node": "pve1", "status": "online",
			"cpu": 0.42, "cpus": 8, "maxcpu": 8,
			"mem": 8589934592, "maxmem": 17179869184,
			"rootfs": 10737418240, "maxrootfs": 21474836480,
			"uptime": 86400, "version": "7.4.19", "kversion": "5.15.152-1-pve",
			"loadavg": ["0.12", "0.08", "0.05"]
		}}`)
	})
	st, err := c.NodeStatus(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("NodeStatus: %v", err)
	}
	if st.Node != "pve1" || st.Status != "online" {
		t.Fatalf("node/status = %q/%q, want pve1/online", st.Node, st.Status)
	}
	if st.CPU != 0.42 || st.CPUs != 8 || st.MaxCPU != 8 {
		t.Fatalf("cpu fields = %+v", st)
	}
	if st.Mem != 8589934592 || st.MaxMem != 17179869184 {
		t.Fatalf("mem fields = %+v", st)
	}
	// rootfs 裸数字 → Used；PVE 9 对象字段缺失（nil）。
	if st.Rootfs.Used != 10737418240 || st.Rootfs.Total != 0 || st.MaxRootfs != 21474836480 {
		t.Fatalf("rootfs = %+v, want used 10737418240/total 0", st.Rootfs)
	}
	if st.CPUInfo != nil || st.Memory != nil {
		t.Fatalf("PVE 7 should not have cpuinfo/memory, got %+v", st)
	}
	if st.Uptime != 86400 || st.Version != "7.4.19" || st.PveVersion != "" || st.KVersion != "5.15.152-1-pve" {
		t.Fatalf("uptime/version fields = %+v", st)
	}
	if len(st.Loadavg) != 3 || st.Loadavg[0] != "0.12" {
		t.Fatalf("loadavg = %v", st.Loadavg)
	}
}

// TestNodeStatusPVE8RootfsObject 验证 PVE 8 的 status 解析（rootfs 对象
// 格式，与 PVE 9 同款 rootfs）。
func TestNodeStatusPVE8RootfsObject(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": {
			"node": "pve1", "status": "online",
			"cpu": 0.42, "cpus": 8, "maxcpu": 8,
			"mem": 8589934592, "maxmem": 17179869184,
			"rootfs": {"total": 21474836480, "used": 10737418240, "avail": 10737418240, "percent": 50},
			"maxrootfs": 21474836480,
			"uptime": 86400, "version": "8.2.4", "kversion": "6.8.12-2-pve",
			"loadavg": ["0.12", "0.08", "0.05"]
		}}`)
	})
	st, err := c.NodeStatus(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("NodeStatus: %v", err)
	}
	if st.Rootfs.Total != 21474836480 || st.Rootfs.Used != 10737418240 || st.MaxRootfs != 21474836480 {
		t.Fatalf("rootfs = %+v", st.Rootfs)
	}
	if st.Version != "8.2.4" {
		t.Fatalf("version = %q, want 8.2.4", st.Version)
	}
}

// TestNodeStatusMissingFields 验证 PVE 响应缺少可选字段（如 loadavg、
// maxcpu）时按零值容错，绝不报错。
func TestNodeStatusMissingFields(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": {"node": "pve1", "status": "online", "cpu": 0.1}}`)
	})
	st, err := c.NodeStatus(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("NodeStatus: %v", err)
	}
	if st.CPUs != 0 || st.MaxCPU != 0 || st.MaxMem != 0 || st.Uptime != 0 {
		t.Fatalf("missing fields should default to zero, got %+v", st)
	}
	if st.CPUInfo != nil || st.Memory != nil {
		t.Fatalf("missing cpuinfo/memory should default to nil, got %+v", st)
	}
	if st.Loadavg != nil {
		t.Fatalf("loadavg = %v, want nil", st.Loadavg)
	}
	if st.Status != "online" {
		t.Fatalf("status = %q", st.Status)
	}
}

// TestNodeNetworkActiveNumeric 验证 active 为数字 1/0（PVE 9 实测格式）
// 时解析为 true/false（PveBool 双格式兼容）。
func TestNodeNetworkActiveNumeric(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nodes/pve1/network" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data": [
			{"iface": "nic0", "active": 1, "type": "eth", "method": "manual", "exists": 1, "families": ["inet"]},
			{"priority": 4, "type": "bridge", "active": 0, "address": "10.0.0.251", "iface": "vmbr0"}
		]}`)
	})
	ifaces, err := c.NodeNetwork(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("NodeNetwork: %v", err)
	}
	if len(ifaces) != 2 {
		t.Fatalf("len = %d, want 2", len(ifaces))
	}
	a := ifaces[0]
	if a.Iface != "nic0" || a.Type != "eth" || a.Active == nil || !bool(*a.Active) {
		t.Fatalf("ifaces[0] = %+v, want active=true", a)
	}
	b := ifaces[1]
	if b.Iface != "vmbr0" || b.Type != "bridge" || b.Address != "10.0.0.251" ||
		b.Active == nil || bool(*b.Active) {
		t.Fatalf("ifaces[1] = %+v, want active=false", b)
	}
}

// TestNodeNetworkActiveBoolean 验证 active 为 true/false（PVE 7/8 格式）
// 时同样解析正确。
func TestNodeNetworkActiveBoolean(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [
			{"iface": "vmbr0", "type": "bridge", "address": "10.0.0.1/24", "active": true},
			{"iface": "lo", "type": "lo", "address": "127.0.0.1/8", "active": false}
		]}`)
	})
	ifaces, err := c.NodeNetwork(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("NodeNetwork: %v", err)
	}
	if len(ifaces) != 2 {
		t.Fatalf("len = %d, want 2", len(ifaces))
	}
	if ifaces[0].Active == nil || !bool(*ifaces[0].Active) {
		t.Fatalf("ifaces[0] = %+v, want active=true", ifaces[0])
	}
	if ifaces[1].Active == nil || bool(*ifaces[1].Active) {
		t.Fatalf("ifaces[1] = %+v, want active=false", ifaces[1])
	}
}

// TestNodeNetworkActiveMissing 验证 active 缺失（null）时指针为 nil，
// 不报错。
func TestNodeNetworkActiveMissing(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [
			{"iface": "eth0", "type": "eth", "address": "10.0.0.2/24"},
			{"iface": "eth1", "type": "eth", "address": "10.0.0.3/24", "active": null}
		]}`)
	})
	ifaces, err := c.NodeNetwork(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("NodeNetwork: %v", err)
	}
	for i, iface := range ifaces {
		if iface.Active != nil {
			t.Fatalf("ifaces[%d].active = %+v, want nil", i, iface.Active)
		}
	}
}

// TestNodeNetIO 验证 rrddata 解析：netin/netout 取数组最后一个元素
// （bytes/s），并校验请求带 timeframe=hour 查询参数。
func TestNodeNetIO(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nodes/pve1/rrddata" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("timeframe"); got != "hour" {
			t.Errorf("timeframe = %q, want hour", got)
		}
		fmt.Fprint(w, `{"data": [
			{"time": 1786107660, "netin": 100.0, "netout": 50.0, "cpu": 0.0056},
			{"time": 1786107720, "netin": 299570.925, "netout": 14786.975, "cpu": 0.0075}
		]}`)
	})
	io, err := c.NodeNetIO(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("NodeNetIO: %v", err)
	}
	// 取最后一个元素的 netin/netout。
	if io.NetIn != 299570.925 || io.NetOut != 14786.975 {
		t.Fatalf("netio = %+v, want last point 299570.925/14786.975", io)
	}
}

// TestNodeNetIOEmpty 验证 rrddata 数组为空时返回全零而不报错（容错）。
func TestNodeNetIOEmpty(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": []}`)
	})
	io, err := c.NodeNetIO(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("NodeNetIO: %v", err)
	}
	if io.NetIn != 0 || io.NetOut != 0 {
		t.Fatalf("netio = %+v, want zero values", io)
	}
}

// TestNodeNetIOMissingFields 验证 rrddata 点缺失 netin/netout 字段时按
// 零值容错不报错（最后一个点缺失则整体为零）。
func TestNodeNetIOMissingFields(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [{"time": 1786107660, "cpu": 0.0056}]}`)
	})
	io, err := c.NodeNetIO(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("NodeNetIO: %v", err)
	}
	if io.NetIn != 0 || io.NetOut != 0 {
		t.Fatalf("netio = %+v, want zero values", io)
	}
}

// TestNodeStatusUpstreamError 验证上游 4xx/5xx 以 *UpstreamError 呈现。
func TestNodeStatusUpstreamError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "401 token invalid", status: http.StatusUnauthorized, body: `{"errors": {"auth": "no valid API token"}}`, wantErr: "no valid API token"},
		{name: "500 server error", status: http.StatusInternalServerError, body: `{"errors": {"root": "boom"}}`, wantErr: "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			_, err := c.NodeStatus(context.Background(), "pve1")
			var upErr *UpstreamError
			if !errors.As(err, &upErr) {
				t.Fatalf("err = %v (%T), want *UpstreamError", err, err)
			}
			if upErr.StatusCode != tc.status {
				t.Fatalf("StatusCode = %d, want %d", upErr.StatusCode, tc.status)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not carry %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestNodeNetworkNetworkError 验证服务器关闭（网络层失败）时返回普通
// error 而非 *UpstreamError。
func TestNodeNetworkNetworkError(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})
	// 手动关闭服务器模拟节点不可达（Cleanup 中的第二次 Close 幂等无害）。
	srv.Close()

	_, err := c.NodeNetwork(context.Background(), "pve1")
	if err == nil {
		t.Fatal("NodeNetwork succeeded, want network error")
	}
	var upErr *UpstreamError
	if errors.As(err, &upErr) {
		t.Fatalf("err = %v, want plain error, not *UpstreamError", err)
	}
}
