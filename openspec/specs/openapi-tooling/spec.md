## Purpose

让 OpenAPI 契约文档成为可被工具链可靠消费的契约源：通过官方校验器零错误校验、operationId 完整、可被生成器消费，并提供在线浏览入口。

## Requirements

### Requirement: 契约可通过官方校验器

OpenAPI 契约文档 SHALL 通过官方校验器（redocly lint）的完整校验，结果为零错误；任何违反 OpenAPI 3.0 规范的条目 SHALL 被修复后通过校验。

#### Scenario: 校验零错误

- **WHEN** 对契约文档执行 redocly lint 校验
- **THEN** 校验结果为零错误（warning 级问题允许存在，须有明确处理说明）

### Requirement: 操作标识完整

契约中的每一个操作 SHALL 具有唯一且语义化的 operationId（含健康检查端点），供代码生成器与文档工具消费。

#### Scenario: 全部操作有 operationId

- **WHEN** 枚举契约的全部操作
- **THEN** 每个操作均存在唯一 operationId，无遗漏

### Requirement: 契约可被生成器消费

契约文档 SHALL 可被 openapi-typescript 生成器成功消费并产出 TypeScript 类型定义，证明契约满足常见工具链的消费要求。

#### Scenario: 生成器成功生成

- **WHEN** 对契约文档执行 openapi-typescript 生成
- **THEN** 生成成功且产出合法的 TypeScript 类型定义（生成物仅用于验证，不落入仓库）

### Requirement: 在线浏览入口

系统 SHALL 提供 `GET /docs` 端点返回 Swagger UI 页面，供人工在线浏览契约文档。

#### Scenario: 访问契约浏览页

- **WHEN** 用户请求 `GET /docs`
- **THEN** 系统返回可交互的 Swagger UI 页面，可浏览全部端点与 schema
