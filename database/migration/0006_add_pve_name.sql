-- 为 pve_nodes 表新增 PVE 集群节点名列。本迁移在 0005_add_node_port.sql 之后应用。
--
-- 为什么需要这个列：PVE 节点业务名（Name）与 PVE 集群节点名是分离的两套概念，
-- 而旧代码使用业务名直接请求 PVE API 的 /nodes/{name} 路径，当两者不一致时
-- 会导致 PVE 返回 595 错误（节点不存在）。新增的 pve_name 列用于显式保存
-- PVE 集群节点名，默认值为空字符串（''），表示尚未录入或未知，业务代码在
-- 为空时应回退到 name 列取值。
--
-- 回填意图：将存量行的 pve_name 初始化为与 name 相同，保证存量数据在没有
-- 显式录入集群节点名之前行为不回归（等价于旧代码直接用业务名请求节点）。
ALTER TABLE pve_nodes ADD COLUMN pve_name TEXT NOT NULL DEFAULT '';
UPDATE pve_nodes SET pve_name = name;
