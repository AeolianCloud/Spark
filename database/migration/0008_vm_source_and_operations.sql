-- 为 vms 表新增来源标识列（source），并新建虚拟机生命周期操作记录表
-- （vm_operations）。本迁移在 0007_import_vm.sql 之后应用。
--
-- vms.source 列（设计 D3）：
--   spark_created：由 Spark 镜像创建（列默认值，存量创建链路不受影响）；
--   claimed：已认领（原"导入"）的外部虚拟机；
--   external：PVE 上存在而本地无记录——不落库，由列表接口对 PVE 全量摘要
--   与本地记录实时差集判定。
-- 存量已导入行（image_id IS NULL，0007 放宽约束后导入的行）回填 claimed，
-- 其余行保持默认值 spark_created。
--
-- vm_operations 表（设计 D5）：
--   生命周期操作（start/stop/reboot/destroy）被 PVE 受理后同步写入的审计
--   记录；result 取 accepted / failed，失败原因记录在 error_message。
--   node_id 外键引用 pve_nodes(id)，未指定 ON DELETE 子句因此行为为默认的
--   NO ACTION：只要节点上留有操作记录，删除该节点就会被数据库拒绝（须先
--   清理操作记录才能删节点）；操作记录不随 vms 行删除而删除（无 vms 外键），
--   供审计与排障使用。
--   error_message 为 VARCHAR(1000)：service 层落库前已截断并脱敏，列约束
--   兜底拒绝超长值（SQLSTATE 22001）。
--   user_id 列预留可空：用户体系（单独提案）启用前恒为 NULL。
ALTER TABLE vms ADD COLUMN source TEXT NOT NULL DEFAULT 'spark_created';
UPDATE vms SET source = 'claimed' WHERE image_id IS NULL;

CREATE TABLE vm_operations (
    id            BIGSERIAL PRIMARY KEY,
    node_id       BIGINT NOT NULL REFERENCES pve_nodes(id),
    pve_vmid      BIGINT NOT NULL,
    action        TEXT NOT NULL,
    result        TEXT NOT NULL,
    error_message VARCHAR(1000) NOT NULL DEFAULT '',
    user_id       BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 操作记录查询按 (node_id, pve_vmid) 过滤并按时间倒序分页，因此为
-- node_id + pve_vmid 建复合索引，created_at DESC 直接服务倒序查询。
CREATE INDEX vm_operations_node_vmid_created_idx
    ON vm_operations (node_id, pve_vmid, created_at DESC);
