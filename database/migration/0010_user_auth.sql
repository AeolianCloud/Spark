-- 新增用户体系核心表（admins / users），并为 vms 与 vm_operations 补充
-- 归属与操作者字段。本迁移在 0009_image_download.sql 之后应用（文件名
-- 升序：image_download < user_auth），是"用户体系与双身份认证"提案的
-- 数据库部分（设计 D3 / D5 / D8）。
--
-- admins 表（设计 D3）：管理员账号，与前台用户分属两个身份域
-- （双身份认证）。username 唯一标识管理员；password_hash 存 bcrypt
-- 不可逆哈希（设计 D1），不存明文，因此无法解密回用。created_at /
-- updated_at 由应用层维护。
-- users 表（设计 D3）：前台用户账号。name 为展示名，默认空字符串；
-- status 取 enabled / disabled，disabled 用户登录与鉴权均被拒绝
-- （设计 D4/D5 每次请求查库校验）。
-- vms.user_id（设计 D3）：虚拟机归属用户；创建/认领时可选传入，
-- 不传则为 NULL=无主。与 0008_vm_source_and_operations.sql 新增的
-- vms.source 互不冲突：两者同表追加列，source 描述来源、user_id
-- 描述归属。
-- vm_operations 操作者列（设计 D5/D8）：0008 迁移已为 vm_operations
-- 预留可空的 user_id 列（用户体系启用前恒为 NULL）；本迁移补充
-- operator_type（admin / user）与 operator_id（对应表 ID）作为实际
-- 操作者。两者均为 NULL 表示旧记录（用户体系落地前产生的操作，无
-- 操作者信息）。
CREATE TABLE admins (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'enabled',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- vms 表加归属列：外键引用 users(id)，未指定 ON DELETE 子句因此行为为
-- 默认的 NO ACTION——删除用户时只要名下还有虚拟机，删除就会被数据库
-- 拒绝（service 层将外键违反映射为 user_has_resources 冲突错误，设计
-- D6 的"有资源禁删"）。
ALTER TABLE vms ADD COLUMN user_id BIGINT NULL REFERENCES users(id);

-- vm_operations 表补操作者列（设计 D5/D8）：operator_type 取 admin /
-- user，operator_id 为对应身份域表（admins / users）的 ID；两者均为
-- NULL 表示旧记录（无操作者信息）。0008 迁移预留的 user_id 列保留
-- 不动，操作者语义以本组列（operator_type / operator_id）为准。
ALTER TABLE vm_operations
    ADD COLUMN operator_type TEXT,
    ADD COLUMN operator_id BIGINT;
