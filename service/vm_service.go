package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"spark/crypto"
	"spark/model"
	"spark/pve"
	"spark/repository"
)

// Additional service error kinds for the VM lifecycle domain. The values sit
// outside the shared iota range of errors.go (owned by other batches) to
// avoid coupling this file to their edits.
const (
	// KindVMNotReady: the VM has no PVE counterpart yet (provisioning not
	// finished or the PVE VM is gone); lifecycle operations are refused.
	KindVMNotReady ErrorKind = 102
	// KindDiskShrinkNotAllowed: the requested disk size is smaller than the
	// current one.
	KindDiskShrinkNotAllowed ErrorKind = 103
	// KindImageNotAvailable: the image is not present on every enabled node
	// of the requested zone.
	KindImageNotAvailable ErrorKind = 104
)

func vmNotReadyf(format string, args ...any) *Error {
	return &Error{Kind: KindVMNotReady, Message: fmt.Sprintf(format, args...)}
}

func diskShrinkNotAllowedf(format string, args ...any) *Error {
	return &Error{Kind: KindDiskShrinkNotAllowed, Message: fmt.Sprintf(format, args...)}
}

func imageNotAvailablef(format string, args ...any) *Error {
	return &Error{Kind: KindImageNotAvailable, Message: fmt.Sprintf(format, args...)}
}

const (
	// vmClaimRetries bounds the conditional-IP-claim retry loop inside the
	// create transaction (repository.ErrAllocationRetry).
	vmClaimRetries = 5
	// vmProvisionTimeout bounds the whole detached provisioning chain:
	// NextVMID + create + WaitTask (default 10m) + resize + config read.
	vmProvisionTimeout = 12 * time.Minute
	// maxProvisionErrorLen caps the provision_error value stored in vms so a
	// verbose PVE dump cannot bloat the row.
	maxProvisionErrorLen = 1000
	// vmNamePattern is the PVE qm name rule: it must match
	// ^[A-Za-z0-9_][A-Za-z0-9_.\-]*$ (a letter, digit or underscore first,
	// then letters, digits, underscores, dots and dashes).
	vmNamePattern = `^[A-Za-z0-9_][A-Za-z0-9_.\-]*$`
)

var vmNameRegex = regexp.MustCompile(vmNamePattern)

// TxBeginner begins a database transaction; *pgxpool.Pool satisfies it. The
// VM service keeps the IP-allocation transaction orchestration in the
// service layer (per the migration 0002 header conventions), so it needs a
// transaction entry point in addition to the repositories.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// VMRepository is the vms data access the VMService depends on.
type VMRepository interface {
	CreateVMTx(ctx context.Context, tx pgx.Tx, vm model.VM) (*model.VM, error)
	GetVM(ctx context.Context, id int64) (*repository.VMWithIP, error)
	ListVMs(ctx context.Context) ([]repository.VMWithIP, error)
	SetVMIPIDTx(ctx context.Context, tx pgx.Tx, id, ipID int64) error
	UpdateVMPVEVMID(ctx context.Context, id, vmid, diskGB int64) error
	SetProvisionError(ctx context.Context, id int64, message string) error
	UpdateSpec(ctx context.Context, id int64, newCPU int, newMemMB, newDiskGB int64, oldCPU int, oldMemMB, oldDiskGB int64) error
	DeleteVMTx(ctx context.Context, tx pgx.Tx, id int64) error
}

// VMZoneRepository is the zone data access the VMService depends on.
type VMZoneRepository interface {
	GetZone(ctx context.Context, id int64) (*model.Zone, error)
	ListZones(ctx context.Context) ([]model.Zone, error)
}

// VMNodeRepository is the node data access the VMService depends on.
type VMNodeRepository interface {
	GetNode(ctx context.Context, id int64) (*model.PVENode, error)
	ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error)
}

// VMIPPoolRepository is the IP pool data access the VMService depends on.
type VMIPPoolRepository interface {
	ListPoolsByZone(ctx context.Context, zoneID int64) ([]model.IPPool, error)
	GetPoolNodes(ctx context.Context, poolID int64) ([]model.PVENode, error)
	ClaimFreeIP(ctx context.Context, tx pgx.Tx, poolID int64, vmID *int64) (model.IP, error)
	ReleaseIPByVMTx(ctx context.Context, tx pgx.Tx, vmID int64) error
}

