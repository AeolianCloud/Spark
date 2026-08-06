# Spark Web 前端

Spark 平台（PVE 虚拟化后端）的 Web 管理界面，基于 **Nuxt 4** + **Nuxt UI v4** 构建。

## 技术栈与约束

- Node.js >= 22.19.0，包管理器 npm（版本锁定见 `package.json` 的 `engines` 与 `packageManager`）
- 所有界面文案为中文，代码注释为中文
- API 契约以仓库根 `docs/openapi.yaml` 为唯一事实来源，前端 client 由契约生成（见任务 2 批次）
- 功能页面（9 个）：Dashboard、可用区、区域节点（/zones/:zoneId/nodes）、节点总览、IP 池、存储类型、镜像、VM 列表、VM 详情；另有基础布局、导航与统一错误/加载/空态组件

## 常用命令

```bash
npm install        # 安装依赖（postinstall 自动执行 nuxt prepare）
npm run dev        # 本地开发（:3000，/api 代理到后端 :8080）
npm run generate   # 静态站点生成（SPA 预渲染产物，含 index.html 与各路由 SPA 壳，可被 nginx 静态托管）
npm run build      # 构建（SSR 关闭时产物仅含 _nuxt 资源与静态文件，无 index.html，不适用于静态托管）
npm run typecheck  # TypeScript 类型检查
npm run lint       # ESLint 检查
npm run api:gen    # 从仓库根 docs/openapi.yaml 生成 API client 类型（产物入库，禁止手改）
npm run api:check  # 重新生成 + git diff 校验（CI 用：契约变更未同步 client 时失败）
```

## 目录结构

```
web/
├── app/
│   ├── api/                 # API client（见"API Client（由契约生成）"一节）
│   ├── app.vue              # 根组件（UApp 包裹）
│   ├── app.config.ts        # Nuxt UI 主题配置
│   ├── assets/css/main.css  # Tailwind + Nuxt UI 样式入口
│   ├── components/          # 共享组件（App* 前缀，自动导入）
│   │   ├── AppErrorAlert.vue  # 后端错误统一展示（code 徽章 + message）
│   │   ├── AppLoading.vue     # 骨架屏加载态
│   │   └── AppEmpty.vue       # 空态展示
│   ├── composables/         # 组合式函数（useCatalog：Zone/节点名称映射目录）
│   ├── layouts/default.vue  # 基础布局：侧边导航 + 内容区
│   ├── pages/               # 功能页面（9 个：Dashboard/可用区/区域节点/节点总览/IP 池/存储类型/镜像/VM 列表/VM 详情）
│   └── utils/               # 共享工具（format：时间与 VM 状态标签；vm：状态徽章与容量/时长格式化）
├── nuxt.config.ts           # Nuxt 配置（devProxy、SSR/类型模式等）
└── package.json
```

## API Client（由契约生成）

`docs/openapi.yaml` 是 API 契约的唯一事实来源（25 个端点）。前端请求层由 `openapi-typescript` 生成类型 + 自写轻量 fetch 封装组成，两层分离：

```
web/app/api/
├── generated/schema.d.ts   # 生成物（契约镜像）：openapi-typescript 输出，禁止手改
├── client.ts               # fetch 基础层：ApiError（status/code/message）、分页头与 Location 解析
├── types.ts                # 契约类型统一出口（schema 类型别名）
├── healthz.ts / zones.ts / nodes.ts / pools.ts
├── storage-types.ts / images.ts / vms.ts   # 每个资源一个模块，每端点一个类型化函数
├── contract-verify.ts      # type-level 契约校验：端点覆盖与写操作请求/响应字段一一对应
└── index.ts                # 统一出口：`import { createVM, ApiError } from '~/api'`
```

- 契约变更后执行 `npm run api:gen` 重新生成并提交生成物；生成物 diff 是契约同步的机器证据
- `contract-verify.ts` 在 `npm run typecheck` 中编译期校验：25 个端点齐全；全部写操作（createZone/createNode/createPool/createStorageType/createImage/createVM/updateNode/updateStorageType/setPoolNodes/resizeVM/destroyVM）的请求体必填字段与响应体、列表端点的 query 参数、X-Total-Count/Location 响应头均与契约 schema 一一对应（契约增删改字段时断言失败）
- 错误统一为 `ApiError`：`status`（HTTP 状态码）、`code`（契约错误码，唯一可依赖）、`message`（脱敏后的可读信息）

## CI（契约一致性保障）

仓库根 `.github/workflows/frontend.yml` 在每次 push / PR 时执行：

1. `npm ci` 安装依赖（Node 24）
2. `npm run api:check`：重新生成 client 并与已提交版本 diff，**契约变更未同步 client 时流水线失败，变更不允许合并**
3. `npm run typecheck` / `npm run lint`
4. `npm run generate`：验证静态构建产物可用

## 开发代理

`nuxt.config.ts` 中 `nitro.devProxy` 将 `/api` 代理到本地后端（`http://127.0.0.1:8080`，`changeOrigin: true`），开发环境避免 CORS。

## 生产部署

使用 `npm run generate` 生成静态站点，产物位于 `.output/public`（包含 `index.html`、各路由 SPA 壳及 `404.html`/`200.html`），由 nginx 静态托管；`/api` 反向代理到后端进程。

nginx 需配置 SPA 回退，使未知路径与历史路由均落到 `index.html`：

```nginx
location / {
    try_files $uri $uri/ /index.html;
}
```

部署细节（nginx 静态托管 + `/api` 反代配置示例、无鉴权网络隔离提示）见 [docs/web-deployment.md](../docs/web-deployment.md)。
