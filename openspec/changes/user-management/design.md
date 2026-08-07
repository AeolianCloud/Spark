# Design: 用户体系与双身份认证

## Context

动机见 proposal.md Why。现状要点（探索结论）：

- 项目零认证：无 user 概念、无 auth 中间件、`vms` 表无归属字段，所有 API 裸奔
- `go.mod` 无 JWT / bcrypt 依赖；已有 `crypto` 包（AES-256-GCM），用于 VM 密码等**需要解密回用**的敏感字段
- 参照智简魔方模型：后端管理用户表，前台用户（CLIENT_JWT）+ 后台管理员（ADMIN_JWT）双身份域
- `vm_operations` 表（操作记录）在 `all-pve-vms-visible` 提案的迁移 0008 中创建，本提案在其后落地（迁移 0009）

## Goals / Non-Goals

Goals:
- 双身份账号体系（users + admins）+ 双 JWT 登录与全接口鉴权
- 管理员侧用户 CRUD 与状态管理
- 资源归属（`vms.user_id`）+ 身份分流（管理员全局 / 用户仅自己）
- 生命周期操作归属校验，操作记录记操作者

Non-Goals:
- 开放注册、忘记密码、验证码/实名认证（管理员预置账号；改密接口本期提供）
- 令牌刷新、多端登录管理
- 前端页面实现

## Decisions

### D1: 密码用 bcrypt 不可逆哈希（新增 `golang.org/x/crypto`）

VM 密码用 AES 可逆加密（注入 cloudinit 需解密）；登录密码只需比对，用 bcrypt 防拖库逆向。不与其他字段混用 `crypto` 包。两套并存：`crypto` 管可逆敏感字段，bcrypt 管登录凭证。

### D2: JWT 用 HS256（新增 `golang-jwt/jwt/v5`），密钥入 config

- `config.yaml` 新增 `auth.jwt_secret`（缺失则启动报错，与现有 `crypto.encryption_key` 同风格）
- token claims：`sub`（身份 ID）、`role`（`admin` / `user`）、`iat`、`exp`
- 有效期 24h，无刷新机制（过期重新登录）
- e2e/测试通过注入式 jwt 生成器或直接生成测试令牌

### D3: 表结构（迁移 0009）

```
admins:  id BIGSERIAL PK, username TEXT NOT NULL UNIQUE,
         password_hash TEXT NOT NULL, created_at, updated_at
users:   id BIGSERIAL PK, username TEXT NOT NULL UNIQUE,
         password_hash TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
         status TEXT NOT NULL DEFAULT 'enabled'  -- enabled/disabled
         created_at, updated_at
vms:     + user_id BIGINT NULL REFERENCES users(id)
         （创建/认领可选传；不传为 NULL=无主）
```

`vms.user_id` 与 `all-pve-vms-visible` 的 `vms.source` 互不冲突，同表追加列。

### D4: 双登录入口 + 统一鉴权中间件

- `POST /auth/login`（用户）：username+password → user JWT + user_id
- `POST /auth/admin/login`（管理员）：username+password → admin JWT + admin_id
- 中间件 `requireAuth`：解析 Bearer JWT → 校验 role/exp → 按 role 查库校验 status=enabled（禁用即 401）→ 身份注入 gin.Context
- 路由分层：公开组（`/health`、两个登录）→ `requireAuth` 组（全部业务）→ 组内再挂 `requireAdmin`（用户 CRUD）
- 密码比对失败统一返回 `unauthorized`（401），不区分账号不存在/密码错误

### D5: 身份分流与归属校验

- `GET /vms`：`requireAuth` 后按 `role` 分流——admin 走现有全量合并（含 external）；user 走 `user_id = claims.sub` 过滤（external 天然排除，因为无本地行无归属）
- 生命周期操作（start/stop/reboot/destroy/规格调整）：操作前校验——admin 放行；user 要求 VM.user_id == 自身且 VM 存在，否则 403/404
- 归属校验与 `all-pve-vms-visible` 的 `ext-` 外部 VM 合成标识交互：user 令牌对 `ext-` 标识一律 403（external 无归属，用户不可操作）
- `vm_operations` 记录操作者：`operator_type`（admin/user）+ `operator_id`（对应表 ID），随 `all-pve-vms-visible` 的 vm_operations 结构扩展或本提案加列

### D6: 用户 CRUD 路由（admin-only）

```
GET    /users           列表（分页 + 总数）
POST   /users           创建（username 唯一 + 初始密码 + name）
GET    /users/{id}      详情
PUT    /users/{id}      修改（name / 重置密码）
DELETE /users/{id}      删除（名下 vms.user_id 存在 → 409 冲突）
PUT    /users/{id}/status  启用/禁用
```

删除用户时若 `vms` 表存在 `user_id = 该用户` 的行 → `user_has_resources` 冲突错误；操作记录表仅存 ID 引用不级联。

### D7: 种子管理员 CLI

`cmd/` 新增子命令（如 `spark admin create --username <u> --password <p>`），密码走同一 bcrypt；不提供默认密码。启动不强制要求存在管理员（首次使用时 CLI 创建），文档写明步骤。

### D8: 与 all-pve-vms-visible 的衔接

- 迁移顺序：0008（外部 VM 可见性 + vm_operations）→ 0009（本提案）
- `vm_operations` 的操作者字段：若 all-pve-vms-visible 已落地的表无操作者列，本提案迁移 0009 一并补 `operator_type`/`operator_id`
- 两个提案 tasks 存在依赖，apply 时按顺序串行

## Risks / Trade-offs

- 全 API 强制认证是破坏性变更 → 契约双副本同步 + e2e 全部改带 token；前端同批改（api:check 门禁）
- JWT 密钥泄漏风险 → config 必填 + 文档提示；HS256 对称密钥，未来可升级 RS256
- 禁用用户即时生效需每次请求查库 → users 表规模小可接受；后续可换短时令牌+缓存
- bcrypt 计算开销（默认 cost）→ 登录接口频次低，可接受
- `vms.user_id` 为无主 VM（NULL）时用户视角不可见、管理员视角可见 → 与 all-pve-vms-visible 语义一致

## Migration Plan

1. 迁移 0009：`admins` / `users` 表 + `vms.user_id` 列 + （如需要）vm_operations 操作者列
2. 依赖引入：`golang-jwt/jwt/v5`、`golang.org/x/crypto`（仅构建依赖，不安装系统软件）
3. 后端：auth 服务/登录 handler/中间件 → 用户 CRUD → VM 分流与归属校验 → CLI → 操作记录操作者
4. 契约：openapi 双副本 + api-errors 同步，lint 通过
5. e2e：token 注入、双身份分流、归属校验、禁用用户断言；`go test -tags=e2e` 全量通过
6. 回滚：迁移可逆（DROP 表/列）；认证开启后无法访问的旧客户端需同步升级

## Open Questions

- 令牌撤销：除禁用用户即时拒登外，是否需要主动踢出已登录会话（本期不做，记录即可）
