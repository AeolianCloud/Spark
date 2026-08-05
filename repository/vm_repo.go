package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	"spark/model"
)

// VMWithIP pairs a VM row with the plaintext IP of its allocated address
// ("" when the VM has no address). The live status is never stored
// (pass-through, design D1).
//
// vms.pve_vmid is NOT NULL, so the zero value doubles as the "not created on
// PVE yet" sentinel: the DB row is written before the provisioning chain
// assigns the PVE VMID, and a zero pve_vmid means the VM is still creating
// or failed provisioning.
type VMWithIP struct {
	VM model.VM
	IP string
}

// VMRepository persists model.VM rows. The IP-allocation transaction flow
// (INSERT vms -> claim ip -> set vms.ip_id) is orchestrated by the VM
// service; this repository provides the Tx-aware steps per the migration
// 0002 header conventions.
type VMRepository struct {
	pool pgxQuerier
}

// NewVMRepository creates a VMRepository backed by pool.
func NewVMRepository(pool pgxQuerier) *VMRepository {
	return &VMRepository{pool: pool}
}

// vmCols is the vms column list used by the insert RETURNING and the read
// queries. Every column is table-qualified: GetVM/ListVMs join vms against
// ips, and a bare "id" would be ambiguous (SQLSTATE 42702). provision_error
// is nullable (migration 0004 added it without a default), so it is scanned
// with COALESCE: a NULL row must read as "" and cannot fail the scan into a
// plain string. Table-qualified names are valid in INSERT ... RETURNING.
const vmCols = "vms.id, vms.uuid, vms.name, vms.zone_id, vms.node_id, vms.pve_vmid, vms.image_id, vms.storage_type_id, vms.cpu, vms.mem_mb, vms.disk_gb, vms.ip_id, vms.password_encrypted, COALESCE(vms.provision_error, '') AS provision_error, vms.created_at, vms.updated_at"

// CreateVMTx inserts the VM row inside the caller's transaction with ip_id
// NULL and pve_vmid zero (step 1 of the migration 0002 allocation flow: the
// FK cycle forces ip_id to be written after the ips claim). The created row
// is returned with id and timestamps filled.
func (r *VMRepository) CreateVMTx(ctx context.Context, tx pgx.Tx, vm model.VM) (*model.VM, error) {
	var created model.VM
	err := tx.QueryRow(ctx,
		"INSERT INTO vms (uuid, name, zone_id, node_id, pve_vmid, image_id, storage_type_id, cpu, mem_mb, disk_gb, ip_id, password_encrypted) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING "+vmCols,
		vm.UUID, vm.Name, vm.ZoneID, vm.NodeID, vm.PVEVmid, vm.ImageID, vm.StorageTypeID,
		vm.CPU, vm.MemMB, vm.DiskGB, vm.IPID, vm.PasswordEncrypted,
	).Scan(&created.ID, &created.UUID, &created.Name, &created.ZoneID, &created.NodeID,
		&created.PVEVmid, &created.ImageID, &created.StorageTypeID, &created.CPU, &created.MemMB,
		&created.DiskGB, &created.IPID, &created.PasswordEncrypted, &created.ProvisionError,
		&created.CreatedAt, &created.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &created, nil
}

// SetVMIPIDTx links the claimed address to the VM row inside the caller's
// transaction (step 3 of the migration 0002 allocation flow).
func (r *VMRepository) SetVMIPIDTx(ctx context.Context, tx pgx.Tx, id, ipID int64) error {
	if _, err := tx.Exec(ctx, "UPDATE vms SET ip_id=$1, updated_at=now() WHERE id=$2", ipID, id); err != nil {
		return err
	}
	return nil
}

// GetVM returns the VM with the given id joined with its plaintext IP, or
// pgx.ErrNoRows when absent. The LEFT JOIN keeps VMs without an allocated
// address readable (defensive; every v1 VM is created with an IP).
func (r *VMRepository) GetVM(ctx context.Context, id int64) (*VMWithIP, error) {
	var v model.VM
	var ip string
	err := r.pool.QueryRow(ctx,
		"SELECT "+vmCols+", COALESCE(ips.ip, '') FROM vms LEFT JOIN ips ON ips.id = vms.ip_id WHERE vms.id=$1", id,
	).Scan(&v.ID, &v.UUID, &v.Name, &v.ZoneID, &v.NodeID, &v.PVEVmid, &v.ImageID, &v.StorageTypeID,
		&v.CPU, &v.MemMB, &v.DiskGB, &v.IPID, &v.PasswordEncrypted, &v.ProvisionError,
		&v.CreatedAt, &v.UpdatedAt, &ip)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &VMWithIP{VM: v, IP: ip}, nil
}

// ListVMs returns every VM row with its plaintext IP ("" when the VM has no
// address), ordered by id. It feeds the pass-through list query (task 8.1):
// the merge with the live PVE state happens in the service layer and is
// never stored (design D1).
func (r *VMRepository) ListVMs(ctx context.Context) ([]VMWithIP, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT "+vmCols+", COALESCE(ips.ip, '') FROM vms LEFT JOIN ips ON ips.id = vms.ip_id ORDER BY vms.id")
	if err != nil {
		return nil, classifyDBError(err)
	}
	defer rows.Close()

	out := make([]VMWithIP, 0)
	for rows.Next() {
		var v model.VM
		var ip string
		if err := rows.Scan(&v.ID, &v.UUID, &v.Name, &v.ZoneID, &v.NodeID, &v.PVEVmid, &v.ImageID,
			&v.StorageTypeID, &v.CPU, &v.MemMB, &v.DiskGB, &v.IPID, &v.PasswordEncrypted,
			&v.ProvisionError, &v.CreatedAt, &v.UpdatedAt, &ip); err != nil {
			return nil, classifyDBError(err)
		}
		out = append(out, VMWithIP{VM: v, IP: ip})
	}
	if err := rows.Err(); err != nil {
		return nil, classifyDBError(err)
	}
	return out, nil
}

// ListVMsPage returns one page of VM rows ordered by id. It feeds the
// paginated pass-through list: LIMIT/OFFSET apply to the local metadata,
// and the PVE merge only runs on the page's rows (at most maxPageLimit rows
// per node call in the worst case).
func (r *VMRepository) ListVMsPage(ctx context.Context, limit, offset int) ([]VMWithIP, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT "+vmCols+", COALESCE(ips.ip, '') FROM vms LEFT JOIN ips ON ips.id = vms.ip_id ORDER BY vms.id LIMIT $1 OFFSET $2",
		limit, offset)
	if err != nil {
		return nil, classifyDBError(err)
	}
	defer rows.Close()

	out := make([]VMWithIP, 0)
	for rows.Next() {
		var v model.VM
		var ip string
		if err := rows.Scan(&v.ID, &v.UUID, &v.Name, &v.ZoneID, &v.NodeID, &v.PVEVmid, &v.ImageID,
			&v.StorageTypeID, &v.CPU, &v.MemMB, &v.DiskGB, &v.IPID, &v.PasswordEncrypted,
			&v.ProvisionError, &v.CreatedAt, &v.UpdatedAt, &ip); err != nil {
			return nil, classifyDBError(err)
		}
		out = append(out, VMWithIP{VM: v, IP: ip})
	}
	if err := rows.Err(); err != nil {
		return nil, classifyDBError(err)
	}
	return out, nil
}

