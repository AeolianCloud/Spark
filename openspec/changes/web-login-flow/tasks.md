## 1. 令牌与登录 API

- [ ] 1.1 `web/app/api/auth.ts`：新增 `adminLogin(username, password)` 调用 `POST /auth/admin/login`，类型经 `types.ts` 从生成契约导出
- [ ] 1.2 `web/app/api/types.ts`：导出 `AdminLoginRequest`/`AdminLoginResponse`（对应契约 schema，含 `token/admin_id/username`）

## 2. 令牌存取与请求注入

- [ ] 2.1 `web/app/utils/auth.ts`：localStorage 存取 `getToken/setAuth/clearAuth/getIdentity`（key 前缀 `spark.`）
- [ ] 2.2 `web/app/api/client.ts`：`request()` 统一注入 `Authorization: Bearer <token>`（排除 `/auth/*` 与 `/healthz`）；响应 401 且非登录请求时清令牌并跳转 `/login` 后抛 ApiError

## 3. 登录状态 composable

- [ ] 3.1 `web/app/composables/useAuth.ts`：响应式 `isLoggedIn/identity`，`login()`（调 adminLogin 成功后写令牌）、`logout()`（清令牌并跳转登录页）

## 4. 路由守卫与登录页

- [ ] 4.1 `web/app/middleware/auth.global.ts`：未登录访问业务页 → `/login`；已登录访问 `/login` → `/dashboard`
- [ ] 4.2 `web/app/pages/login.vue`：独立布局（`layout: false`）居中登录表单，提交调 `useAuth.login()`，成功跳 `/dashboard`，失败展示错误

## 5. 布局与登出入口

- [ ] 5.1 `web/app/layouts/default.vue`：删除"无鉴权管理面"页脚文案，展示登录管理员 username 与登出按钮

## 6. 验证

- [ ] 6.1 `npm run lint`、`npm run typecheck`、`npm run api:check` 通过
- [ ] 6.2 `npm run generate` 构建通过；浏览器/curl 验证：未登录访问跳转登录页、登录成功进首页、带令牌请求成功、无令牌/过期令牌 401 跳登录、登出清令牌
