package pve

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultWaitInterval is the polling interval used when WaitTask receives no
// explicit interval.
const DefaultWaitInterval = 2 * time.Second

// DefaultWaitTimeout bounds WaitTask when no explicit timeout is given.
const DefaultWaitTimeout = 10 * time.Minute

// UPID is a parsed Proxmox VE task ID. The canonical format is
// "UPID:node:pid:pstart:starttime:type:id:user" with pid, pstart and
// starttime in hexadecimal:
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

// ParseUPID parses a UPID string. The hex-encoded numeric fields are decoded
// to their integer values.
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

// TaskStatus is the payload of GET /nodes/{node}/tasks/{upid}/status.
// Status is "running" or "stopped"; ExitStatus is "OK" on success and is
// absent while the task is still running.
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

// GetTaskStatus reads a single task status sample.
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

// WaitTask polls a task's status until it stops or timeout elapses. The node
// to query is taken from the UPID itself when it parses (PVE may schedule a
// task on a different node than the one that was asked, and the UPID carries
// the actual executor), falling back to the passed node. interval and timeout
// of zero fall back to DefaultWaitInterval/DefaultWaitTimeout. A stopped task
// with a non-OK exit status returns an error containing the exit status.
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

// DiskImportString builds the scsi0 disk string that imports a cloud image
// during VM creation: "<storage>:0,import-from=<source>". Pass it to
// CreateVMParams.Scsi0 (PVE 7.0+ supports import-from at create time), so
// create and image import happen in the single qmcreate task. There is no
// REST importdisk endpoint in PVE 7/8/9 (importdisk is only a qm CLI command).
func DiskImportString(storage, source string) string {
	return storage + ":0,import-from=" + source
}
