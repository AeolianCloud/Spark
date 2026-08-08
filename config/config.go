// Package config 从默认值、可选的 YAML 文件和环境变量加载服务配置
// （按此优先级顺序）。
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Config 是应用根配置。
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Crypto   CryptoConfig   `yaml:"crypto"`
	Auth     AuthConfig     `yaml:"auth"`
	Log      LogConfig      `yaml:"log"`
	Images   ImagesConfig   `yaml:"images"`
}

// ServerConfig 保存 HTTP 服务器设置。
type ServerConfig struct {
	Port int `yaml:"port"`
}

// DatabaseConfig 保存 PostgreSQL 连接设置。
type DatabaseConfig struct {
	DSN string `yaml:"dsn"`
}

// CryptoConfig 保存应用层加密设置。
// EncryptionKey 是 base64 编码的 32 字节 AES 密钥。
type CryptoConfig struct {
	EncryptionKey string `yaml:"encryption_key"`
}

// AuthConfig 保存认证鉴权（JWT）设置。
// JWTSecret 是签发与校验 JWT 的 HMAC-SHA256 密钥，必填，且至少
// minJWTSecretLen 个字符（过短密钥可被暴力猜测，拒绝启动）。
type AuthConfig struct {
	JWTSecret string `yaml:"jwt_secret"`
}

// LogConfig 保存日志设置。
type LogConfig struct {
	Level string `yaml:"level"`
}

// ImagesConfig 保存镜像功能（镜像登记与下载）设置。
type ImagesConfig struct {
	// DownloadHostAllowlist 是镜像下载源域名白名单：镜像 download_url 的
	// host（忽略端口，精确匹配）必须命中该列表才受理镜像创建与下载。
	// 空列表语义为拒绝所有下载（SSRF 面最小化）；常见云镜像源已内置在
	// Default 中，生产部署可通过 config.yaml 或环境变量覆盖。
	DownloadHostAllowlist []string `yaml:"download_host_allowlist"`
}

const (
	defaultConfigPath = "config/config.yaml"
	defaultPort       = 8080
	defaultLogLevel   = "info"

	// exampleEncryptionKey 是 "0123456789abcdef0123456789abcdef" 的 base64 编码，
	// 用于示例配置；它不是机密。
	exampleEncryptionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	// exampleJWTSecret 是示例配置中的占位 JWT 密钥（config/config.yaml）；
	// 它不是机密，且长度不满足 minJWTSecretLen，配置示例会先触发警告再被
	// 长度校验拒绝，引导用户生成随机密钥。
	exampleJWTSecret = "change-me"
	// minJWTSecretLen 是 JWT 密钥的最小字符数下限。openssl rand -base64 32
	// 输出约 44 字符，32 字符下限（≈256 位随机熵）足以防御暴力猜测。
	minJWTSecretLen = 32
)

// defaultImageDownloadHosts 是镜像下载源的默认域名白名单（常见云镜像源）。
// 下载请求最终由 PVE 节点代发（SSRF 的受害方），仅允许向这些受信源下载
// 镜像文件；空列表语义为拒绝所有下载。
var defaultImageDownloadHosts = []string{
	"cloud.debian.org",
	"cloud-images.ubuntu.com",
	"cloud.centos.org",
	"download.cirros-cloud.net",
	"cloud-images.rockylinux.org",
}

// Default 返回一个填充了内置默认值的 Config。
func Default() *Config {
	return &Config{
		Server:   ServerConfig{Port: defaultPort},
		Database: DatabaseConfig{},
		Crypto:   CryptoConfig{},
		Auth:     AuthConfig{},
		Log:      LogConfig{Level: defaultLogLevel},
		// 内置常见云镜像源；生产通过 config.yaml / 环境变量覆盖。
		Images: ImagesConfig{DownloadHostAllowlist: append([]string(nil), defaultImageDownloadHosts...)},
	}
}

