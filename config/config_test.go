package config

import (
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
	cfg, err := Load(writeTempConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"cloud.debian.org", "images.example.com"}
	if !slices.Equal(cfg.Images.DownloadHostAllowlist, want) {
		t.Fatalf("allowlist = %v, want %v", cfg.Images.DownloadHostAllowlist, want)
	}
}

// TestLoadAllowlistEnvEmptyRejectsAll 验证环境变量为空串时白名单为空
//（拒绝所有下载），且整体覆盖默认值。
func TestLoadAllowlistEnvEmptyRejectsAll(t *testing.T) {
	t.Setenv("SPARK_IMAGES_DOWNLOAD_HOST_ALLOWLIST", "")
	cfg, err := Load(writeTempConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Images.DownloadHostAllowlist) != 0 {
		t.Fatalf("allowlist = %v, want empty (reject all downloads)", cfg.Images.DownloadHostAllowlist)
	}
}

// TestLoadAllowlistFromYAML 验证 YAML 配置覆盖默认值并做小写归一。
func TestLoadAllowlistFromYAML(t *testing.T) {
	path := writeTempConfig(t, `
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
//（拒绝所有下载），而非保留默认值。
func TestLoadAllowlistYAMLNull(t *testing.T) {
	path := writeTempConfig(t, "images:\n  download_host_allowlist: null\n")
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
			_, err := Load(writeTempConfig(t, tc.yaml))
			if err == nil {
				t.Fatalf("Load: want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %q, want to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
