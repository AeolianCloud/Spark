## 1. 解析与加载

- [x] 1.1 `config/config.go` 新增纯函数 `parseDotEnv`：按行解析 `KEY=VALUE`，忽略空行与 `#` 注释，剥离单/双引号，重复键后者胜；可单测
- [x] 1.2 新增 `loadDotEnvLocal`：文件缺失静默返回 nil；解析失败行跳过；仅对未设置的环境变量执行 `os.Setenv`（进程环境优先）
- [x] 1.3 `Load` 接入：读取 config.yaml 之后、applyEnv 之前调用 `loadDotEnvLocal`；注释说明优先级链
- [x] 1.4 单测：解析规则（注释/空行/引号/重复键）、优先级（显式环境变量不被 .env.local 覆盖）、文件缺失静默

## 2. 文档同步

- [x] 2.1 README「本地敏感配置」小节改为自动加载说明（`set -a; source` 方式降级为可选说明）
- [x] 2.2 `.env.example` 头注释同步（说明自动加载与优先级）

## 3. 验证

- [x] 3.1 `go test ./config/ -count=1` 全量通过；`go build ./...`、`go vet ./...`、`gofmt` 检查
- [x] 3.2 冒烟：在仓库根直接 `go run ./cmd/server`（不手动 source）能正常启动（`.env.local` 已含合法密钥）
