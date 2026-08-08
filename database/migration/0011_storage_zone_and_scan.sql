-- 存储类型由"手动登记"改为"扫描同步"（提案 auto-scan-pve-storage）。
-- 在 0010_user_auth.sql 之后应用。
--
-- 变更摘要：
-- * zone_id：存储按 zone 归属（一个 zone 对应一个 PVE 集群），唯一键
--   从 name 改为 (zone_id, pve_storage)——0003 建立的 name 全局唯一索引
--   移除（不同 zone 存在同名存储，如每个集群都有 local）。
-- * enabled：管理员启用开关，默认开启。
-- * type / content：PVE 存储类型与内容能力快照，由扫描权威填充；
--   查询时据此派生 capabilities（images/iso/backup/vztmpl/rootdir/
--   snippets + can_download_image）。
-- * name 可空：业务名由管理员补填，为空时对外展示回退到 pve_storage。
-- * display_name 移除：展示语义并入 name（name || pve_storage）。
--
-- 存量数据归置：zones 仅一行时全量归入该 zone；多 zone 且 storage_types
-- 非空时无法自动推断归属，中止迁移并提示人工处理后再执行；多 zone 但
-- 表为空、或 zones 为空（全新部署）时直接继续（空表可安全加 NOT NULL
-- 约束）。zones 为空但 storage_types 非空属于异常数据状态（zones 被清
-- 空），同样中止并提示先建 zone。归置 UPDATE 引用 zone_id 列，必须在
-- 该列 ADD 之后执行。
-- 事务性说明：整个迁移由 database.Migrate 包在单个事务中执行，任何
-- 分支 RAISE EXCEPTION 都会连同上方 ADD COLUMN 一起整体回滚，不产生
-- 部分变更。

ALTER TABLE storage_types
    ADD COLUMN zone_id BIGINT REFERENCES zones(id),
    ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN type TEXT,
    ADD COLUMN content TEXT;

DO $$
DECLARE
    zone_count   integer;
    row_count    integer;
    target_zone  bigint;
BEGIN
    SELECT count(*) INTO zone_count FROM zones;
    SELECT count(*) INTO row_count FROM storage_types;

    IF zone_count = 1 THEN
        SELECT id INTO target_zone FROM zones LIMIT 1;
        UPDATE storage_types SET zone_id = target_zone;
    ELSIF zone_count = 0 AND row_count > 0 THEN
        RAISE EXCEPTION 'zones 表为空但 storage_types 有存量数据，无法归置 zone_id，请先创建 zone 后再迁移';
    ELSIF zone_count > 1 AND row_count > 0 THEN
        RAISE EXCEPTION '存在多个 zone 且 storage_types 有存量数据，无法自动归置 zone_id，请人工归置后再迁移';
    END IF;
END $$;

-- 存量行已在上方 DO 块归置完毕，zone_id 从此刻起必须非空
ALTER TABLE storage_types ALTER COLUMN zone_id SET NOT NULL;

-- 唯一键迁移：移除 name 全局唯一（不同 zone 同名存储会冲突），
-- 改为 (zone_id, pve_storage) 唯一（PVE 存储名在集群内唯一）
DROP INDEX storage_types_name_key;
CREATE UNIQUE INDEX storage_types_zone_pve_storage_key
    ON storage_types(zone_id, pve_storage);

-- name 可空（扫描新建时为 NULL，待管理员补填）；display_name 移除
ALTER TABLE storage_types ALTER COLUMN name DROP NOT NULL;
ALTER TABLE storage_types DROP COLUMN display_name;