// VMImageRepository is the image data access the VMService depends on.
type VMImageRepository interface {
	Get(ctx context.Context, id int64) (*model.Image, error)
	EnabledNodeNamesByZone(ctx context.Context, zoneID int64) ([]string, error)
}

// VMStorageTypeRepository is the storage type data access the VMService
// depends on.
type VMStorageTypeRepository interface {
	Get(ctx context.Context, id int64) (*model.StorageType, error)
}

// CreateVMRequest is the validated input of VMService.CreateVM; the field
// names match the D6 API shape exactly (POST /vms body).
type CreateVMRequest struct {
	Name          string `json:"name"`
	CPU           int    `json:"cpu"`
	MemMB         int64  `json:"mem_mb"`
	DiskGB        int64  `json:"disk_gb"`
	ImageID       int64  `json:"image_id"`
	StorageTypeID int64  `json:"storage_type_id"`
	ZoneID        int64  `json:"zone_id"`
	Password      string `json:"password"`
}

// validateCreateVMRequest enforces the non-existence parts of the create
// validation: the name and the positive specs, and a non-empty password. The
// existence checks (zone, image, storage type, image-on-zone) run before
// this in CreateVM, matching the documented validation order.
func validateCreateVMRequest(req CreateVMRequest) error {
	switch {
	case strings.TrimSpace(req.Name) == "":
		return badRequestf("vm name is required")
	case !vmNameRegex.MatchString(req.Name):
		return badRequestf("vm name must match %s", vmNamePattern)
	case req.Password == "":
		return badRequestf("password is required")
	case req.CPU <= 0:
		return badRequestf("cpu must be > 0")
	case req.MemMB <= 0:
		return badRequestf("mem_mb must be > 0")
	case req.DiskGB <= 0:
		return badRequestf("disk_gb must be > 0")
	}
	return nil
}

// VMService implements the business rules of the VM lifecycle: create (with
// atomic IP allocation and a detached PVE provisioning chain), start/stop/
// restart, destroy (with IP release) and spec changes.
type VMService struct {
	beginner    TxBeginner
	vmRepo      VMRepository
	ipPoolRepo  VMIPPoolRepository
	zoneRepo    VMZoneRepository
	nodeRepo    VMNodeRepository
	imageRepo   VMImageRepository
	storageRepo VMStorageTypeRepository
	cipher      *crypto.Cipher
	// newClient builds the PVE client for a node; injectable so tests can
	// point the provisioning chain and lifecycle calls at fake servers.
	newClient func(host, apiUser, apiTokenSecret string) *pve.Client
	// selectNode picks the deployment node among the pool candidates;
	// injectable for tests, the production default probes reachability with
	// the same newClient factory the service uses for every other node
	// interaction (so SetClientFactory redirects the probes too).
	selectNode func(ctx context.Context, nodes []model.PVENode) (model.PVENode, error)
}

// NewVMService creates a VMService backed by the given repositories and the
// encryption cipher (used to encrypt the cloud-init password before it is
// stored).
func NewVMService(beginner TxBeginner, vmRepo VMRepository, ipPoolRepo VMIPPoolRepository,
	zoneRepo VMZoneRepository, nodeRepo VMNodeRepository, imageRepo VMImageRepository,
	storageRepo VMStorageTypeRepository, cipher *crypto.Cipher) *VMService {
	s := &VMService{
		beginner:    beginner,
		vmRepo:      vmRepo,
		ipPoolRepo:  ipPoolRepo,
		zoneRepo:    zoneRepo,
		nodeRepo:    nodeRepo,
		imageRepo:   imageRepo,
		storageRepo: storageRepo,
		cipher:      cipher,
		newClient: func(host, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient(host, apiUser, apiTokenSecret)
		},
	}
	// The reachability probes must use the same client factory as every
	// other node interaction, so overriding newClient (SetClientFactory,
	// tests, reverse proxies) redirects the probes as well. It is assigned
	// after construction because it closes over s itself.
	s.selectNode = func(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
		return selectReachableNode(ctx, nodes, s.newClient)
	}
	return s
}

// SetClientFactory replaces the PVE client factory used for every node
// interaction (provisioning chain, lifecycle operations, pass-through
// queries, reachability probes). The default factory builds clients against
// https://{host}:8006/api2/json; overriding it lets callers point the
// service at a different base URL (tests, reverse proxies).
func (s *VMService) SetClientFactory(fn func(host, apiUser, apiTokenSecret string) *pve.Client) {
	if fn != nil {
		s.newClient = fn
	}
}