// Load 通过合并默认值、可选的 YAML 文件和环境变量（SPARK_*）构建 Config。
// 如果 path 为空，当 config/config.yaml 存在时使用它；文件缺失不算错误。
func Load(path string) (*Config, error) {
	cfg := Default()

	if path == "" {
		path = defaultConfigPath
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	if err := applyEnv(cfg); err != nil {
		return nil, err
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnv 使用 SPARK_* 环境变量覆盖配置字段。
func applyEnv(cfg *Config) error {
	if v, ok := os.LookupEnv("SPARK_SERVER_PORT"); ok {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: SPARK_SERVER_PORT must be an integer, got %q", v)
		}
		cfg.Server.Port = port
	}
	if v, ok := os.LookupEnv("SPARK_DATABASE_DSN"); ok {
		cfg.Database.DSN = v
	}
	if v, ok := os.LookupEnv("SPARK_CRYPTO_ENCRYPTION_KEY"); ok {
		cfg.Crypto.EncryptionKey = v
	}
	if v, ok := os.LookupEnv("SPARK_AUTH_JWT_SECRET"); ok {
		cfg.Auth.JWTSecret = v
	}
	if v, ok := os.LookupEnv("SPARK_LOG_LEVEL"); ok {
		cfg.Log.Level = v
	}
	// 逗号分隔覆盖镜像下载源白名单；空值/空列表语义为拒绝所有下载。
	if v, ok := os.LookupEnv("SPARK_IMAGES_DOWNLOAD_HOST_ALLOWLIST"); ok {
		cfg.Images.DownloadHostAllowlist = nil
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				cfg.Images.DownloadHostAllowlist = append(cfg.Images.DownloadHostAllowlist, h)
			}
		}
	}
	return nil
}

func validate(cfg *Config) error {
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return fmt.Errorf("config: invalid server port %d", cfg.Server.Port)
	}
	if cfg.Crypto.EncryptionKey == exampleEncryptionKey {
		slog.Warn("config: crypto.encryption_key is the example value; generate a random key with: openssl rand -base64 32")
	}
	if cfg.Crypto.EncryptionKey != "" {
		key, err := base64.StdEncoding.DecodeString(cfg.Crypto.EncryptionKey)
		if err != nil {
			return fmt.Errorf("config: crypto.encryption_key is not valid base64: %w", err)
		}
		if len(key) != 32 {
			return fmt.Errorf("config: crypto.encryption_key must decode to 32 bytes, got %d", len(key))
		}
	}
	// jwt_secret 必填且至少 minJWTSecretLen 字符：缺失或过短说明配置不完整，
	// 拒绝启动以避免 JWT 密钥静默为空或被暴力猜测。示例占位值（change-me）
	// 先打警告（与 encryption_key 的 example 警告同风格），提示生成随机密钥；
	// 该值长度不足，随后仍会被长度校验拒绝。
	if cfg.Auth.JWTSecret == exampleJWTSecret {
		slog.Warn("config: auth.jwt_secret is the example value; generate a random key with: openssl rand -base64 32")
	}
	if len(cfg.Auth.JWTSecret) < minJWTSecretLen {
		return fmt.Errorf("config: auth.jwt_secret must be at least %d characters, got %d", minJWTSecretLen, len(cfg.Auth.JWTSecret))
	}
	// 白名单项归一：trim + 小写（域名大小写不敏感），拒绝空项、含 "/" 或
	// ":"（防误填 URL/路径/端口）及含空白（防误填多余空格）的条目；归一
	// 后的值写回 cfg。空列表本身合法，语义为拒绝所有下载（service 层体现）。
	clean := make([]string, 0, len(cfg.Images.DownloadHostAllowlist))
	for _, h := range cfg.Images.DownloadHostAllowlist {
		if h = strings.ToLower(strings.TrimSpace(h)); h == "" {
			return fmt.Errorf("config: images.download_host_allowlist must not contain empty entries")
		}
		if strings.ContainsAny(h, "/:") {
			return fmt.Errorf("config: images.download_host_allowlist entry %q must be a bare host name (no scheme, path or port)", h)
		}
		if strings.IndexFunc(h, unicode.IsSpace) >= 0 {
			return fmt.Errorf("config: images.download_host_allowlist entry %q must not contain whitespace", h)
		}
		clean = append(clean, h)
	}
	cfg.Images.DownloadHostAllowlist = clean
	return nil
}
