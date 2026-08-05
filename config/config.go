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

	"gopkg.in/yaml.v3"
)

// Config 是应用根配置。
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Crypto   CryptoConfig   `yaml:"crypto"`
	Log      LogConfig      `yaml:"log"`
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

// LogConfig 保存日志设置。
type LogConfig struct {
	Level string `yaml:"level"`
}

const (
	defaultConfigPath = "config/config.yaml"
	defaultPort       = 8080
	defaultLogLevel   = "info"

	// exampleEncryptionKey 是 "0123456789abcdef0123456789abcdef" 的 base64 编码，
	// 用于示例配置；它不是机密。
	exampleEncryptionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
)

// Default 返回一个填充了内置默认值的 Config。
func Default() *Config {
	return &Config{
		Server:   ServerConfig{Port: defaultPort},
		Database: DatabaseConfig{},
		Crypto:   CryptoConfig{},
		Log:      LogConfig{Level: defaultLogLevel},
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
	if v, ok := os.LookupEnv("SPARK_LOG_LEVEL"); ok {
		cfg.Log.Level = v
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
	return nil
}
