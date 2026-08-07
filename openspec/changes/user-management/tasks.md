## 1. 依赖与配置

- [ ] 1.1 引入 `golang-jwt/jwt/v5` 与 `golang.org/x/crypto`（bcrypt）依赖，`go mod tidy`
- [ ] 1.2 `config.yaml` 新增 `auth.jwt_secret`（必填，缺失启动报错），config 结构体与加载逻辑同步

## 2. 数据库迁移

- [ ] 2.1 新增迁移 0009：`admins` 表（id/username 唯一/password_hash/时间戳）
- [ ] 2.2 新增迁移 0009：`users` 表（id/username 唯一/password_hash/name/status/时间戳）
- [ ] 2.3 新增迁移 0009：`vms` 表加 `user_id BIGINT NULL REFERENCES users(id)` 列
- [ ] 2.4 如 `vm_operations`（all-pve-vms-visible 迁移 0008）缺少操作者列，本迁移补 `operator_type`/`operator_id`
- [ ] 2.5 model 层：新增 `Admin`、`User` 实体，`VM` 加 `UserID` 字段

## 3. 认证核心

- [ ] 3.1 auth service：密码 bcrypt 哈希/校验、JWT 签发（HS256，claims: sub/role/iat/exp，24h）
- [ ] 3.2 `POST /auth/login`（用户）：校验账号/密码/启用状态，成功返回 user JWT + user_id
- [ ] 3.3 `POST /auth/admin/login`（管理员）：同上返回 admin JWT + admin_id
- [ ] 3.4 错误处理：凭证无效统一 `unauthorized`（401），不区分账号不存在/密码错误；禁用用户拒绝登录
- [ ] 3.5 单测：登录成功/失败、禁用、JWT 签发与解析边界

## 4. 鉴权中间件

- [ ] 4.1 `requireAuth` 中间件：解析 Bearer JWT → 校验 role/exp → 按 role 查库校验 status=enabled → 身份注入 gin.Context
- [ ] 4.2 `requireAdmin` 中间件：管理员令牌才放行，用户令牌返回无权访问（403）
- [ ] 4.3 路由挂载：公开组（health、双登录）与受保护组分层；用户 CRUD 挂 requireAdmin
- [ ] 4.4 单测：缺失/非法/过期令牌、禁用用户、user 访问 admin 接口

## 5. 用户管理 CRUD

- [ ] 5.1 repository：User 仓储（创建/列表分页/详情/更新/删除/状态切换，username 唯一约束映射冲突错误）
- [ ] 5.2 service + handler：`POST /users`（username 唯一 + 初始密码 + name）
- [ ] 5.3 `GET /users` 分页列表（X-Total-Count）+ `GET /users/{id}` 详情
- [ ] 5.4 `PUT /users/{id}` 修改（name/重置密码）
- [ ] 5.5 `DELETE /users/{id}`：名下存在 vms.user_id 引用 → `user_has_resources` 冲突错误；无引用删除成功
- [ ] 5.6 `PUT /users/{id}/status` 启用/禁用；错误码同步 `docs/api-errors.md`
- [ ] 5.7 单测：CRUD 全路径、唯一冲突、有资源禁删

## 6. VM 归属与身份分流

- [ ] 6.1 创建 VM：请求体支持可选 `user_id`，落库 `vms.user_id`（不传为 NULL）
- [ ] 6.2 认领 VM（`POST /vms/import`）：请求体支持可选 `user_id`，认领时写入归属
- [ ] 6.3 `GET /vms` 分流：admin 全量合并（含 external）；user 仅返回 `user_id = 自身` 的虚拟机（external 天然排除）
- [ ] 6.4 生命周期操作（start/stop/reboot/destroy/resize）：admin 放行；user 校验 VM 归属自身，否则 403；`ext-` 合成标识对 user 一律 403
- [ ] 6.5 详情接口同样按身份校验（user 访问他人 VM 详情 → 403）
- [ ] 6.6 `vm_operations` 写入操作者（operator_type/operator_id：admin 或 user）
- [ ] 6.7 单测：分流、归属校验、无主 VM 双视角可见性

## 7. CLI 种子管理员

- [ ] 7.1 `cmd/` 新增子命令（`spark admin create --username --password`），密码 bcrypt 落库
- [ ] 7.2 命令说明写入 README/文档；重复创建同账号返回冲突提示

## 8. 契约同步

- [ ] 8.1 `docs/openapi.yaml`：`/auth/login`、`/auth/admin/login`、`/users` CRUD 全套、`/vms` 响应加归属字段、创建/认领请求体加 `user_id`，operationId 完整
- [ ] 8.2 同步副本 `api/swagger/openapi.yaml`，双副本字节一致；`npx --yes @redocly/cli lint docs/openapi.yaml` 通过

## 9. e2e 与回归

- [ ] 9.1 e2e：管理员 token 注入与全链路回归（现有用例改带 token）
- [ ] 9.2 e2e：用户登录 → 仅见自己 VM → 操作他人 VM 403 → 外部 VM 不可见
- [ ] 9.3 e2e：禁用用户登录拒绝、已签发令牌操作拒绝；用户 CRUD 与有资源禁删
- [ ] 9.4 `go test -tags=e2e ./e2e/ -count=1 -v` 全量通过；`go vet ./...` 与 `gofmt` 检查
