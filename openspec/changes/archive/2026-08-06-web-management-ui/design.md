## Context

后端已具备完整管理面 API（约 25 个端点，`docs/openapi.yaml` 为权威契约，redocly 校验零错误），VM 实时状态为透传查询（ADR 0002，每次列表请求向 PVE 扇出），节点 API 令牌加密落库且只写不读（ADR 0004）。仓库目前纯 Go，无任何前端。动机见 proposal.md - Why。

## Goals / Non-Goals

**Goals:**
- 让 `web/` 子目录的 Nuxt 前端成为后端全功能的可视化管理面
- 前端与后端通过 OpenAPI 契约对齐（生成 client），契约即合同
- 构建产物可被 nginx 静态托管并反代 `/api`

**Non-Goals:**
- 第一批不做登录/鉴权（已知权衡，后续独立变更）
- 不做面向客户的云控制台（客户面将来另起）
- 不改动任何后端 API 行为与路由

## Decisions

### D1: 前端放仓库内 `web/` 子目录，而非独立仓库
理由：契约同步零摩擦（一个 PR 可同时改契约、后端与前端），CI 可在同一流水线生成 client 并 diff 校验。独立仓库需要额外的契约同步工具链，收益仅为仓库隔离。
替代方案：独立 `spark-web` 仓库 —— 否决，协同成本高。

### D2: 框架选型 Nuxt 4 + Nuxt UI v4
理由：管理面是表格/表单密集应用，Nuxt UI 提供开箱即用的 DataTable、Form、Toast 等组件，开发效率高；Nuxt 的 SSR/静态托管对内部工具无负担，dev proxy 配置简单。
替代方案：React + Ant Design —— 团队工具链（环境已具备 nuxt-ui 技能）倾向 Vue 系，选 Nuxt 减少心智负担。

### D3: API client 从 `docs/openapi.yaml` 生成（生成物入库）
理由：契约是权威源（contract-first），生成 client 使契约变更在编译期暴露；写操作（增删改）契约未同步时，生成后类型/接口缺失或漂移会立刻被 diff/编译发现，将"接口契约同步红线"（AGENTS.md 已固化）机械化。
生成工具在 orval 与 openapi-typescript 之间选型（见 Open Questions，apply 阶段定），两者均支持 TS 类型 + 请求封装。
生成物入库（提交到 git）而非仅构建期生成：保证任意时刻仓库可独立构建、diff 可审查。

### D4: 第一批不加鉴权
理由：内部工具优先跑通功能闭环（用户决策）。
约束：部署方须网络层隔离；界面不做任何登录交互。鉴权作为独立 change 在后续引入，届时同步更新契约 security 声明。

### D5: VM 状态展示采用"手动刷新 + 低频轮询"，不做秒级自动刷新
理由：VM 状态为透传查询（ADR 0002），每次列表请求对每台 VM 扇出一次 PVE 调用；高频轮询会把扇出放大。默认手动刷新，可选 ≥10s 低频自动刷新。
PVE 不可达降级时（后端返回静态字段），界面展示降级提示而非伪造状态（见 specs - VM 列表与详情）。

### D6: 分页遵循后端契约
VM 列表使用 limit/offset 参数与 `X-Total-Count` 响应头计算总页数；列表页状态（页码、每页条数）为 URL query 驱动，便于刷新后保持。

### D7: 令牌与密码只写不读
节点 API 令牌与 VM 注入密码均为一次性输入：提交后前端不持久化、不缓存、不回显（与 ADR 0004 只写语义一致）。

### D8: 开发与生产拓扑
开发：Nuxt `devProxy` 将 `/api` 代理到本地后端（:8080），避免 CORS。
生产：nginx 托管 `web/` 构建产物（静态），`/api` 反向代理到后端进程；不引入后端静态 embed。

## Risks / Trade-offs

- [写接口契约未同步 → 前端 client 静默过期] → AGENTS.md 红线 + 生成物入库 diff 审查 + CI 校验；契约先行，代码先行不被合并
- [node 生态首次进入仓库，构建环境变化] → package.json/lockfile 锁定版本，CI 引入 node 构建步骤（与现有 Go 流水线并行）
- [VM 列表扇出放大] → 刷新策略 D5 限制频率；PVE 不可达时依赖后端降级行为
- [无鉴权暴露管理面] → 网络层隔离（部署文档明示）；鉴权为后续独立 change，设计上预留契约 security 扩展位
- [生成代码风格与手写风格不一致] → 接受生成物为契约镜像，以契约为准，禁止手改生成文件

## Migration Plan

1. 新增 `web/` 目录与 Nuxt 脚手架，配置 devProxy
2. 配置 client 生成工具，首次生成并入库（校验与契约一致）
3. 按资源实现页面（先基础设施，后 VM 生命周期）
4. 本地联调（后端 + 前端 + fake PVE / 真实 PVE），e2e 后端测试保持通过
5. 部署文档补充 nginx 静态托管与 `/api` 反代说明
6. 回滚：不引入任何后端变更，仅移除 `web/` 目录即恢复原状

## Open Questions

- client 生成工具选型：orval（更完整的请求封装/验证）vs openapi-typescript（更薄、纯类型）——apply 阶段按生成效果定，不影响 spec 与任务划分
- Node 版本与包管理器（pnpm/npm）锁定——脚手架初始化时定