// CreateVM validates the request, picks a reachable node (D4), atomically
// allocates an IP and persists the VM record (D3 + migration 0002
// conventions), then launches the detached provisioning chain (D5) and
// returns the VM with its plaintext IP. The returned record has
// "creating" semantics: pve_vmid stays zero until the chain succeeds.
//
// The provisioning goroutine must not borrow the caller's context (it is
// cancelled when the HTTP handler returns), so it runs under a detached
// background context bounded by vmProvisionTimeout.
func (s *VMService) CreateVM(ctx context.Context, req CreateVMRequest) (*repository.VMWithIP, error) {
	// 1. zone existence.
	if req.ZoneID <= 0 {
		return nil, badRequestf("zone_id must be a positive integer")
	}
	if _, err := s.zoneRepo.GetZone(ctx, req.ZoneID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("zone %d not found", req.ZoneID)
		}
		return nil, fmt.Errorf("create vm: check zone: %w", err)
	}
	// 2. image existence.
	if req.ImageID <= 0 {
		return nil, badRequestf("image_id must be a positive integer")
	}
	image, err := s.imageRepo.Get(ctx, req.ImageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("image %d not found", req.ImageID)
		}
		return nil, fmt.Errorf("create vm: get image: %w", err)
	}
	// 3. storage type existence.
	if req.StorageTypeID <= 0 {
		return nil, badRequestf("storage_type_id must be a positive integer")
	}
	storageType, err := s.storageRepo.Get(ctx, req.StorageTypeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("storage type %d not found", req.StorageTypeID)
		}
		return nil, fmt.Errorf("create vm: get storage type: %w", err)
	}
	// 4. image availability on every enabled node of the zone (reuses the
	// 6.3 intersection semantics: the node_images map must have a key for
	// each enabled node).
	nodeNames, err := s.imageRepo.EnabledNodeNamesByZone(ctx, req.ZoneID)
	if err != nil {
		return nil, fmt.Errorf("create vm: enabled nodes by zone: %w", err)
	}
	if len(filterImagesAvailableByNodes([]model.Image{*image}, nodeNames)) == 0 {
		return nil, imageNotAvailablef("image %d is not available on every enabled node of zone %d", req.ImageID, req.ZoneID)
	}
	// 5. password and specs.
	if err := validateCreateVMRequest(req); err != nil {
		return nil, err
	}

	// Node and pool selection (D4): the zone's pools in id order; for each
	// pool its whitelisted nodes intersect the zone's enabled nodes and the
	// first reachable node wins; an unreachable pool is skipped in favor of
	// the next one.
	pool, node, err := s.selectPoolAndNode(ctx, req.ZoneID)
	if err != nil {
		return nil, err
	}

	// The cloud-init password is stored encrypted (crypto.Cipher) and is
	// never persisted, logged or echoed in plain text.
	passwordEncrypted, err := s.cipher.Encrypt(req.Password)
	if err != nil {
		return nil, fmt.Errorf("create vm: encrypt password: %w", err)
	}

	// Atomic placement (D3 + migration 0002 conventions): one transaction
	// runs INSERT vms (ip_id NULL) -> claim ip -> UPDATE vms.ip_id; any
	// failure rolls back the vms row together with the claim.
	tx, err := s.beginner.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create vm: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, err := s.vmRepo.CreateVMTx(ctx, tx, model.VM{
		UUID:              uuid.NewString(),
		Name:              req.Name,
		ZoneID:            req.ZoneID,
		NodeID:            node.ID,
		ImageID:           req.ImageID,
		StorageTypeID:     req.StorageTypeID,
		CPU:               req.CPU,
		MemMB:             req.MemMB,
		DiskGB:            req.DiskGB,
		PasswordEncrypted: passwordEncrypted,
	})
	if err != nil {
		return nil, fmt.Errorf("create vm: insert: %w", err)
	}

	var claimed model.IP
	for attempt := 0; attempt < vmClaimRetries; attempt++ {
		ip, err := s.ipPoolRepo.ClaimFreeIP(ctx, tx, pool.ID, &created.ID)
		if err == nil {
			claimed = ip
			break
		}
		if errors.Is(err, repository.ErrAllocationRetry) {
			continue // pick another random candidate inside the same tx
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ipExhaustedf("pool %d has no free ip", pool.ID)
		}
		return nil, fmt.Errorf("create vm: claim ip in pool %d: %w", pool.ID, err)
	}
	if claimed.ID == 0 {
		return nil, ipExhaustedf("pool %d has no free ip after %d attempts", pool.ID, vmClaimRetries)
	}

	if err := s.vmRepo.SetVMIPIDTx(ctx, tx, created.ID, claimed.ID); err != nil {
		return nil, fmt.Errorf("create vm: link ip: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("create vm: commit: %w", err)
	}

	ipID := claimed.ID
	created.IPID = &ipID

	// Detached provisioning chain (D5): the request has already succeeded,
	// so chain failures are recorded into vms.provision_error instead of
	// being returned. context.Background() (NOT the request ctx, which is
	// cancelled when the handler returns), bounded by vmProvisionTimeout.
	vm := *created
	go s.provisionVM(vm, node, image, storageType, pool, req.Password, claimed.IP)

	return &repository.VMWithIP{VM: vm, IP: claimed.IP}, nil
}

