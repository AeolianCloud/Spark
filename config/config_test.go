package config

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeTempConfig 将 YAML 内容写入临时文件并返回路径：Load 在 path 为空时
// 会读取仓库 config/config.yaml，测试必须显式传路径以隔离真实配置。
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

// testJWTSecret 是满足最小长度（minJWTSecretLen）的合法测试密钥。
const testJWTSecret = "test-jwt-secret-0123456789abcdefghijklmnopqrstuv"

// baseConfigYAML 返回带 auth.jwt_secret 的最小合法配置片段，供其它测试
// 拼接：jwt_secret 为必填项且必须满足最小长度，否则 Load 会报错。
func baseConfigYAML() string {
	return "auth:\n  jwt_secret: \"" + testJWTSecret + "\"\n"
}

// TestDefaultAllowlist 验证内置默认白名单包含 5 个常见云镜像源域名。
func TestDefaultAllowlist(t *testing.T) {
	got := Default().Images.DownloadHostAllowlist
	if !slices.Equal(got, defaultImageDownloadHosts) {
		t.Fatalf("default allowlist = %v, want %v", got, defaultImageDownloadHosts)
	}
}

// TestLoadAllowlistFromEnv 验证 SPARK_IMAGES_DOWNLOAD_HOST_ALLOWLIST 逗号
// 分隔整体覆盖默认值，且条目被 trim + 小写归一。
func TestLoadAllowlistFromEnv(t *testing.T) {
	t.Setenv("SPARK_IMAGES_DOWNLOAD_HOST_ALLOWLIST", "  Cloud.Debian.ORG , images.example.com ")
	cfg, err := Load(writeTempConfig(t, baseConfigYAML()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"cloud.debian.org", "images.example.com"}
	if !slices.Equal(cfg.Images.DownloadHostAllowlist, want) {
		t.Fatalf("allowlist = %v, want %v", cfg.Images.DownloadHostAllowlist, want)
	}
}

// TestLoadAllowlistEnvEmptyRejectsAll 验证环境变量为空串时白名单为空
// （拒绝所有下载），且整体覆盖默认值。
func TestLoadAllowlistEnvEmptyRejectsAll(t *testing.T) {
	t.Setenv("SPARK_IMAGES_DOWNLOAD_HOST_ALLOWLIST", "")
	cfg, err := Load(writeTempConfig(t, baseConfigYAML()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Images.DownloadHostAllowlist) != 0 {
		t.Fatalf("allowlist = %v, want empty (reject all downloads)", cfg.Images.DownloadHostAllowlist)
	}
}

// TestLoadAllowlistFromYAML 验证 YAML 配置覆盖默认值并做小写归一。
func TestLoadAllowlistFromYAML(t *testing.T) {
	path := writeTempConfig(t, baseConfigYAML()+`
images:
  download_host_allowlist:
    - "Cloud.Debian.ORG"
    - images.example.com
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"cloud.debian.org", "images.example.com"}
	if !slices.Equal(cfg.Images.DownloadHostAllowlist, want) {
		t.Fatalf("allowlist = %v, want %v", cfg.Images.DownloadHostAllowlist, want)
	}
}

// TestLoadAllowlistYAMLNull 验证 download_host_allowlist: null 归一为空列表
// （拒绝所有下载），而非保留默认值。
func TestLoadAllowlistYAMLNull(t *testing.T) {
	path := writeTempConfig(t, baseConfigYAML()+"images:\n  download_host_allowlist: null\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Images.DownloadHostAllowlist) != 0 {
		t.Fatalf("allowlist = %v, want empty (reject all downloads)", cfg.Images.DownloadHostAllowlist)
	}
}

// TestLoadAllowlistRejectsBadEntries 验证校验拒绝空项、含 "/" 或 ":"（URL/
// 端口）以及含空白的条目。
func TestLoadAllowlistRejectsBadEntries(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"empty entry", "images:\n  download_host_allowlist:\n    - \"\"\n", "empty entries"},
		{"whitespace-only entry", "images:\n  download_host_allowlist:\n    - \"   \"\n", "empty entries"},
		{"scheme and path", "images:\n  download_host_allowlist:\n    - \"https://cloud.debian.org\"\n", "bare host"},
		{"port", "images:\n  download_host_allowlist:\n    - \"cloud.debian.org:443\"\n", "bare host"},
		{"internal whitespace", "images:\n  download_host_allowlist:\n    - \"cloud debian.org\"\n", "whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, baseConfigYAML()+tc.yaml))
			if err == nil {
				t.Fatalf("Load: want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %q, want to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoadMissingJWTSecret 验证 auth.jwt_secret 缺失时 Load 报错并拒绝启动。
func TestLoadMissingJWTSecret(t *testing.T) {
	_, err := Load(writeTempConfig(t, "server:\n  port: 8080\n"))
	if err == nil {
		t.Fatal("Load: want error for missing auth.jwt_secret, got nil")
	}
	if !strings.Contains(err.Error(), "auth.jwt_secret") {
		t.Fatalf("err = %q, want to contain auth.jwt_secret", err.Error())
	}
}

// TestLoadJWTSecretTooShort 验证 jwt_secret 短于最小长度时 Load 报错并
// 拒绝启动（G1：过短密钥可被暴力猜测）。
func TestLoadJWTSecretTooShort(t *testing.T) {
	_, err := Load(writeTempConfig(t, "auth:\n  jwt_secret: \"short-secret\"\n"))
	if err == nil {
		t.Fatal("Load: want error for short jwt_secret, got nil")
	}
	if !strings.Contains(err.Error(), "auth.jwt_secret") || !strings.Contains(err.Error(), "at least 32") {
		t.Fatalf("err = %q, want to contain auth.jwt_secret and at least 32", err.Error())
	}
}

// TestLoadJWTSecretExampleValueRejected 验证示例占位值 change-me 同样被
// 长度校验拒绝（示例配置不能直接用于生产启动）。
func TestLoadJWTSecretExampleValueRejected(t *testing.T) {
	_, err := Load(writeTempConfig(t, "auth:\n  jwt_secret: \"change-me\"\n"))
	if err == nil {
		t.Fatal("Load: want error for example jwt_secret, got nil")
	}
}

// TestLoadJWTSecretFromYAML 验证 YAML 中配置的 jwt_secret 被正确加载。
func TestLoadJWTSecretFromYAML(t *testing.T) {
	path := writeTempConfig(t, "auth:\n  jwt_secret: \"from-yaml-secret-0123456789abcdefghij\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.JWTSecret != "from-yaml-secret-0123456789abcdefghij" {
		t.Fatalf("JWTSecret = %q, want %q", cfg.Auth.JWTSecret, "from-yaml-secret-0123456789abcdefghij")
	}
}

// TestLoadJWTSecretFromEnv 验证 SPARK_AUTH_JWT_SECRET 环境变量覆盖 YAML 值。
func TestLoadJWTSecretFromEnv(t *testing.T) {
	t.Setenv("SPARK_AUTH_JWT_SECRET", "env-secret-0123456789abcdefghijklmnop")
	path := writeTempConfig(t, "auth:\n  jwt_secret: \"yaml-secret-0123456789abcdefghijklmn\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.JWTSecret != "env-secret-0123456789abcdefghijklmnop" {
		t.Fatalf("JWTSecret = %q, want %q", cfg.Auth.JWTSecret, "env-secret-0123456789abcdefghijklmnop")
	}
}

// TestParseDotEnv 验证 parseDotEnv 的解析规则：空行/注释忽略、单双引号
// 各剥离一层、无 = 的行跳过、重复键后者胜、首尾空白 trim。
func TestParseDotEnv(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    map[string]string
	}{
		{"empty content", "", map[string]string{}},
		{"comment and blank lines", "# comment\n\n  # indented comment\nFOO=1\n", map[string]string{"FOO": "1"}},
		{"double quotes stripped", "KEY=\"v\"\n", map[string]string{"KEY": "v"}},
		{"single quotes stripped", "KEY='v'\n", map[string]string{"KEY": "v"}},
		{"line without equals skipped", "no-equals-line\nKEY=1\n", map[string]string{"KEY": "1"}},
		{"duplicate key last wins", "A=1\nA=2\n", map[string]string{"A": "2"}},
		{"whitespace trimmed", "  KEY  =  value  \n", map[string]string{"KEY": "value"}},
		{"spaces inside quotes kept", "KEY=\" a b \"\n", map[string]string{"KEY": " a b "}},
		// 以下为审查补强的边界用例：行为与 design.md D1「测试锁定行为」一致。
		{"empty value", "KEY=\n", map[string]string{"KEY": ""}},
		{"value without key skipped", "=VALUE\n", map[string]string{}},
		{"empty quoted value", "KEY=\"\"\nKEY2=''\n", map[string]string{"KEY": "", "KEY2": ""}},
		// 行内注释不剥离：仅行首（trim 后）的 # 才算注释，值保留 "v # c"。
		{"inline comment kept in value", "KEY=v # c\n", map[string]string{"KEY": "v # c"}},
		// 值内含 =：按首个 = 拆分，base64 密钥（尾 =）无损。
		{"equals inside value", "KEY=a=b\n", map[string]string{"KEY": "a=b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDotEnv(tc.content)
			if !maps.Equal(got, tc.want) {
				t.Fatalf("parseDotEnv(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// TestLoadDotEnvLocalMissingFile 验证 loadDotEnvLocal 对不存在的文件
// 返回 nil（静默跳过）。
func TestLoadDotEnvLocalMissingFile(t *testing.T) {
	if err := loadDotEnvLocal(filepath.Join(t.TempDir(), "not-exist.env")); err != nil {
		t.Fatalf("loadDotEnvLocal(missing) = %v, want nil", err)
	}
}

// TestLoadDotEnvLocalKeepsProcessEnv 验证进程环境变量优先：SPARK_AUTH_JWT_SECRET
// 已在进程环境中设置时，.env.local 中的同名键不覆盖（loadDotEnvLocal 只注入
// 未设置的变量）。
func TestLoadDotEnvLocalKeepsProcessEnv(t *testing.T) {
	t.Setenv("SPARK_AUTH_JWT_SECRET", "process-secret-0123456789abcdefghijklmnop")
	path := filepath.Join(t.TempDir(), ".env.local")
	if err := os.WriteFile(path, []byte("SPARK_AUTH_JWT_SECRET=file-secret-0123456789abcdefghijklmnop\n"), 0o600); err != nil {
		t.Fatalf("write .env.local: %v", err)
	}
	if err := loadDotEnvLocal(path); err != nil {
		t.Fatalf("loadDotEnvLocal: %v", err)
	}
	if got := os.Getenv("SPARK_AUTH_JWT_SECRET"); got != "process-secret-0123456789abcdefghijklmnop" {
		t.Fatalf("SPARK_AUTH_JWT_SECRET = %q, want process env value preserved", got)
	}
}

// TestLoadAppliesDotEnvLocal 验证 Load 完整链路：工作目录存在 .env.local 时，
// 其中的 SPARK_SERVER_PORT 注入进程环境并经过 applyEnv 生效。yaml 中显式写
// port 8081（区别于默认值 8080），断言最终生效的是 .env.local 的 9099——
// 锁定「config.yaml < .env.local」环节（spec：默认值 → config.yaml →
// .env.local → 进程环境变量）。通过 os.Chdir 切到临时目录构造 .env.local；
// 本测试不使用 t.Parallel，因此修改全局工作目录是安全的，defer 恢复原目录。
func TestLoadAppliesDotEnvLocal(t *testing.T) {
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	defer func() {
		if err := os.Chdir(origDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}()

	// 清理可能预先存在的 SPARK_SERVER_PORT（避免 .env.local 注入被跳过
	// 或 applyEnv 读到无关值），结束时恢复原值。
	origPort, hadPort := os.LookupEnv("SPARK_SERVER_PORT")
	os.Unsetenv("SPARK_SERVER_PORT")
	defer func() {
		os.Unsetenv("SPARK_SERVER_PORT")
		if hadPort {
			os.Setenv("SPARK_SERVER_PORT", origPort)
		}
	}()

	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	if err := os.WriteFile(".env.local", []byte("SPARK_SERVER_PORT=9099\n"), 0o600); err != nil {
		t.Fatalf("write .env.local: %v", err)
	}
	// 显式在 yaml 中写 port（8081），与默认值 8080 区分：只有 .env.local 覆盖
	// yaml 时最终 port 才是 9099。
	cfg, err := Load(writeTempConfig(t, strings.Replace(baseConfigYAML(), "port: 8080", "port: 8081", 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 9099 {
		t.Fatalf("port = %d, want 9099 (from .env.local, overriding yaml 8081)", cfg.Server.Port)
	}
}
