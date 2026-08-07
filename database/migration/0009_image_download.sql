-- 移除 images.node_images 列、新增 images.download_url 列，并新建镜像操作
-- 记录表（image_operations）。本迁移在 0008_vm_source_and_operations.sql
-- 之后应用。
--
-- images 表变更：
--   node_images（JSONB，各节点上的镜像部署状态）随镜像下载流程改版由
--   image_operations 表取代，因此删除该列；存量 node_images 数据随列删除
--   丢弃，新流程以 PVE 实时扫描为准，无需回填。列删除不可恢复，属破坏性
--   迁移。
--   download_url 记录镜像下载源地址。列默认值为 '' 占位：迁移在
--   service 层接入前先行落地，后续由 service 层保证写入非空值。
--
-- image_operations 表：
--   镜像操作（download 等）被受理后同步写入的审计记录；action 为操作
--   名称，result 取 running / success / failed（下载为异步任务，与
--   vm_operations 的受理语义不同），失败原因记录在 error_message，
--   upid 为 PVE 侧任务标识（UPID），节点尚未受理或未分配任务时为空。
--   image_id 与 node_id 外键引用 images(id) / pve_nodes(id)，未指定
--   ON DELETE 子句因此行为为默认的 NO ACTION：只要某镜像或节点上留有
--   操作记录，删除该镜像/节点就会被数据库拒绝（须先清理操作记录才能
--   删除），操作记录不随 images 行删除而删除，供审计与排障使用。
--   error_message 为 TEXT：service 层落库前已截断并脱敏，默认 '' 兜底
--   成功路径不写 NULL。不用 VARCHAR(1000) 的列级长度兜底：镜像下载失败
--   原因由 service 层截断与脱敏后落库，长度超限由 service 层处理。
--   user_id 列预留可空：用户体系（单独提案）启用前恒为 NULL。
--   updated_at 由 repository 的 UPDATE SQL 维护（now()）。
ALTER TABLE images DROP COLUMN node_images;
ALTER TABLE images ADD COLUMN download_url TEXT NOT NULL DEFAULT '';

CREATE TABLE image_operations (
    id            BIGSERIAL PRIMARY KEY,
    image_id      BIGINT NOT NULL REFERENCES images(id),
    node_id       BIGINT NOT NULL REFERENCES pve_nodes(id),
    action        TEXT NOT NULL,
    result        TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    user_id       BIGINT,
    upid          TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 操作记录查询按 image_id 过滤并按时间倒序分页，因此为 image_id 建
-- 复合索引，created_at DESC 直接服务倒序查询（与 vm_operations 的
-- node_id + pve_vmid + created_at DESC 索引风格一致）。
CREATE INDEX image_operations_image_id_created_idx
    ON image_operations (image_id, created_at DESC);
