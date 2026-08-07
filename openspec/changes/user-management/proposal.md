## Why

Spark 目前没有任何认证与用户概念（`vms` 表无 `user_id`，所有 API 裸奔），无法区分管理员与平台用户、无法支撑用户自助服务。参照智简魔方（idcsmart）模型：后端管理用户表，前台用户与后台管理员双身份、双 JWT 认证，用户登录后只能看到与操作自己名下的资源。

## What Changes

- 新增 `users` 表（自助服务登录用户）：`id`、`username`（登录账号，管理员预置时指定）、`password_encrypted`、`name`、`status`、时间戳；**BREAKING**：全 API 从裸奔变为强制认证
- 新增 `admins` 表（管理员）：`id`、`username`、`password_encrypted`；种子管理员由 CLI 命令创建
- 双登录接口：用户登录 `POST /auth/login`、管理员登录 `POST /auth/admin/login`，各返回 JWT（Bearer）
- 鉴权中间件：除登录与健康检查外全部接口要求有效 JWT；管理员接口（如用户 CRUD）仅管理员令牌可访问
- 管理员侧用户管理 CRUD（仿智简魔方）：列表/新建/详情/修改/删除/状态切换；删除时名下有资源 → 禁止删除
- 资源归属：`vms.user_id`（可空 FK）；创建/认领时可选指定归属用户，不指定则为无主
- 列表按身份分流：管理员令牌 → 全部虚拟机（含外部 VM）；用户令牌 → 仅显示自己名下的；外部 VM 对用户不可见
- 生命周期操作归属校验：用户令牌操作虚拟机时校验归属（非本人资源返回 403），外部 VM 用户不可操作
- 操作记录：`vm_operations` 记录操作者（admin 或 user）与归属快照
- OpenAPI 契约双副本同步，错误码同步 `docs/api-errors.md`
- **不在本次范围**：开放注册/忘记密码/验证码（管理员预置账号，改密后移）、前端页面实现、用户间资源流转

## Capabilities

### New Capabilities
- `user-auth`: 管理员与平台用户的账号体系、双 JWT 登录与鉴权
- `user-management`: 管理员侧的用户 CRUD 与状态管理

### Modified Capabilities
- `vm-lifecycle`: 虚拟机列表按身份过滤（管理员全部/用户仅自己）、生命周期操作归属校验

## Impact

- **后端**：`api/`（登录 handler、鉴权中间件、用户 CRUD handler、路由挂载）、`service/`（auth 服务、用户服务、VM 列表分流与归属校验）、`repository/`（user/admin 仓储）、`database/migration/0008`（users/admins 表、`vms.user_id`）、`cmd/`（种子管理员 CLI）
- **依赖**：新增 `golang-jwt/jwt/v5`（JWT）与 `golang.org/x/crypto`（bcrypt 密码哈希）
- **API**：`/auth/login`、`/auth/admin/login`、`/users` CRUD、`/vms` 响应新增归属字段；契约双副本 + `docs/api-errors.md`
- **测试**：单测（auth/用户 CRUD/归属校验）+ e2e（fake PVE 配合 token 注入、双身份分流断言）
- **依赖提案**：`all-pve-vms-visible`（外部 VM 可见性、`vm_operations` 表）——`vms.user_id` 与操作者记录在本提案落地，建议实现顺序上 user-management 先行或同步协调