// selectPoolAndNode walks the zone's IP pools in id order (D4). For each
// pool the whitelisted nodes (ip_pool_nodes, by node id) are intersected
// with the zone's enabled nodes and the first reachable node is picked; a
// pool without reachable candidates is skipped in favor of the next pool.
// When no pool yields a reachable node a KindNodeUnavailable error is
// returned (this also covers a zone without any pool: the candidate set is
// empty by construction).
func (s *VMService) selectPoolAndNode(ctx context.Context, zoneID int64) (model.IPPool, model.PVENode, error) {
	enabledNodes, err := s.nodeRepo.ListEnabledNodesByZone(ctx, zoneID)
	if err != nil {
		return model.IPPool{}, model.PVENode{}, fmt.Errorf("select node: list enabled nodes: %w", err)
	}
	pools, err := s.ipPoolRepo.ListPoolsByZone(ctx, zoneID)
	if err != nil {
		return model.IPPool{}, model.PVENode{}, fmt.Errorf("select node: list pools: %w", err)
	}
	for _, pool := range pools {
		poolNodes, err := s.ipPoolRepo.GetPoolNodes(ctx, pool.ID)
		if err != nil {
			return model.IPPool{}, model.PVENode{}, fmt.Errorf("select node: pool %d nodes: %w", pool.ID, err)
		}
		candidates := poolCandidates(poolNodes, enabledNodes)
		if len(candidates) == 0 {
			continue
		}
		node, err := s.selectNode(ctx, candidates)
		if err == nil {
			return pool, node, nil
		}
		// KindNodeUnavailable: keep the last error and try the next pool.
	}
	return model.IPPool{}, model.PVENode{}, nodeUnavailablef("no reachable node for zone %d", zoneID)
}

// poolCandidates intersects the pool's whitelisted nodes with the zone's
// enabled nodes. The result follows the node id order returned by
// GetPoolNodes; v1 accepts this node-id order instead of the pool's check
// order (the check order itself is not persisted, no schema change).
func poolCandidates(poolNodes, enabledNodes []model.PVENode) []model.PVENode {
	enabled := make(map[int64]struct{}, len(enabledNodes))
	for _, n := range enabledNodes {
		enabled[n.ID] = struct{}{}
	}
	candidates := make([]model.PVENode, 0, len(poolNodes))
	for _, n := range poolNodes {
		if _, ok := enabled[n.ID]; ok {
			candidates = append(candidates, n)
		}
	}
	return candidates
}

// provisionVM runs the detached PVE provisioning chain (D5) and records
// failures into vms.provision_error. It is invoked as a goroutine with a
// detached background context; the returned error is only for logging.
//
// The goroutine must never take the process down: a panic anywhere in the
// chain is recovered here and recorded as an internal provisioning error,
// leaving the VM row inspectable (provision_error set, pve_vmid still zero).
func (s *VMService) provisionVM(vm model.VM, node model.PVENode, image *model.Image,
	storageType *model.StorageType, pool model.IPPool, plainPassword, ipAddr string) {
	ctx, cancel := context.WithTimeout(context.Background(), vmProvisionTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			msg := sanitizeProvisionError(fmt.Errorf("internal panic during provisioning: %v", r), plainPassword)
			if uerr := s.vmRepo.SetProvisionError(ctx, vm.ID, msg); uerr != nil {
				slog.Error("could not persist provision_error", "vm_id", vm.ID, "error", uerr)
			}
			slog.Error("vm provisioning panicked",
				"vm_id", vm.ID,
				"node", node.Name,
				"error", msg,
			)
		}
	}()
	if err := s.provision(ctx, vm, node, image, storageType, pool, plainPassword, ipAddr); err != nil {
		slog.Error("vm provisioning failed",
			"vm_id", vm.ID,
			"node", node.Name,
			"error", err,
		)
	}
}

