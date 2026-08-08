## Why

目前 `.env.local` 需要手动 `set -a; source .env.local; set +a` 才生效，否则 `go run ./cmd/server` 会使用 `config/config.yaml` 中的示例值（如 `auth.jwt_secret: "change-me"`）导致启动失败，或加载错误的密钥。新手与本地开发极易踩坑。

## What Changes

- `config.Load` 启动时自动加载仓库根目录的 `.env.local`（若存在）：解析 `KEY=VALUE` 行（支持 `#` 注释、忽略空行），仅对**尚未设置**的环境变量执行注入（显式设置的环境变量优先级更高）
- 加载逻辑封装为独立函数（如 `loadDotEnvLocal`），纯解析逻辑可单测；文件缺失时静默跳过（不报错，保持 `config.Load` 现有"文件缺失不算错误"语义）
- 不引入第三方依赖（自实现轻量解析，覆盖本项目 .env 文件的简单键值形态）
- 文档同步：README「本地敏感配置」小节改为自动加载说明；`.env.example` 头注释同步
- **不在本次范围**：变量插值（`${VAR}`）、导出标记（`export`）、引号转义等复杂 dotenv 特性；`--env-file` 参数

## Capabilities

### Modified Capabilities
- `openapi-tooling` 与配置加载无直接 capability 归属，本变更归属 `config` 加载行为——无既有 capability 覆盖配置加载，新增 `config-loading` capability 描述配置加载行为（默认值 → config.yaml → .env.local → 环境变量）

## Impact

- **后端**：`config/config.go`（`Load` 调用点 + 新增解析函数）、`config/config_test.go`（解析与优先级单测）
- **文档**：`README.md`、`.env.example` 注释
- **测试**：单测覆盖解析（注释/空行/引号剥离/重复键）、优先级（显式环境变量不被覆盖）、文件缺失静默
