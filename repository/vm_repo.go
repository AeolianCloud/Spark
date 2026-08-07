package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	"spark/model"
)

// VMWithIP 将 VM 行与其已分配地址的明文 IP 配对
// （VM 没有地址时为 ""）。实时状态从不落库（透传，设计 D1）。
//
// vms.pve_vmid 为 NOT NULL，因此零值兼任"尚未在 PVE 上创建"的哨兵：
// 数据库行在供给链路分配 PVE VMID 之前写入，pve_vmid 为零表示 VM
// 仍在创建中或供给失败。
type VMWithIP struct {
	VM model.VM
	IP string
}

// VMRepository 负责持久化 model.VM 行。IP 分配事务流程
// （INSERT vms -> 领取 ip -> 设置 vms.ip_id）由 VM 服务编排；
// 本仓库按照 migration 0002 头部的约定提供基于事务的步骤。
type VMRepository struct {
	pool pgxQuerier
}

// NewVMRepository 创建由 pool 支撑的 VMRepository。
func NewVMRepository(pool pgxQuerier) *VMRepository {
	return &VMRepository{pool: pool}
}

// vmCols 是 INSERT RETURNING 与读取查询使用的 vms 列清单。每一列
// 都带表限定名：GetVM/ListVMs 会将 vms 与 ips 做 JOIN，裸的 "id"
// 会产生歧义（SQLSTATE 42702）。provision_error 可为 NULL
// （migration 0004 添加时未带默认值）；password_encrypted 在
// migration 0007 之后也可为 NULL（导入的 VM 无密码）。这两列都使用
// COALESCE 扫描：NULL 行必须读作 "" 而不能让扫描到普通 string 失败。
// 带表限定名的列在 INSERT ... RETURNING 中合法。
const vmCols = "vms.id, vms.uuid, vms.name, vms.zone_id, vms.node_id, vms.pve_vmid, vms.image_id, vms.storage_type_id, vms.cpu, vms.mem_mb, vms.disk_gb, vms.ip_id, COALESCE(vms.password_encrypted, '') AS password_encrypted, COALESCE(vms.provision_error, '') AS provision_error, vms.created_at, vms.updated_at"

// CreateVMTx 在调用方的事务内以 ip_id 为 NULL、pve_vmid 为零插入 VM 行
// （migration 0002 分配流程的第 1 步：FK 环要求 ip_id 在 ips 领取之后
// 写入）。返回创建的行，已填充 id 与时间戳。
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