// provision executes the single-step create chain (design D5): NextVMID,
// then one CreateVM call carrying the scsi0 import-from disk, the
// cloud-init data disk (ide2), vmbr0 networking and the cloud-init
// injection (ciuser/cipassword/ipconfig0/nameserver); then WaitTask for the
// qmcreate task; then a disk resize to the requested size when the imported
// image is smaller; and finally the pve_vmid/disk_gb metadata update. Every
// failure is persisted via SetProvisionError with a sanitized message (the
// plaintext cloud-init password never lands in the DB or the logs).
func (s *VMService) provision(ctx context.Context, vm model.VM, node model.PVENode,
	image *model.Image, storageType *model.StorageType, pool model.IPPool,
	plainPassword, ipAddr string) error {
	client := s.newClient(node.Host, node.APIUser, node.APITokenSecret)

	vmid, err := client.NextVMID(ctx)
	if err != nil {
		return s.failProvision(ctx, vm.ID, 0, "next vmid", err, plainPassword)
	}

	imagePath := image.NodeImages[node.Name]
	if imagePath == "" {
		return s.failProvision(ctx, vm.ID, 0, "image path",
			fmt.Errorf("image %q has no storage path for node %q", image.Name, node.Name), plainPassword)
	}

	prefix, err := netip.ParsePrefix(pool.NetworkCIDR)
	if err != nil {
		return s.failProvision(ctx, vm.ID, 0, "pool prefix",
			fmt.Errorf("pool %d has invalid network_cidr %q: %v", pool.ID, pool.NetworkCIDR, err), plainPassword)
	}

	upid, err := client.CreateVM(ctx, node.Name, pve.CreateVMParams{
		VMID:       int64(vmid),
		Name:       vm.Name,
		Memory:     vm.MemMB,
		Cores:      vm.CPU,
		Scsi0:      pve.DiskImportString(storageType.PVEStorage, imagePath),
		IDE2:       storageType.PVEStorage + ":cloudinit",
		Net0:       "virtio,bridge=vmbr0",
		BootDisk:   "scsi0",
		ScsiHW:     "virtio-scsi-pci",
		CIUser:     image.DefaultUser,
		CIPassword: plainPassword,
		IPConfig0:  fmt.Sprintf("ip=%s/%d,gw=%s", ipAddr, prefix.Bits(), pool.Gateway),
		Nameserver: pool.DNS,
	})
	if err != nil {
		return s.failProvision(ctx, vm.ID, int64(vmid), "create", err, plainPassword)
	}

	if _, err := client.WaitTask(ctx, node.Name, upid, 0, 0); err != nil {
		return s.failProvision(ctx, vm.ID, int64(vmid), "wait create", err, plainPassword)
	}

	// The imported image may be smaller than the requested size; the disk is
	// grown to disk_gb in that case. When the image is at least as big as
	// requested, the actual size is what gets persisted.
	diskGB := vm.DiskGB
	cfg, err := client.GetVMConfig(ctx, node.Name, int64(vmid))
	if err != nil {
		return s.failProvision(ctx, vm.ID, int64(vmid), "read config", err, plainPassword)
	}
	boot := cfg.BootDisk()
	if boot == "" {
		boot = "scsi0"
	}
	if actual, perr := parseDiskSizeGB(cfg.String(boot)); perr == nil {
		if vm.DiskGB > actual {
			upid, err := client.ResizeDisk(ctx, node.Name, int64(vmid), boot, vm.DiskGB)
			if err != nil {
				return s.failProvision(ctx, vm.ID, int64(vmid), "resize disk", err, plainPassword)
			}
			if upid != "" {
				if _, err := client.WaitTask(ctx, node.Name, upid, 0, 0); err != nil {
					return s.failProvision(ctx, vm.ID, int64(vmid), "wait resize", err, plainPassword)
				}
			}
			diskGB = vm.DiskGB
		} else {
			diskGB = actual
		}
	}

	if err := s.vmRepo.UpdateVMPVEVMID(ctx, vm.ID, int64(vmid), diskGB); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The VM row was destroyed while provisioning; the PVE VM is
			// orphaned on the node and needs manual cleanup.
			return fmt.Errorf("provision vm %d: row deleted during provisioning (orphaned pve vmid %d)", vm.ID, vmid)
		}
		return fmt.Errorf("provision vm %d: persist vmid: %w", vm.ID, err)
	}
	return nil
}

