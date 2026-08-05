## Why

仓库 AGENTS.md 与全局规则均要求"代码注释一律使用中文"，但现有 61 个 go 文件约 1679 行注释中仅 43 行含中文，存量实现几乎全英文注释，与既定规则不符；同时 OpenAPI 契约文档（docs/openapi.yaml）从未经过官方校验器验证、缺少完整的 operationId、无在线浏览入口，无法被工具可靠消费。

## What Changes

- 全部 go 代码注释（含测试与 e2e）翻译为中文，纯注释改动、零行为变更
- 仓库 AGENTS.md 固化"代码注释一律使用中文"约定，后续所有变更持续遵守
- docs/openapi.yaml 补全 `/healthz` 的 operationId，保证每个操作有唯一标识
- 引入官方校验器（redocly lint）对契约做结构校验，修复全部告警，保证契约零错误可校验
- 用 openapi-typescript 生成 TypeScript 类型定义验证契约可被生成器消费（生成物不落入仓库）
- 新增 `GET /docs` 路由挂载 Swagger UI，提供契约在线浏览入口
- README 补充 /docs 入口与契约校验、生成命令说明

## Capabilities

### New Capabilities

- `code-comments-chinese`: 全部 go 代码注释与说明使用中文，并持续保持该约定
- `openapi-tooling`: OpenAPI 契约可通过官方校验器零错误校验、operationId 完整、可被生成器消费、提供在线浏览入口

### Modified Capabilities

（无，现有能力的行为契约不变，本变更只涉及注释语言、工具链与文档形态）

## Impact

- 代码：61 个 go 文件注释翻译（pve/repository/service/api/model/database/config/crypto/cmd/e2e），不改变任何逻辑
- 配置/约定：AGENTS.md 新增注释语言规则
- 契约文档：docs/openapi.yaml（operationId 补全与 lint 修复）
- 新增依赖：go 模块 `swaggest/swgui`（Swagger UI 静态资源 embed）；工具链依赖 npx `@redocly/cli`、`openapi-typescript`（仅开发/CI 使用，不进运行时）
- 新增端点：`GET /docs`（Swagger UI 在线浏览）
