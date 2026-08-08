-- 存储节点挂载快照（提案 auto-scan-pve-storage 设计 D8）。
-- 在 0011_storage_zone_and_scan.sql 之后应用。
--
-- 变更摘要：
-- * nodes：该存储挂载的节点名列表，存 PVE GET /storage 的 nodes 原文
--   （逗号分隔，如 "pve1,pve2"）；空串（''）语义为"不限制节点、所有
--   节点可用"。由扫描权威填充（仅扫描可写，与 pve_storage/type/content
--   同级，UpdateMeta 不触碰），创建 VM 调度时据此过滤候选节点——只把
--   磁盘发到挂载了所选存储的节点。
--
-- 备选 TEXT[] 的取舍（设计 D8）：PVE 原文就是逗号分隔字符串，TEXT +
-- 逗号拆分与既有列（content 同为逗号分隔）风格一致、迁移与测试更简单，
-- 故选 TEXT。空串与 NULL 语义相同（均表示不限制），列非空默认 ''，
-- 避免 NULL/空串双形态。

ALTER TABLE storage_types ADD COLUMN nodes TEXT NOT NULL DEFAULT '';
