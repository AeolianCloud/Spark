package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// VMStatus is one entry of GET /nodes/{node}/qemu. Memory and disk values are
// bytes; CPU is the fraction of the configured cores currently in use and
// Cpus is the maximum usable CPU count of the configuration (sockets×cores,
// capped at the host core count). Stopped VMs omit most fields, which decode
// to zero values.
type VMStatus struct {
	VMID    int64   `json:"vmid"`
	Name    string  `json:"name,omitempty"`
	Status  string  `json:"status,omitempty"`
	CPU     float64 `json:"cpu,omitempty"`
	Cpus    int64   `json:"cpus,omitempty"`
	Mem     int64   `json:"mem,omitempty"`
	MaxMem  int64   `json:"maxmem,omitempty"`
	Disk    int64   `json:"disk,omitempty"`
	MaxDisk int64   `json:"maxdisk,omitempty"`
	Uptime  int64   `json:"uptime,omitempty"`
}

// CreateVMParams are the supported parameters of POST /nodes/{node}/qemu.
// Zero-value fields are omitted and left to PVE defaults; Extra carries any
// additional config keys (ostype, ...) verbatim.
//
// CreateVM is a one-step provisioning call: image import, networking and
// cloud-init are all applied in the single qmcreate task. Scsi0 accepts a
// disk string built by DiskImportString (e.g. "local-lvm:0,import-from=...",
// PVE 7.0+) to import a cloud image while the VM is created. IDE2 accepts a
// cloud-init data disk string (e.g. "local-lvm:cloudinit"): without it PVE
// ignores ciuser/cipassword/ipconfig0/nameserver.
type CreateVMParams struct {
	VMID   int64
	Name   string
	Memory int64 // MiB
	Cores  int
	CPU    string // emulated CPU type, e.g. "x86-64-v2-AES"

	// One-step provisioning fields.
	Scsi0        string // disk string, e.g. DiskImportString(storage, source)
	IDE2         string // cloud-init data disk, e.g. "local-lvm:cloudinit"
	Net0         string // e.g. "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0"
	BootDisk     string // boot controller, e.g. "scsi0"
	ScsiHW       string // e.g. "virtio-scsi-single" or "virtio-scsi-pci"
	CIUser       string // cloud-init user, e.g. image default_user
	CIPassword   string // cloud-init password
	IPConfig0    string // static IP, e.g. "ip=10.0.0.5/24,gw=10.0.0.1"
	Nameserver   string
	SearchDomain string

	Extra map[string]string
}

func (p CreateVMParams) body() map[string]any {
	b := map[string]any{"vmid": p.VMID}
	if p.Name != "" {
		b["name"] = p.Name
	}
	if p.Memory > 0 {
		b["memory"] = p.Memory
	}
	if p.Cores > 0 {
		b["cores"] = p.Cores
	}
	if p.CPU != "" {
		b["cpu"] = p.CPU
	}
	if p.Scsi0 != "" {
		b["scsi0"] = p.Scsi0
	}
	if p.IDE2 != "" {
		b["ide2"] = p.IDE2
	}
	if p.Net0 != "" {
		b["net0"] = p.Net0
	}
	if p.BootDisk != "" {
		b["bootdisk"] = p.BootDisk
	}
	if p.ScsiHW != "" {
		b["scsihw"] = p.ScsiHW
	}
	if p.CIUser != "" {
		b["ciuser"] = p.CIUser
	}
	if p.CIPassword != "" {
		b["cipassword"] = p.CIPassword
	}
	if p.IPConfig0 != "" {
		b["ipconfig0"] = p.IPConfig0
	}
	if p.Nameserver != "" {
		b["nameserver"] = p.Nameserver
	}
	if p.SearchDomain != "" {
		b["searchdomain"] = p.SearchDomain
	}
	for k, v := range p.Extra {
		b[k] = v
	}
	return b
}

// CreateVM creates a VM with POST /nodes/{node}/qemu and returns the task ID
// (UPID) of the qmcreate task. The VMID is caller-assigned. One call covers
// VM creation, disk provisioning — including image import via a scsi0 disk
// string built with DiskImportString — the cloud-init data disk (IDE2,
// e.g. "local-lvm:cloudinit"), networking (Net0) and cloud-init injection
// (CIUser/CIPassword/IPConfig0/...); the import time is absorbed by the
// qmcreate task, which callers typically wait on with WaitTask.
func (c *Client) CreateVM(ctx context.Context, node string, params CreateVMParams) (string, error) {
	path := fmt.Sprintf("/nodes/%s/qemu", node)
	raw, err := c.doJSON(ctx, http.MethodPost, path, nil, params.body())
	if err != nil {
		return "", err
	}
	return decodeUPID(raw)
}

