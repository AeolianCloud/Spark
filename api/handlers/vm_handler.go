package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"spark/model"
	"spark/repository"
	"spark/service"
)

// Additional API error codes for the VM lifecycle domain, following the
// naming style of the base codes in error.go.
const (
	// CodeVMNotReady: the VM has no PVE counterpart yet (provisioning not
	// finished) or its PVE VM is gone; lifecycle operations are refused
	// (409 — the resource exists but is not in a usable state).
	CodeVMNotReady = "vm_not_ready"
	// CodeDiskShrinkNotAllowed: the requested disk size is below the
	// current one (422 — well-formed request, refused by a domain rule).
	CodeDiskShrinkNotAllowed = "disk_shrink_not_allowed"
	// CodeImageNotAvailableInZone: the image cannot be used in the
	// requested zone. 400 (not 422) was chosen because the request
	// parameters are plainly not offerable: the client asked for an
	// image/zone combination that cannot be served, i.e. an invalid
	// parameter set.
	CodeImageNotAvailableInZone = "image_not_available_in_zone"
)

// mapVMServiceError maps service errors onto the unified API error
// contract. The shared and zone/IP-pool kinds are delegated to
// mapServiceErrorExtended; the VM domain kinds are mapped here.
func mapVMServiceError(err error) error {
	var serr *service.Error
	if !errors.As(err, &serr) {
		return mapServiceErrorExtended(err)
	}
	switch serr.Kind {
	case service.KindVMNotReady:
		return NewError(http.StatusConflict, CodeVMNotReady, serr.Message)
	case service.KindDiskShrinkNotAllowed:
		return NewError(http.StatusUnprocessableEntity, CodeDiskShrinkNotAllowed, serr.Message)
	case service.KindImageNotAvailable:
		return NewError(http.StatusBadRequest, CodeImageNotAvailableInZone, serr.Message)
	default:
		return mapServiceErrorExtended(err)
	}
}

// VMHandler serves the /vms routes.
type VMHandler struct {
	svc *service.VMService
}

// NewVMHandler creates a VMHandler backed by svc.
func NewVMHandler(svc *service.VMService) *VMHandler {
	return &VMHandler{svc: svc}
}

// RegisterVMsRoutes mounts the VM lifecycle routes on rg. It is called by
// the router with the /vms group. GET /vms and GET /vms/:id coexist with the
// POST/PATCH/DELETE routes without conflict (gin keeps separate trees per
// HTTP method). The spec-change and destroy operations follow REST method
// semantics: PATCH /vms/:id is the partial spec update, DELETE /vms/:id the
// destroy.
func RegisterVMsRoutes(rg *gin.RouterGroup, svc *service.VMService) {
	h := NewVMHandler(svc)
	rg.POST("", Handler(h.Create))
	rg.GET("", Handler(h.List))
	rg.GET("/:id", Handler(h.Get))
	rg.POST("/:id/start", Handler(h.Start))
	rg.POST("/:id/stop", Handler(h.Stop))
	rg.POST("/:id/restart", Handler(h.Restart))
	rg.PATCH("/:id", Handler(h.Resize))
	rg.DELETE("/:id", Handler(h.Destroy))
}

