-- 为 vms 表放宽"导入已有 VM（纳管）"所需的约束，并新增 (node_id, pve_vmid)
-- 部分唯一索引。本迁移在 0006_add_pve_name.sql 之后应用。
--
-- 为什么允许这三列为 NULL：供给链路创建的 VM 必然携带镜像、存储类型与
-- 加密密码，因此 0002_create_tables.sql 将这三列声明为 NOT NULL；但导入
-- 的 VM 来自 PVE 上已经存在的虚拟机，可能没有关联的云镜像、存储类型或
-- 密码，三者语义为"无"。这里只去掉 NOT NULL 约束，image_id 与
-- storage_type_id 上的外键约束（REFERENCES images/storage_types）保留
-- 不动——非空值仍必须引用存在的行。
--
-- 为什么使用部分唯一索引：pve_vmid 在供给链路中兼任"尚未在 PVE 上创建"
-- 的哨兵（此时为 0，见 repository/vm_repo.go 中 VMWithIP 的注释），
-- 供给中的 VM 多行 pve_vmid 同为 0。若对 (node_id, pve_vmid) 建立全表
-- 唯一索引，这些行会互相冲突；部分唯一索引只约束 pve_vmid > 0 的行，
-- 即真实存在于 PVE 上的虚拟机（含导入的 VM），从而在数据库层兜底防止
-- 同一 PVE VMID 被重复导入（并发下的 23505 由仓库 classifyDBError
-- 映射为 ErrConflict，幂等检查 GetVMByNodeVMID 负责常规路径）。
ALTER TABLE vms ALTER COLUMN image_id DROP NOT NULL;
ALTER TABLE vms ALTER COLUMN storage_type_id DROP NOT NULL;
ALTER TABLE vms ALTER COLUMN password_encrypted DROP NOT NULL;
CREATE UNIQUE INDEX vms_node_vmid_key ON vms(node_id, pve_vmid) WHERE pve_vmid > 0;