// StartVM starts a VM (POST /nodes/{node}/qemu/{vmid}/status/start) and
// returns the task ID.
func (c *Client) StartVM(ctx context.Context, node string, vmid int64) (string, error) {
	raw, err := c.doJSON(ctx, http.MethodPost, vmStatusPath(node, vmid, "start"), nil, nil)
	if err != nil {
		return "", err
	}
	return decodeUPID(raw)
}

// StopVM stops a VM (POST /nodes/{node}/qemu/{vmid}/status/stop) and returns
// the task ID. Force stops abort an in-flight clean shutdown first: PVE's
// QEMU stop endpoint has no literal "force" parameter (that exists only for
// containers), so force maps to the overrule-shutdown flag of PVE 8.2+.
func (c *Client) StopVM(ctx context.Context, node string, vmid int64, force bool) (string, error) {
	var body any
	if force {
		body = map[string]any{"overrule-shutdown": 1}
	}
	raw, err := c.doJSON(ctx, http.MethodPost, vmStatusPath(node, vmid, "stop"), nil, body)
	if err != nil {
		return "", err
	}
	return decodeUPID(raw)
}

// RebootVM reboots a VM (POST /nodes/{node}/qemu/{vmid}/status/reboot) and
// returns the task ID.
func (c *Client) RebootVM(ctx context.Context, node string, vmid int64) (string, error) {
	raw, err := c.doJSON(ctx, http.MethodPost, vmStatusPath(node, vmid, "reboot"), nil, nil)
	if err != nil {
		return "", err
	}
	return decodeUPID(raw)
}

// ListVMs returns all VMs on a node (GET /nodes/{node}/qemu).
func (c *Client) ListVMs(ctx context.Context, node string) ([]VMStatus, error) {
	path := fmt.Sprintf("/nodes/%s/qemu", node)
	raw, err := c.doJSON(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	vms, err := decodeData[[]VMStatus](raw)
	if err != nil {
		return nil, fmt.Errorf("pve: list VMs on %s: %w", node, err)
	}
	return vms, nil
}

// VMConfig is the configuration map of a VM (GET
// /nodes/{node}/qemu/{vmid}/config). PVE returns all values as strings;
// numeric accessors parse them on demand.
type VMConfig map[string]string

// parseConfig converts the config payload into a string map, tolerating
// scalar values that arrive as JSON numbers or booleans.
func parseConfig(raw json.RawMessage) (VMConfig, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("parse VM config: %w", err)
	}
	cfg := make(VMConfig, len(fields))
	for k, v := range fields {
		cfg[k] = string(v)
	}
	return cfg, nil
}

// String returns the value of a config key, unquoted when it is a JSON
// string ("" if absent).
func (c VMConfig) String(key string) string {
	raw, ok := c[key]
	if !ok {
		return ""
	}
	return unquoteJSONString(raw)
}

// Int parses a config key as an integer, unquoting JSON strings if needed.
func (c VMConfig) Int(key string) (int, error) {
	v, err := c.Int64(key)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

// Int64 parses a config key as an int64.
func (c VMConfig) Int64(key string) (int64, error) {
	raw, ok := c[key]
	if !ok {
		return 0, fmt.Errorf("pve: VM config has no %q", key)
	}
	raw = unquoteJSONString(raw)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pve: VM config %q = %q is not an integer", key, c[key])
	}
	return v, nil
}

// unquoteJSONString strips the surrounding quotes of a JSON string value.
func unquoteJSONString(raw string) string {
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1]
	}
	return raw
}

// Cores returns the configured core count.
func (c VMConfig) Cores() (int, error) { return c.Int("cores") }

// MemoryMB returns the configured memory in MiB.
func (c VMConfig) MemoryMB() (int64, error) { return c.Int64("memory") }

// CPUType returns the emulated CPU type.
func (c VMConfig) CPUType() string { return c.String("cpu") }

// BootDisk returns the boot disk controller name (e.g. "scsi0").
func (c VMConfig) BootDisk() string { return c.String("bootdisk") }