// vmResponse is the public VM payload. The password is never included;
// provision_error is omitted while empty; pve_vmid is omitted while the VM
// has not been created on PVE yet.
type vmResponse struct {
	ID             int64     `json:"id"`
	UUID           string    `json:"uuid"`
	Name           string    `json:"name"`
	CPU            int       `json:"cpu"`
	MemMB          int64     `json:"mem_mb"`
	DiskGB         int64     `json:"disk_gb"`
	ImageID        int64     `json:"image_id"`
	StorageTypeID  int64     `json:"storage_type_id"`
	ZoneID         int64     `json:"zone_id"`
	NodeID         int64     `json:"node_id"`
	PVEVmid        int64     `json:"pve_vmid,omitempty"`
	IP             string    `json:"ip,omitempty"`
	Status         string    `json:"status"`
	ProvisionError string    `json:"provision_error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// toVMResponse converts a repository VM row into the public payload.
func toVMResponse(vm *repository.VMWithIP, status string) vmResponse {
	return vmResponse{
		ID:             vm.VM.ID,
		UUID:           vm.VM.UUID,
		Name:           vm.VM.Name,
		CPU:            vm.VM.CPU,
		MemMB:          vm.VM.MemMB,
		DiskGB:         vm.VM.DiskGB,
		ImageID:        vm.VM.ImageID,
		StorageTypeID:  vm.VM.StorageTypeID,
		ZoneID:         vm.VM.ZoneID,
		NodeID:         vm.VM.NodeID,
		PVEVmid:        vm.VM.PVEVmid,
		IP:             vm.IP,
		Status:         status,
		ProvisionError: vm.VM.ProvisionError,
		CreatedAt:      vm.VM.CreatedAt,
		UpdatedAt:      vm.VM.UpdatedAt,
	}
}

// localVMStatus derives the transitional status: "creating" while the PVE
// VM does not exist yet, "failed" when the async chain failed, otherwise
// "ready" — a stand-in for the create response only; the list/detail/resize
// responses carry the pass-through status read live from PVE.
func localVMStatus(vm *repository.VMWithIP) string {
	switch {
	case vm.VM.ProvisionError != "":
		return model.VMStateFailed
	case vm.VM.PVEVmid == 0:
		return model.VMStateCreating
	default:
		return model.VMStateReady
	}
}

// vmListItem is the public pass-through VM payload (tasks 8.1/8.2): the 7.x
// vmResponse metadata plus the live runtime portion read from PVE (design
// D1). The spec sizes (cpu/mem_mb/disk_gb) are the local DB values requested
// at create time; the runtime metrics (cpu_usage, mem/maxmem, disk/maxdisk in
// bytes, uptime) come from PVE and are omitted while the VM has no PVE
// counterpart (creating/failed) or is stopped.
type vmListItem struct {
	vmResponse
	CPUUsage float64 `json:"cpu_usage,omitempty"`
	Mem      int64   `json:"mem,omitempty"`
	MaxMem   int64   `json:"maxmem,omitempty"`
	Disk     int64   `json:"disk,omitempty"`
	MaxDisk  int64   `json:"maxdisk,omitempty"`
	Uptime   int64   `json:"uptime,omitempty"`
}

// nodeWarning is one partial-failure notice of GET /vms: a node whose live
// query failed, so its VMs are absent from the list (task 8.3).
type nodeWarning struct {
	Node  string `json:"node"`
	Error string `json:"error"`
}

// toVMListItem converts a merged service item into the public payload.
func toVMListItem(item *service.VMListItem) vmListItem {
	out := vmListItem{vmResponse: toVMResponse(&item.VM, item.Status)}
	if item.Live != nil {
		out.CPUUsage = item.Live.CPUUsage
		out.Mem = item.Live.Mem
		out.MaxMem = item.Live.MaxMem
		out.Disk = item.Live.Disk
		out.MaxDisk = item.Live.MaxDisk
		out.Uptime = item.Live.Uptime
	}
	return out
}

// List handles GET /vms: one PVE call per enabled node merged with the
// local metadata page (8.1). The page is selected by the shared limit/offset
// query parameters and the X-Total-Count header reports the total number of
// local VM rows. Failed nodes are reported in warnings (8.3); warnings is
// always an array (empty when every node answered).
func (h *VMHandler) List(c *gin.Context) error {
	limit, offset, err := parsePagination(c)
	if err != nil {
		return err
	}
	items, warnings, total, err := h.svc.ListVMs(c.Request.Context(), limit, offset)
	if err != nil {
		return mapVMServiceError(err)
	}
	vms := make([]vmListItem, 0, len(items))
	for i := range items {
		vms = append(vms, toVMListItem(&items[i]))
	}
	warns := make([]nodeWarning, 0, len(warnings))
	for _, w := range warnings {
		warns = append(warns, nodeWarning{Node: w.Node, Error: w.Error})
	}
	setTotalCount(c, total)
	c.JSON(http.StatusOK, gin.H{"vms": vms, "warnings": warns})
	return nil
}

// Get handles GET /vms/:id: the local metadata plus the live status
// pass-through (8.2). A node failure answers 503 node_unavailable instead of
// a fake creating status (8.3).
func (h *VMHandler) Get(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	item, err := h.svc.GetVM(c.Request.Context(), id)
	if err != nil {
		return mapVMServiceError(err)
	}
	c.JSON(http.StatusOK, toVMListItem(item))
	return nil
}

// Create handles POST /vms: validates the request, allocates an IP,
// persists the record, triggers the detached provisioning chain and answers
// 201 with the VM — the plaintext IP is included, the password is never
// echoed.
func (h *VMHandler) Create(c *gin.Context) error {
	var req service.CreateVMRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	vm, err := h.svc.CreateVM(c.Request.Context(), req)
	if err != nil {
		return mapVMServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/vms/%d", vm.VM.ID))
	// The chain runs detached and has not finished here, so the status is
	// always "creating" — PVE does not have this VM yet.
	c.JSON(http.StatusCreated, toVMResponse(vm, localVMStatus(vm)))
	return nil
}

// Start handles POST /vms/:id/start. 202 + {accepted: true} was chosen over
// returning the PVE task ID: the operation is dispatched asynchronously and
// the client has no task-polling endpoint — the VM's real state is read
// pass-through (batch 8). The Location header points at the pass-through
// status endpoint GET /vms/:id, where the client can observe the outcome.
func (h *VMHandler) Start(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Start(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/vms/%d", id))
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
	return nil
}

// Stop handles POST /vms/:id/stop (clean ACPI shutdown; see
// VMService.Stop). The Location header points at the pass-through status
// endpoint GET /vms/:id.
func (h *VMHandler) Stop(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Stop(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/vms/%d", id))
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
	return nil
}

// Restart handles POST /vms/:id/restart. The Location header points at the
// pass-through status endpoint GET /vms/:id.
func (h *VMHandler) Restart(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Restart(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/vms/%d", id))
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
	return nil
}

// Destroy handles DELETE /vms/:id. The operation is synchronous (the PVE
// destroy task is waited on) and answers 204 with no body on success. The
// DELETE idempotency semantics live in the service layer and are unchanged:
// a VM row that does not exist yields 404 not_found before any PVE call,
// while a PVE-side 404 (the VM was already removed on the node) is treated
// as "already destroyed" and only the local cleanup runs.
func (h *VMHandler) Destroy(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Destroy(c.Request.Context(), id); err != nil {
		return mapVMServiceError(err)
	}
	c.Status(http.StatusNoContent)
	return nil
}

// resizeRequest is the body of PATCH /vms/:id; every field is optional, at
// least one must be set. Absent fields keep their current values.
type resizeRequest struct {
	CPU    *int   `json:"cpu"`
	MemMB  *int64 `json:"mem_mb"`
	DiskGB *int64 `json:"disk_gb"`
}

// Resize handles PATCH /vms/:id: a partial update of the VM spec. Only the
// fields present in the {cpu?, mem_mb?, disk_gb?} body are applied; a
// missing or null field keeps its current value (PVE first, then the local
// row). It returns the VM with its real, pass-through status: after
// the spec change is applied the live status is re-read from PVE (GetVM),
// so the response reflects the actual VM state rather than a stand-in — the
// same shape as GET /vms/:id.
func (h *VMHandler) Resize(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	var req resizeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	if _, err := h.svc.Resize(c.Request.Context(), id, req.CPU, req.MemMB, req.DiskGB); err != nil {
		return mapVMServiceError(err)
	}
	item, err := h.svc.GetVM(c.Request.Context(), id)
	if err != nil {
		return mapVMServiceError(err)
	}
	c.JSON(http.StatusOK, toVMListItem(item))
	return nil
}