// parseDiskSizeGB converts the size field of a PVE disk string
// ("local-lvm:vm-100-disk-0,size=10G") to whole GiB.
func parseDiskSizeGB(diskString string) (int64, error) {
	size := ""
	for _, part := range strings.Split(diskString, ",") {
		if v, ok := strings.CutPrefix(part, "size="); ok {
			size = v
			break
		}
	}
	if size == "" {
		return 0, fmt.Errorf("no size in disk string %q", diskString)
	}
	bytes, err := pve.ParseSize(size)
	if err != nil {
		return 0, err
	}
	gb := bytes / (1 << 30)
	if gb == 0 && bytes > 0 {
		gb = 1
	}
	return gb, nil
}

// failProvision persists the provisioning failure in vms.provision_error
// with a sanitized message and returns a sanitized error for logging. The IP
// stays allocated on failure by design (design doc Risks: no automatic
// release, an operator reclaims dirty addresses manually).
//
// vmid is the PVE VMID allocated by NextVMID (0 when the chain failed before
// the VMID was known). Once the VMID exists, the message embeds it (and the
// "create succeeded" marker for post-create steps) so operators can locate
// and clean up half-created VMs on the node.
//
// The step prefix is assembled first and the whole message is sanitized
// afterwards (redact then truncate), so a verbose PVE error can never push
// the stored value past maxProvisionErrorLen and the prefix itself is never
// truncated away — the same length rule as the recover branch of provisionVM.
func (s *VMService) failProvision(ctx context.Context, vmID, vmid int64, step string, err error, plainPassword string) error {
	var msg string
	switch {
	case vmid == 0:
		msg = fmt.Sprintf("%s: %s", step, err)
	case step == "create":
		msg = fmt.Sprintf("create (vmid=%d) failed: %s", vmid, err)
	default:
		msg = fmt.Sprintf("create succeeded (vmid=%d) but %s failed: %s", vmid, step, err)
	}
	msg = sanitizeProvisionError(errors.New(msg), plainPassword)
	if uerr := s.vmRepo.SetProvisionError(ctx, vmID, msg); uerr != nil {
		slog.Error("could not persist provision_error", "vm_id", vmID, "error", uerr)
	}
	return fmt.Errorf("provision vm %d: %s", vmID, msg)
}

// sanitizeProvisionError redacts every occurrence of the given secrets (the
// cloud-init password) from the error message and bounds its length, so
// vms.provision_error and the logs never carry the password or an unbounded
// PVE dump. Redaction runs before truncation so a secret spanning the length
// boundary is never half-stored; the truncation cuts on a rune boundary so
// multi-byte UTF-8 characters are never split into invalid sequences (the
// vms.provision_error column would reject them, Postgres 22021).
func sanitizeProvisionError(err error, secrets ...string) string {
	msg := err.Error()
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, "[redacted]")
	}
	r := []rune(msg)
	if len(r) > maxProvisionErrorLen {
		msg = string(r[:maxProvisionErrorLen])
	}
	return msg
}

// vmAndNode loads the VM (mapping a missing row to not_found) and its node.
// A VM whose pve_vmid is still zero has not been provisioned yet and yields
// KindVMNotReady.
func (s *VMService) vmAndNode(ctx context.Context, id int64) (*repository.VMWithIP, *model.PVENode, error) {
	vm, err := s.vmRepo.GetVM(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, notFoundf("vm %d not found", id)
		}
		return nil, nil, fmt.Errorf("get vm %d: %w", id, err)
	}
	if vm.VM.PVEVmid == 0 {
		return nil, nil, vmNotReadyf("vm %d is not provisioned yet", id)
	}
	node, err := s.nodeRepo.GetNode(ctx, vm.VM.NodeID)
	if err != nil {
		return nil, nil, fmt.Errorf("get node %d of vm %d: %w", vm.VM.NodeID, id, err)
	}
	return vm, node, nil
}