// GetVMConfig reads the VM configuration (GET /nodes/{node}/qemu/{vmid}/config).
func (c *Client) GetVMConfig(ctx context.Context, node string, vmid int64) (VMConfig, error) {
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid)
	raw, err := c.doJSON(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	return parseConfig(raw)
}

// VMConfigParams are the supported fields of PUT /nodes/{node}/qemu/{vmid}/config.
// Nil fields are left untouched; Extra carries additional config keys
// (net0, bootdisk, ciuser, ipconfig, ...) verbatim.
type VMConfigParams struct {
	Cores    *int
	MemoryMB *int64
	CPU      *string
	Extra    map[string]string
}

func (p VMConfigParams) body() map[string]any {
	b := make(map[string]any, len(p.Extra)+3)
	if p.Cores != nil {
		b["cores"] = *p.Cores
	}
	if p.MemoryMB != nil {
		b["memory"] = *p.MemoryMB
	}
	if p.CPU != nil {
		b["cpu"] = *p.CPU
	}
	for k, v := range p.Extra {
		b[k] = v
	}
	return b
}

// SetVMConfig updates VM options (PUT /nodes/{node}/qemu/{vmid}/config) and
// returns the task ID. The endpoint is synchronous on PVE 7/8/9: PVE applies
// the change to the running config or the pending changes (depending on the
// VM state) and replies {"data": null} instead of a UPID, so the returned
// task ID is always empty and nothing needs polling.
func (c *Client) SetVMConfig(ctx context.Context, node string, vmid int64, params VMConfigParams) (string, error) {
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid)
	_, err := c.doJSON(ctx, http.MethodPut, path, nil, params.body())
	if err != nil {
		return "", err
	}
	return "", nil
}

// ResizeDisk grows a VM disk (PUT /nodes/{node}/qemu/{vmid}/resize, per the
// standard API the endpoint is PUT, not POST). sizeGB is an absolute target
// size in GiB and must exceed the current size; shrinking is rejected by PVE
// server-side and by the caller's service layer.
//
// The response differs across PVE versions: PVE 7 applies the resize
// synchronously and returns {"data": null} (empty task ID, nothing to poll),
// while PVE 8/9 return a UPID for the asynchronous resize task. Both shapes
// are handled; callers should only wait on a non-empty task ID.
func (c *Client) ResizeDisk(ctx context.Context, node string, vmid int64, disk string, sizeGB int64) (string, error) {
	if disk == "" {
		return "", fmt.Errorf("pve: resize: empty disk name")
	}
	if sizeGB < 0 {
		return "", fmt.Errorf("pve: resize: negative size %dG", sizeGB)
	}
	path := fmt.Sprintf("/nodes/%s/qemu/%d/resize", node, vmid)
	body := map[string]any{"disk": disk, "size": FormatSizeGB(sizeGB)}
	raw, err := c.doJSON(ctx, http.MethodPut, path, nil, body)
	if err != nil {
		return "", err
	}
	if isEmptyData(raw) {
		// PVE 7 synchronous completion: the resize is already applied.
		return "", nil
	}
	return decodeUPID(raw)
}

// DestroyVM deletes a VM (DELETE /nodes/{node}/qemu/{vmid}) and waits for the
// destruction task to finish. purge removes the VMID from backup/replication
// jobs and HA configurations. The returned UPID is the completed task's ID.
func (c *Client) DestroyVM(ctx context.Context, node string, vmid int64, purge bool) (string, error) {
	path := fmt.Sprintf("/nodes/%s/qemu/%d", node, vmid)
	var query url.Values
	if purge {
		query = url.Values{"purge": {"1"}}
	}
	raw, err := c.doJSON(ctx, http.MethodDelete, path, query, nil)
	if err != nil {
		return "", err
	}
	upid, err := decodeUPID(raw)
	if err != nil {
		return "", err
	}
	if _, err := c.WaitTask(ctx, node, upid, DefaultWaitInterval, DefaultWaitTimeout); err != nil {
		return upid, err
	}
	return upid, nil
}

// vmStatusPath builds /nodes/{node}/qemu/{vmid}/status/{action}.
func vmStatusPath(node string, vmid int64, action string) string {
	return fmt.Sprintf("/nodes/%s/qemu/%d/status/%s", node, vmid, action)
}
