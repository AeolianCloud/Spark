## Why

后端 user-auth 已生效（2026-08-08 归档）：全部接口除登录与健康检查外强制 `Authorization: Bearer JWT`，匿名请求一律 401。但前端 `web/` 尚无任何登录流程——API client 不携带令牌、没有登录页、没有令牌存取逻辑，导致整个管理界面所有请求全部 401 加载失败，界面不可用。

## What Changes

- 新增登录页（`/login`）：平台管理员账号密码登录，调用 `POST /auth/admin/login` 获取 admin JWT；用户自助登录（`POST /auth/login`）本次暂不做（管理界面仅面向运维人员，需用户侧登录时另立变更）
- 令牌管理：登录成功后令牌与身份信息持久化（localStorage），提供统一 auth composable（登录/登出/身份状态）
- API client 注入：`web/app/api/client.ts` 统一在请求头附加 `Authorization: Bearer <token>`；登录接口本身与健康检查不注入
- 401 统一处理：任意接口返回 401 unauthorized 时清除本地令牌并跳转登录页
- 路由守卫：未登录访问任意业务页面时重定向到 `/login`，已登录访问 `/login` 时跳回首页
- 登出：界面提供退出登录入口，清除本地令牌后跳转登录页
- 前端类型定义同步：`web/app/api/types.ts` 增加登录请求/响应类型（与契约 `docs/openapi.yaml` 中 `/auth/admin/login` 一致）

## Capabilities

### New Capabilities

- `web-auth`: 管理界面的前端登录流程——登录页、令牌持久化、API 请求令牌注入、401 跳转与登出

### Modified Capabilities

- `web-management-ui`: 「无鉴权访问」要求作废，改为「全部界面操作须在登录后携带有效 JWT 访问；未登录自动导向登录页」

## Impact

- **前端**：`web/app/`——新增 `pages/login.vue`、`composables/useAuth.ts`（令牌存取/登出）、登录请求与类型（`api/auth.ts`、`types.ts`）、`api/client.ts` 注入与 401 统一处理、`layouts/default.vue` 与导航加登出入口、nuxt route middleware 路由守卫
- **后端**：无接口变更（复用现有 `/auth/admin/login`）；`docs/openapi.yaml` 不改
- **安全**：令牌仅存 localStorage（XSS 风险已有说明，管理界面为内网工具；后续可评估 httpOnly cookie 方案，另行变更）
- **测试**：前端构建与 `npm run api:check` 通过；手工/浏览器验证登录-鉴权-401 跳转链路
