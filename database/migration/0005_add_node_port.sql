-- 为 pve_nodes 表新增节点 API 端口列。本迁移在 0004_add_provision_error.sql 之后应用。
--
-- 为什么需要这个列：用户可能以 host:port 形式录入节点，而旧代码会剥离端口
-- 并硬编码 8006，导致对监听在其他端口的节点连接被拒。新增的 port 列将端口
-- 显式保存，默认值为 8006（PVE 默认 HTTPS 端口）。
--
-- 回填意图：host 以 :<digits> 后缀结尾的存量行，会把该后缀解析为端口（非法
-- 端口如超出 1-65535 范围时回落默认值 8006）并从 host 中移除，修复存量节点
-- 在 host 字段中内嵌端口的问题。正则限位 \d{1,5}$ 可避免超长数字后缀转换时
-- 触发 integer 溢出，保证迁移在单事务中不会失败回滚。
ALTER TABLE pve_nodes ADD COLUMN port INTEGER NOT NULL DEFAULT 8006;
UPDATE pve_nodes SET port = CASE WHEN host ~ ':\d{1,5}$' AND CAST(substring(host FROM '(\d{1,5})$') AS INTEGER) BETWEEN 1 AND 65535
  THEN CAST(substring(host FROM '(\d{1,5})$') AS INTEGER) ELSE 8006 END
  WHERE host ~ ':\d+$';
UPDATE pve_nodes SET host = regexp_replace(host, ':\d+$', '', 'g') WHERE host ~ ':\d+$';