// mapPVEOpError converts a lifecycle-operation failure into a service
// error: a PVE 404 means the pve_vmid refers to nothing on the node anymore
// (the VM was removed outside the service), which is surfaced as
// vm_not_ready; every other failure stays a plain error (rendered as a
// generic 500 by the handler).
func mapPVEOpError(err error, op string, id int64) error {
	var upErr *pve.UpstreamError
	if errors.As(err, &upErr) && upErr.StatusCode == http.StatusNotFound {
		return vmNotReadyf("vm %d does not exist on the pve node (cannot %s)", id, op)
	}
	return fmt.Errorf("%s vm %d: %w", op, id, err)
}

// Start boots the VM (POST /nodes/{node}/qemu/{vmid}/status/start). The PVE
// task ID is not exposed: the client has nothing to poll it with and the
// VM's real state is read pass-through anyway (batch 8).
func (s *VMService) Start(ctx context.Context, id int64) error {
	vm, node, err := s.vmAndNode(ctx, id)
	if err != nil {
		return err
	}
	client := s.newClient(node.Host, node.APIUser, node.APITokenSecret)
	if _, err := client.StartVM(ctx, node.Name, vm.VM.PVEVmid); err != nil {
		return mapPVEOpError(err, "start", id)
	}
	return nil
}

// Stop shuts the VM down (POST .../status/stop). force=false performs a
// clean ACPI shutdown; forced PVE-side stops are left to operators.
func (s *VMService) Stop(ctx context.Context, id int64) error {
	vm, node, err := s.vmAndNode(ctx, id)
	if err != nil {
		return err
	}
	client := s.newClient(node.Host, node.APIUser, node.APITokenSecret)
	if _, err := client.StopVM(ctx, node.Name, vm.VM.PVEVmid, false); err != nil {
		return mapPVEOpError(err, "stop", id)
	}
	return nil
}

// Restart reboots the VM (POST .../status/reboot).
func (s *VMService) Restart(ctx context.Context, id int64) error {
	vm, node, err := s.vmAndNode(ctx, id)
	if err != nil {
		return err
	}
	client := s.newClient(node.Host, node.APIUser, node.APITokenSecret)
	if _, err := client.RebootVM(ctx, node.Name, vm.VM.PVEVmid); err != nil {
		return mapPVEOpError(err, "restart", id)
	}
	return nil
}

