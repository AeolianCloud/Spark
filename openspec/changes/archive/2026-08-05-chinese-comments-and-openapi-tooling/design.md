## Context

需求动机见 proposal.md：存量注释与仓库中文规则不符（61 个 go 文件约 1679 行注释几乎全英文）；OpenAPI 契约（docs/openapi.yaml，3.0.3）缺 operationId、未经官方校验、无在线浏览入口。约束：注释整改必须零行为变更；契约必须与代码实现保持一致（既有 14 paths/25 operations 已核对）；本机无 Java（生成器选型受限）；npm registry 可达。

## Goals / Non-Goals

**Goals:**

- 存量 go 注释全量中文化且行为零变化，约定固化到 AGENTS.md
- openapi.yaml 通过 redocly lint 零错误、operationId 完整、可被 openapi-typescript 消费
- `GET /docs` 提供 Swagger UI 在线浏览

**Non-Goals:**

- 不改动任何业务逻辑、API 语义与数据模型（注释整改边界内）
- 不引入契约-代码双向同步的自动化（本期只做一次性校验与修复）
- 不生成前端客户端代码入库（生成仅用于验证）
- 不改动 docs/api-errors.md 内容（已与代码一致）

## Decisions

### D1: 注释翻译执行策略：按包分批 + diff 校验

按依赖顺序分 6 批翻译：pve → repository → service → api → model/database/config/crypto/cmd → e2e。每批完成后用 `git diff` 验证仅注释与空行变化（可用脚本过滤 diff 中非注释行的代码改动断言为零）。行为不变量：`go build`、`go vet`、全量测试结果与翻译前一致。

- 备选：一次性全量翻译 → 单批过大难以审查与回退；分批可逐批校验、逐批回滚。

### D2: Swagger UI 挂载方案：swaggest/swgui（纯 Go embed）

引入 `github.com/swaggest/swgui v1.8.9`（**注：swgui 模块不存在 v4 大版本**，最新版本为 v1.8.9，其 v4emb/v5emb 子包分别对应 Swagger UI 4.x/5.x；本实现采用 `v5emb`（Swagger UI 5.32.8））。将 swagger-ui 静态资源 embed 进二进制，注册 `GET /docs` 渲染页面 + `GET /openapi.yaml` 输出契约内容（embed 副本位于 api/swagger/openapi.yaml，与 docs/openapi.yaml 字节级一致，同步命令见 api/swagger/doc.go）。

- 备选：CDN 页面（零依赖但浏览需外网，内网部署不可用）；gin-contrib/swagger（依赖 swag 注释约定，本项目无 swag 注释）。选 swaggest 因为离线可用、无 swag 注释依赖、纯 Go embed 符合本仓库无前端资源目录的现状。
- 注意：/docs 与 /openapi.yaml 不参与业务路由契约（不进 openapi.yaml paths，避免契约自指）；/docs 子树由 swgui 内部处理 404，属于文档路由特例、不套用统一错误契约。

### D3: 校验器：npx @redocly/cli

开发/CI 用 `npx @redocly/cli lint docs/openapi.yaml` 执行校验；契约文件已是 3.0.3、$ref 全有效，预期仅需修复 lint 级告警（如 operationId 缺失、description 规范等）。

- 备选：swagger-cli validate（校验能力弱于 redocly lint，不输出操作级规范告警）。选 redocly 因校验更全面且是官方推荐的 OAS 3 校验器。

### D4: 生成器验证：openapi-typescript

本机无 Java，openapi-generator（Java CLI）不可用；选 `npx openapi-typescript docs/openapi.yaml` 生成 TypeScript 类型定义到临时目录（/tmp）验证，断言生成成功且输出非空。该工具 Node 原生、npm 直达，验证"契约可被生成器消费"的目的等价。

- 备选：apt 安装 Java + openapi-generator-cli（验证更强但引入系统级依赖，收益低）。已与用户确认采用 openapi-typescript。

### D5: 约定固化：AGENTS.md

仓库根 AGENTS.md 增加规则条目"代码注释一律使用中文"，与全局 AGENTS.md 的既有要求对齐，使后续变更（含 sub-agent 实施）有明确约束。

## Risks / Trade-offs

- [注释翻译量大（1600+ 行），人工翻译可能误改代码] → 分批 + 每批 git diff 断言仅注释变化 + reviewer 抽检；测试全量回归兜底。
- [翻译后中英文混杂（字符串/标识符保持英文，仅注释中文）] → 只允许翻译注释内容，标识符、字符串、错误消息一律不动，diff 校验强制。
- [redocly lint 告警较多，部分规则与现有风格冲突] → 修复 lint 级问题；若个别规则与既有契约风格冲突（如 description 长度），记录豁免理由并注释说明，不强行改契约语义。
- [swaggest/swgui 引入新 go 依赖] → 仅静态资源 embed，无运行时外部调用；版本锁定于 go.mod。
- [/docs 端点可能被扫接口工具枚举] → 本期不做鉴权（与 v1 无认证现状一致），README 记录；后续认证方案落地时一并纳入。

## Migration Plan

- 部署：注释翻译与 /docs 路由随下一版本发布；openapi.yaml 修复与 operationId 补全不改变任何端点语义，前端无感知。
- 回滚：注释翻译可整体 revert（分批提交）；/docs 路由删除一行注册即可；openapi.yaml 回退旧版即可。

## Open Questions

- 无（生成器选型、Swagger UI 方案、翻译范围均已确认；lint 的具体告警清单在实施时以实际输出为准，不改变规格与任务拆分）
