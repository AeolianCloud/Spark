// Package config loads service configuration from defaults, an optional
// YAML file and environment variables (in that order of precedence).
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

// Config is the root application configuration.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Crypto   CryptoConfig   `yaml:"crypto"`
	Log      LogConfig      `yaml:"log"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port int `yaml:"port"`
}

// DatabaseConfig holds the PostgreSQL connection settings.
type DatabaseConfig struct {
	DSN string `yaml:"dsn"`
}

// CryptoConfig holds application-layer encryption settings.
// EncryptionKey is a base64-encoded 32-byte AES key.
type CryptoConfig struct {
	EncryptionKey string `yaml:"encryption_key"`
}

// LogConfig holds logging settings.
type LogConfig struct {
	Level string `yaml:"level"`
}

const (
	defaultConfigPath = "config/config.yaml"
	defaultPort       = 8080
	defaultLogLevel   = "info"

	// exampleEncryptionKey is the base64 of "0123456789abcdef0123456789abcdef",
	// used in the sample config; it is not a secret.
	exampleEncryptionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
)

// Default returns a Config populated with built-in defaults.
func Default() *Config {
	return &Config{
		Server:   ServerConfig{Port: defaultPort},
		Database: DatabaseConfig{},
		Crypto:   CryptoConfig{},
		Log:      LogConfig{Level: defaultLogLevel},
	}
}

// Load builds a Config by merging defaults, an optional YAML file and
// environment variables (SPARK_*). If path is empty, config/config.yaml is
// used when it exists; a missing file is not an error.
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

// applyEnv overrides config fields from SPARK_* environment variables.
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