// GetVMByNodeVMID 按 (node_id, pve_vmid) 精确查询 VM 行，作为导入
// （纳管）的幂等检查：重复导入同一 PVE VMID 在服务层先被本方法拒绝，
// 并发场景由 vms_node_vmid_key 部分唯一索引兜底。不存在时返回
// pgx.ErrNoRows。
func (r *VMRepository) GetVMByNodeVMID(ctx context.Context, nodeID, vmid int64) (*model.VM, error) {
	var v model.VM
	err := r.pool.QueryRow(ctx,
		"SELECT "+vmCols+" FROM vms WHERE node_id=$1 AND pve_vmid=$2", nodeID, vmid,
	).Scan(&v.ID, &v.UUID, &v.Name, &v.ZoneID, &v.NodeID, &v.PVEVmid, &v.ImageID, &v.StorageTypeID,
		&v.CPU, &v.MemMB, &v.DiskGB, &v.IPID, &v.PasswordEncrypted, &v.ProvisionError,
		&v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &v, nil
}

// ImportVMTx 在调用方的事务内插入"导入的已有 VM"行（migration 0007）。
// 与 CreateVMTx 的差异：pve_vmid 写入调用方从 PVE 读到的非零 VMID；
// image_id、storage_type_id、password_encrypted 与 ip_id 全部写入 NULL
// （导入的 VM 无镜像/存储类型/密码，地址稍后按需领取，见
// IPPoolRepository.ClaimIPByAddressTx）。同一 (node_id, pve_vmid) 的
// 重复导入由 vms_node_vmid_key 部分唯一索引拒绝，23505 经
// classifyDBError 映射为 ErrConflict。
func (r *VMRepository) ImportVMTx(ctx context.Context, tx pgx.Tx, vm model.VM) (*model.VM, error) {
	var created model.VM
	err := tx.QueryRow(ctx,
		"INSERT INTO vms (uuid, name, zone_id, node_id, pve_vmid, image_id, storage_type_id, cpu, mem_mb, disk_gb, ip_id, password_encrypted) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULL, NULL) RETURNING "+vmCols,
		vm.UUID, vm.Name, vm.ZoneID, vm.NodeID, vm.PVEVmid, vm.ImageID, vm.StorageTypeID,
		vm.CPU, vm.MemMB, vm.DiskGB,
	).Scan(&created.ID, &created.UUID, &created.Name, &created.ZoneID, &created.NodeID,
		&created.PVEVmid, &created.ImageID, &created.StorageTypeID, &created.CPU, &created.MemMB,
		&created.DiskGB, &created.IPID, &created.PasswordEncrypted, &created.ProvisionError,
		&created.CreatedAt, &created.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &created, nil
}

// SetVMIPIDTx 在调用方的事务内把已领取的地址关联到 VM 行
// （migration 0002 分配流程的第 3 步）。
func (r *VMRepository) SetVMIPIDTx(ctx context.Context, tx pgx.Tx, id, ipID int64) error {
	if _, err := tx.Exec(ctx, "UPDATE vms SET ip_id=$1, updated_at=now() WHERE id=$2", ipID, id); err != nil {
		return err
	}
	return nil
}

// GetVM 返回指定 id 的 VM 及其明文 IP 的 JOIN 结果；不存在时返回
// pgx.ErrNoRows。LEFT JOIN 保证没有分配地址的 VM 仍可读取
// （防御性写法；v1 中每个 VM 创建时都会分配 IP）。
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

// ListVMs 返回带明文 IP 的每一行 VM（VM 没有地址时为 ""），按 id
// 排序。它服务于透传式列表查询（任务 8.1）：与 PVE 实时状态的合并
// 发生在服务层，永不落库（设计 D1）。
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

// ListVMsPage 返回按 id 排序的一页 VM 行。它服务于分页的透传式
// 列表：LIMIT/OFFSET 作用于本地元数据，PVE 合并只在该页的行上执行
// （最坏情况下每次节点调用最多 maxPageLimit 行）。
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

// CountVMs 返回 VM 行总数，支撑 GET /vms 的 X-Total-Count 响应头。
// 只统计本地元数据：透传式合并可能丢弃故障节点的行，因此总数可能
// 超过该页的条目数（上报的总数刻意采用完整的本地统计）。
func (r *VMRepository) CountVMs(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM vms").Scan(&n); err != nil {
		return 0, classifyDBError(err)
	}
	return n, nil
}

// UpdateVMPVEVMID 记录供给链路分配的 PVE VMID，并同步实际磁盘大小
// （无需扩容时可能为导入镜像的大小）；供给成功会清除任何
// provision_error。
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

// SetProvisionError 持久化经过净化的供给失败信息。VM 保留其 IP 与
// 零值 pve_vmid，便于运维检查该行（设计 D5：v1 不做自动重试或回滚）。
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

// UpdateSpec 以乐观锁对 vms 行应用完整的规格变更：WHERE 子句重新检查
// 调用方在变更前读到的规格值（旧值），因此期间并发提交的扩容会产生
// ErrSpecConflict，调用方可重试。行整体消失的情况（在 PVE 侧变更应用
// 期间被销毁）与规格竞争无法区分，同样报告为 ErrSpecConflict。
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

// DeleteVMTx 在调用方的事务内删除 vms 行。按照 migration 0002 的约定，
// 调用方必须在此删除之前的同一事务内释放 VM 的 IP（见
// IPPoolRepository.ReleaseIPByVMTx），这样被释放的地址永远不会以
// status='used' 且无归属者的状态留存。
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
