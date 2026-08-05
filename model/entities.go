package model

import "time"

// Zone is a deployment region grouping nodes, IP pools and VMs.
type Zone struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// PVENode is a registered Proxmox VE node with its API credentials.
type PVENode struct {
	ID             int64     `json:"id"`
	ZoneID         int64     `json:"zone_id"`
	Name           string    `json:"name"`
	Host           string    `json:"host"`
	APIUser        string    `json:"api_user"`
	APITokenSecret string    `json:"api_token_secret"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
}

// IPPool is an IP address pool within a zone.
type IPPool struct {
	ID          int64     `json:"id"`
	ZoneID      int64     `json:"zone_id"`
	Name        string    `json:"name"`
	NetworkCIDR string    `json:"network_cidr"`
	Gateway     string    `json:"gateway"`
	DNS         string    `json:"dns"`
	CreatedAt   time.Time `json:"created_at"`
}

// IPPoolNode is a many-to-many join between IP pools and the nodes that may
// serve addresses from the pool (the "available" whitelist).
type IPPoolNode struct {
	IPPoolID int64 `json:"ip_pool_id"`
	NodeID   int64 `json:"node_id"`
}

// IP status values.
const (
	IPStatusFree = "free"
	IPStatusUsed = "used"
)

// IP is a single address inside an IP pool.
type IP struct {
	ID        int64     `json:"id"`
	PoolID    int64     `json:"pool_id"`
	IP        string    `json:"ip"`
	Status    string    `json:"status"`
	VMID      *int64    `json:"vm_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StorageType abstracts a PVE storage (e.g. local-ssd) behind a display name.
type StorageType struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	PVEStorage  string    `json:"pve_storage"`
	CreatedAt   time.Time `json:"created_at"`
}

// Image is a registered cloud image. NodeImages maps node name to the image's
// storage path (or presence marker) on that node.
type Image struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	DefaultUser string            `json:"default_user"`
	NodeImages  map[string]string `json:"node_images"`
	CreatedAt   time.Time         `json:"created_at"`
}

// VM state constants used when the VM is not (yet) present on the PVE side.
// The live status itself is pass-through (queried from PVE, not stored).
const (
	VMStateCreating = "creating"
	// VMStateFailed marks a VM whose detached provisioning chain failed; the
	// failure message is carried in vms.provision_error.
	VMStateFailed = "failed"
	// VMStateReady is a transitional stand-in for the pass-through status of
	// batch 8: the VM exists on PVE (pve_vmid set, no provision error).
	VMStateReady = "ready"
)

// VM is a virtual machine record; the live status is not stored (pass-through
// queries against PVE, see design D1).
type VM struct {
	ID                int64  `json:"id"`
	UUID              string `json:"uuid"`
	Name              string `json:"name"`
	ZoneID            int64  `json:"zone_id"`
	NodeID            int64  `json:"node_id"`
	PVEVmid           int64  `json:"pve_vmid"`
	ImageID           int64  `json:"image_id"`
	StorageTypeID     int64  `json:"storage_type_id"`
	CPU               int    `json:"cpu"`
	MemMB             int64  `json:"mem_mb"`
	DiskGB            int64  `json:"disk_gb"`
	IPID              *int64 `json:"ip_id,omitempty"`
	PasswordEncrypted string `json:"password_encrypted,omitempty"`
	// ProvisionError carries the sanitized failure message of the detached
	// PVE provisioning chain (empty while provisioning or after success).
	ProvisionError string    `json:"provision_error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
