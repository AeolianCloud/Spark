# Design: 自动加载 .env.local

## Context

用户反馈：`go run ./cmd/server` 启动时未手动 source `.env.local`，导致加载 config.yaml 示例值（`auth.jwt_secret: "change-me"` 9 字符）启动失败。README 目前要求手动 `set -a; source .env.local; set +a`，易踩坑。

## Goals / Non-Goals

Goals:
- 启动时自动加载仓库根目录 `.env.local`（存在时），无需手动 source
- 进程环境变量优先级高于 `.env.local`（显式设置不被覆盖）
- 纯解析逻辑可单测；文件缺失静默跳过

Non-Goals:
- 复杂 dotenv 特性：变量插值 `${VAR}`、`export` 前缀、跨行值、转义展开
- 自定义 `--env-file` 路径参数

## Decisions

### D1: 自实现轻量解析，不引入第三方依赖

`.env.local` 形态固定（本项目自己的文件），解析规则简单：按行处理，去空白后 `#` 开头或空行忽略，`KEY=VALUE` 拆分，值去除首尾引号（单/双引号各剥离一层）。不引入 godotenv——依赖面最小化，且解析规则可控（测试锁定行为，避免第三方语义差异）。

### D2: 加载位置与优先级

- `config.Load` 在读取 config.yaml **之后**、`applyEnv` 之前调用 `loadDotEnvLocal(path)`：
  - 优先级链：默认值 < config.yaml < .env.local < 进程环境变量（applyEnv）
  - 实现方式：`os.Setenv` 仅当该变量**未设置**（`os.LookupEnv` 不存在）时写入，随后 `applyEnv` 读到的永远是进程环境优先值——不需要单独维护"来自哪一层"的状态
- 根目录定位：`Load` 的 path 参数语义不变（默认 `config/config.yaml`），`.env.local` 固定在仓库根（`./.env.local`，相对工作目录）。测试通过注入临时目录 + `os.Chdir` 或直接调用纯解析函数规避

### D3: 解析函数与错误语义

- 纯函数 `parseDotEnv(content string) map[string]string`：可单测（空行/注释/引号/重复键后者胜）
- `loadDotEnvLocal(path string) error`：文件缺失 → 返回 nil（静默）；存在 → 解析并注入未设置变量；解析失败（如无 `=` 的行）→ 跳过该行不报错（宽松），除非格式错误需要报错？——宽松处理，与 .env 文件惯例一致
- 保持 `config.Load` 现有"文件缺失不算错误"的整体语义

## Risks / Trade-offs

- 自动加载隐式行为：CI 或其他环境若存在意外 `.env.local` 可能影响行为——文件仅存在于本地（gitignore），生产部署不会带；风险可接受
- `os.Setenv` 是进程级副作用：仅注入未设置变量，多次调用 `Load` 幂等；测试需清理环境变量（t.Setenv 或手动 Unsetenv）
- 工作目录依赖：`.env.local` 按相对路径查找，`go run ./cmd/server` 在仓库根执行时命中；其他工作目录不命中（静默）——文档说明

## Migration Plan

1. `config/config.go`：新增 `parseDotEnv` / `loadDotEnvLocal`，`Load` 接入
2. 单测：解析规则、优先级、缺失静默、引号剥离
3. README / .env.example 注释同步
4. 回归：`go test ./config/`、`go build ./...`

## Open Questions

- 是否需要支持 `export KEY=VALUE` 前缀（当前 .env 文件不含，不做）