// CountVMs returns the total number of VM rows, backing the X-Total-Count
// header of GET /vms. It counts local metadata only: the pass-through merge
// can drop rows of failed nodes, so the total may exceed the page's item
// count (the reported total is deliberately the full local count).
func (r *VMRepository) CountVMs(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM vms").Scan(&n); err != nil {
		return 0, classifyDBError(err)
	}
	return n, nil
}

// UpdateVMPVEVMID records the PVE VMID assigned by the provisioning chain
// and syncs the actual disk size (which may be the imported image's size
// when no resize was needed); a successful provision clears any
// provision_error.
func (r *VMRepository) UpdateVMPVEVMID(ctx context.Context, id, vmid, diskGB int64) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE vms SET pve_vmid=$1, disk_gb=$2, provision_error=NULL, updated_at=now() WHERE id=$3",
		vmid, diskGB, id)
	if err != nil {
		return classifyDBError(err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SetProvisionError persists the sanitized provisioning failure message. The
// VM keeps its IP and its zero pve_vmid so operators can inspect the row
// (design D5: no automatic retry or rollback in v1).
func (r *VMRepository) SetProvisionError(ctx context.Context, id int64, message string) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE vms SET provision_error=$1, updated_at=now() WHERE id=$2", message, id)
	if err != nil {
		return classifyDBError(err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// UpdateSpec applies a full spec change to the vms row with an optimistic
// lock: the WHERE clause re-checks the spec values the caller read before
// applying the change (old values), so a concurrent resize that committed in
// between yields ErrSpecConflict and the caller can retry. A row that
// vanished entirely (destroyed while the PVE-side change was applied) is
// indistinguishable from a spec race and is also reported as ErrSpecConflict.
func (r *VMRepository) UpdateSpec(ctx context.Context, id int64, newCPU int, newMemMB, newDiskGB int64, oldCPU int, oldMemMB, oldDiskGB int64) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE vms SET cpu=$1, mem_mb=$2, disk_gb=$3, updated_at=now() WHERE id=$4 AND cpu=$5 AND mem_mb=$6 AND disk_gb=$7",
		newCPU, newMemMB, newDiskGB, id, oldCPU, oldMemMB, oldDiskGB)
	if err != nil {
		return classifyDBError(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSpecConflict
	}
	return nil
}

// DeleteVMTx deletes the vms row inside the caller's transaction. Per the
// migration 0002 conventions the caller must release the VM's IP (see
// IPPoolRepository.ReleaseIPByVMTx) in the same transaction and BEFORE this
// delete, so a freed address never ends up with status='used' and no owner.
func (r *VMRepository) DeleteVMTx(ctx context.Context, tx pgx.Tx, id int64) error {
	tag, err := tx.Exec(ctx, "DELETE FROM vms WHERE id=$1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
