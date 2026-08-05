package pve

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultWaitInterval 是 WaitTask 未收到显式间隔时使用的轮询间隔。
const DefaultWaitInterval = 2 * time.Second

// DefaultWaitTimeout 在未给出显式超时时限制 WaitTask 的时长。
const DefaultWaitTimeout = 10 * time.Minute

// UPID 是解析后的 Proxmox VE 任务 ID。规范格式为
// "UPID:node:pid:pstart:starttime:type:id:user"，其中 pid、pstart 和
// starttime 是十六进制：
//
//	UPID:pve1:0000FD0F:01CA7A4A:5FAB1EC4:qmsnapshot:100:root@pam:
type UPID struct {
	Raw       string
	Node      string
	PID       uint64
	PStart    uint64
	StartTime uint64
	Type      string
	ID        string
	User      string
}

// ParseUPID 解析 UPID 字符串。十六进制编码的数字字段会被解码为其
// 整数值。
func ParseUPID(s string) (UPID, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 9 {
		return UPID{}, fmt.Errorf("pve: invalid UPID %q: want 9 colon-separated fields, got %d", s, len(parts))
	}
	if parts[0] != "UPID" {
		return UPID{}, fmt.Errorf("pve: invalid UPID %q: prefix %q is not UPID", s, parts[0])
	}
	u := UPID{Raw: s, Node: parts[1], Type: parts[5], ID: parts[6], User: parts[7]}
	var err error
	if u.PID, err = parseUPIDHex(parts[2]); err != nil {
		return UPID{}, fmt.Errorf("pve: invalid UPID %q: pid: %w", s, err)
	}
	if u.PStart, err = parseUPIDHex(parts[3]); err != nil {
		return UPID{}, fmt.Errorf("pve: invalid UPID %q: pstart: %w", s, err)
	}
	if u.StartTime, err = parseUPIDHex(parts[4]); err != nil {
		return UPID{}, fmt.Errorf("pve: invalid UPID %q: starttime: %w", s, err)
	}
	return u, nil
}

func parseUPIDHex(s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a hex number", s)
	}
	return v, nil
}

// TaskStatus 是 GET /nodes/{node}/tasks/{upid}/status 的负载。Status 取值
// 为 "running" 或 "stopped"；ExitStatus 成功时为 "OK"，任务仍在运行时
// 该字段不出现。
type TaskStatus struct {
	UPID       string `json:"upid"`
	Node       string `json:"node"`
	Type       string `json:"type"`
	ID         string `json:"id"`
	User       string `json:"user"`
	PID        int64  `json:"pid"`
	PStart     int64  `json:"pstart"`
	StartTime  int64  `json:"starttime"`
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus,omitempty"`
}

// GetTaskStatus 读取单个任务状态样本。
func (c *Client) GetTaskStatus(ctx context.Context, node, upid string) (*TaskStatus, error) {
	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", node, upid)
	raw, err := c.doJSON(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	st, err := decodeData[TaskStatus](raw)
	if err != nil {
		return nil, fmt.Errorf("pve: read task status %s: %w", upid, err)
	}
	return &st, nil
}

// WaitTask 轮询任务状态直至其停止或超时。当 UPID 可解析时，要查询的
// 节点取自 UPID 本身（PVE 可能将任务调度到与请求不同的节点上，UPID 携带
// 实际执行节点），否则回退到传入的 node。interval 和 timeout 为 0 时回退
// 到 DefaultWaitInterval/DefaultWaitTimeout。已停止且退出状态非 OK 的任务
// 会返回包含退出状态的错误。
func (c *Client) WaitTask(ctx context.Context, node, upid string, interval, timeout time.Duration) (*TaskStatus, error) {
	if upid == "" {
		return nil, fmt.Errorf("pve: wait task: empty upid")
	}
	if parsed, err := ParseUPID(upid); err == nil && parsed.Node != "" {
		node = parsed.Node
	}
	if interval <= 0 {
		interval = DefaultWaitInterval
	}
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		st, err := c.GetTaskStatus(ctx, node, upid)
		if err != nil {
			return nil, err
		}
		if st.Status != "running" {
			if st.ExitStatus == "" || st.ExitStatus == "OK" {
				return st, nil
			}
			return nil, fmt.Errorf("pve: task %s failed: exitstatus %q", upid, st.ExitStatus)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("pve: wait task %s on %s: %w", upid, node, ctx.Err())
		case <-ticker.C:
		}
	}
}

// DiskImportString 构建在 VM 创建期间导入云镜像的 scsi0 磁盘字符串：
// "<storage>:0,import-from=<source>"。将其传给 CreateVMParams.Scsi0
// （PVE 7.0+ 支持在创建时 import-from），使创建与镜像导入在单个 qmcreate
// 任务中完成。PVE 7/8/9 没有 REST 形式的 importdisk 端点（importdisk 只是
// qm CLI 命令）。
func DiskImportString(storage, source string) string {
	return storage + ":0,import-from=" + source
}
