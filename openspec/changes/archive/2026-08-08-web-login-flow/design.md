## Context

后端 user-auth 已落地：`POST /auth/admin/login` 返回 admin JWT（`adminLoginResponse{token,admin_id,username}`），全部业务接口强制 `Authorization: Bearer`，401 统一 `unauthorized`。前端为 Nuxt 4 SPA（`ssr: false`，`nuxt generate` 静态托管），现有 `web/app/api/client.ts` 统一请求入口无任何令牌处理，`layouts/default.vue` 页脚仍标注"无鉴权管理面"。本变更纯前端，不动后端契约。

## Goals / Non-Goals

**Goals:**

- 管理员登录页 + 令牌持久化 + 请求自动注入 + 401 统一跳转 + 登出，恢复管理界面可用性
- 契约驱动：登录请求/响应类型从生成契约导出，client.ts 为唯一请求入口注入点

**Non-Goals:**

- 用户侧登录（`POST /auth/login`）与注册流程（管理界面仅面向运维人员，后续另立变更）
- 令牌安全方案升级（httpOnly cookie、刷新令牌）——内网管理工具，localStorage 足够，另行评估
- 后端接口/契约任何改动

## Decisions

### D1: 令牌存储用 localStorage

登录返回的 JWT 与身份信息（admin_id/username）存 `localStorage`（key 前缀 `spark.`）。SPA 纯客户端、无 SSR，无服务端渲染取用需求；localStorage 不随请求自动发送，注入点集中在 client.ts。XSS 风险与 HTTP-only cookie 方案的权衡见 Risks。

### D2: 令牌注入与 401 跳转集中在 `api/client.ts`

唯一请求入口 `request()` 中统一追加 `Authorization: Bearer <token>`（`/auth/*` 登录路径与 `/healthz` 显式排除）；响应 401 且非登录请求时清除本地令牌并 `window.location.assign('/login')` 全量跳转（SPA 下保证状态干净），再抛 ApiError 供调用方兜底。理由：注入点唯一可避免各页面遗漏；401 跳转集中在客户端最底层，无需每个页面各自处理。

### D3: 令牌读写独立为 `utils/auth.ts`，composable 只做状态包装

`utils/auth.ts` 提供 `getToken/setAuth/clearAuth/getIdentity`（localStorage 读写，client.ts 直接依赖它，避免 client → composable 的循环依赖）；`composables/useAuth.ts` 提供响应式 `isLoggedIn/identity` 与 `login/logout` 动作（login 调用 `api/auth.ts` 成功后写令牌，logout 清令牌并跳转）。

### D4: 路由守卫用全局 middleware

`app/middleware/auth.global.ts`：未登录访问业务页 → `navigateTo('/login')`；已登录访问 `/login` → `navigateTo('/dashboard')`。SPA 下所有导航与首屏加载均走此守卫（middleware 在客户端执行）。

### D5: 登录页为独立轻量布局

`pages/login.vue` 不套用默认侧边栏布局：页面顶部指定 `definePageMeta({ layout: false })`，居中卡片表单（UCard/UForm/UInput），提交后调 `adminLogin`，成功 `navigateTo('/dashboard')`，失败展示 ApiError message 并停留。

### D6: 布局页脚改造

`layouts/default.vue` 删除"无鉴权管理面 · 请网络层隔离"文案，页脚展示当前登录管理员（username + 登出按钮，登出走 useAuth.logout）。

## Risks / Trade-offs

- [localStorage 存 JWT：XSS 可窃取令牌] → 管理界面为内网工具，SPA 无 SSR 无 cookie 注入便利；先落地，升级 httpOnly cookie 方案另立变更（CONTEXT/ADR 记录）
- [401 全量跳转可能打断并行请求的页面状态] → 跳转即全量刷新，状态自然清空；可接受，后续如需静默续期再评估
- [令牌过期后端返回 401 后跳转登录页，用户需重新输入密码] → 预期行为，无刷新令牌机制；管理界面低频操作可接受