// Destroy removes the VM: first the PVE VM is destroyed (purge=true, the
// task is waited on inside DestroyVM); only on success, one transaction
// releases the claimed IP and deletes the vms row (migration 0002
// conventions: release BEFORE delete). Any PVE failure aborts the destroy
// and keeps both the DB record and the IP, so the operator can inspect or
// retry — except a PVE 404 (the VM was already removed on the PVE side, e.g.
// by an operator), which is treated as "already destroyed" and continues
// with the local cleanup. A VM that never reached PVE (pve_vmid == 0) skips
// the PVE call and only cleans up the local record.
func (s *VMService) Destroy(ctx context.Context, id int64) error {
	vm, err := s.vmRepo.GetVM(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return notFoundf("vm %d not found", id)
		}
		return fmt.Errorf("destroy vm %d: get: %w", id, err)
	}
	if vm.VM.PVEVmid > 0 {
		node, err := s.nodeRepo.GetNode(ctx, vm.VM.NodeID)
		if err != nil {
			return fmt.Errorf("destroy vm %d: get node: %w", id, err)
		}
		client := s.newClient(node.Host, node.APIUser, node.APITokenSecret)
		if _, err := client.DestroyVM(ctx, node.Name, vm.VM.PVEVmid, true); err != nil {
			var upErr *pve.UpstreamError
			if errors.As(err, &upErr) && upErr.StatusCode == http.StatusNotFound {
				// The PVE VM is already gone (removed outside the service);
				// the local cleanup below still runs.
			} else {
				return fmt.Errorf("destroy vm %d on pve: %w (vm record and ip kept)", id, err)
			}
		}
	}

	tx, err := s.beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("destroy vm %d: begin tx: %w", id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.ipPoolRepo.ReleaseIPByVMTx(ctx, tx, id); err != nil {
		return fmt.Errorf("destroy vm %d: release ip: %w", id, err)
	}
	if err := s.vmRepo.DeleteVMTx(ctx, tx, id); err != nil {
		return fmt.Errorf("destroy vm %d: delete row: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("destroy vm %d: commit: %w", id, err)
	}
	return nil
}

// validateResizeSpec validates a resize request against the VM's current
// values: at least one field must be present and positive; the disk may only
// grow — a smaller disk_gb is refused with KindDiskShrinkNotAllowed. An
// equal disk_gb is allowed and treated as a no-op (no resize call is made
// for it), which keeps resize idempotent for callers that always send the
// full spec. cpu and mem may grow or shrink.
func validateResizeSpec(cpu *int, memMB, diskGB *int64, current model.VM) error {
	if cpu == nil && memMB == nil && diskGB == nil {
		return badRequestf("at least one of cpu, mem_mb, disk_gb is required")
	}
	if cpu != nil && *cpu <= 0 {
		return badRequestf("cpu must be > 0")
	}
	if memMB != nil && *memMB <= 0 {
		return badRequestf("mem_mb must be > 0")
	}
	if diskGB != nil && *diskGB <= 0 {
		return badRequestf("disk_gb must be > 0")
	}
	if diskGB != nil && *diskGB < current.DiskGB {
		return diskShrinkNotAllowedf("disk size cannot be reduced from %dG to %dG", current.DiskGB, *diskGB)
	}
	return nil
}

// Resize adjusts the VM spec: cpu and mem change via SetVMConfig
// (synchronous on PVE 7/8/9), a larger disk via ResizeDisk. The changes are
// applied to PVE first and only then persisted to the vms row. The persist
// step uses the spec read at the start as an optimistic lock (UpdateSpec
// re-checks it in the WHERE clause): when a concurrent resize committed in
// between, the caller gets KindConflict and can retry. It returns the fresh
// VM record on success.
func (s *VMService) Resize(ctx context.Context, id int64, cpu *int, memMB, diskGB *int64) (*repository.VMWithIP, error) {
	vm, node, err := s.vmAndNode(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validateResizeSpec(cpu, memMB, diskGB, vm.VM); err != nil {
		return nil, err
	}

	// The spec to persist once the PVE-side changes succeeded: requested
	// fields take the new values, the others stay as read. The values read
	// here double as the optimistic-lock baseline for UpdateSpec.
	next := vm.VM
	if cpu != nil {
		next.CPU = *cpu
	}
	if memMB != nil {
		next.MemMB = *memMB
	}
	if diskGB != nil {
		next.DiskGB = *diskGB
	}

	client := s.newClient(node.Host, node.APIUser, node.APITokenSecret)

	changed := false
	if (cpu != nil && *cpu != vm.VM.CPU) || (memMB != nil && *memMB != vm.VM.MemMB) {
		params := pve.VMConfigParams{}
		if cpu != nil {
			c := *cpu
			params.Cores = &c
		}
		if memMB != nil {
			m := *memMB
			params.MemoryMB = &m
		}
		if _, err := client.SetVMConfig(ctx, node.Name, vm.VM.PVEVmid, params); err != nil {
			return nil, fmt.Errorf("resize vm %d: set config: %w", id, err)
		}
		changed = true
	}

	if diskGB != nil && *diskGB > vm.VM.DiskGB {
		cfg, err := client.GetVMConfig(ctx, node.Name, vm.VM.PVEVmid)
		if err != nil {
			return nil, fmt.Errorf("resize vm %d: read config: %w", id, err)
		}
		boot := cfg.BootDisk()
		if boot == "" {
			boot = "scsi0"
		}
		upid, err := client.ResizeDisk(ctx, node.Name, vm.VM.PVEVmid, boot, *diskGB)
		if err != nil {
			return nil, fmt.Errorf("resize vm %d: resize disk: %w", id, err)
		}
		if upid != "" {
			if _, err := client.WaitTask(ctx, node.Name, upid, 0, 0); err != nil {
				return nil, fmt.Errorf("resize vm %d: wait resize: %w", id, err)
			}
		}
		changed = true
	}

	if !changed {
		return vm, nil // no-op (e.g. disk_gb equal to the current value)
	}

	if err := s.vmRepo.UpdateSpec(ctx, id, next.CPU, next.MemMB, next.DiskGB,
		vm.VM.CPU, vm.VM.MemMB, vm.VM.DiskGB); err != nil {
		if errors.Is(err, repository.ErrSpecConflict) {
			return nil, conflictf("规格已被并发修改，请重试")
		}
		return nil, fmt.Errorf("resize vm %d: persist spec: %w", id, err)
	}
	return s.vmRepo.GetVM(ctx, id)
}
