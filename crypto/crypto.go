// Package crypto 提供应用层对称加密（AES-256-GCM），用于加密敏感字段
// （如虚拟机 cloud-init 密码）。密钥从 config.Crypto.EncryptionKey 加载
// （base64 编码的 32 字节）。密文格式：base64(nonce || ciphertext)。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"spark/config"
)

// Decrypt 返回的错误。ErrInvalidCiphertext 表示载荷格式不正确（base64 无效
// 或长度小于 nonce）；ErrDecryptFailed 表示 GCM 认证标签不匹配（密钥错误
// 或数据损坏）。
var (
	ErrInvalidCiphertext = errors.New("crypto: invalid ciphertext")
	ErrDecryptFailed     = errors.New("crypto: decryption failed: wrong key or corrupted data")
)

// aesKeySize 是 AES-256 要求的密钥长度（字节）。
const aesKeySize = 32

// Cipher 使用单个 AES-256-GCM 密钥加密和解密值。
type Cipher struct {
	key []byte
}

// NewCipher 根据配置的加密密钥构建 Cipher。密钥从 base64 解码，
// 且必须正好解码为 32 字节。
func NewCipher(cfg *config.Config) (*Cipher, error) {
	if cfg == nil {
		return nil, errors.New("crypto: nil config")
	}
	key, err := base64.StdEncoding.DecodeString(cfg.Crypto.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode encryption key: %w", err)
	}
	if len(key) != aesKeySize {
		return nil, fmt.Errorf("crypto: encryption key must decode to %d bytes, got %d", aesKeySize, len(key))
	}
	return &Cipher{key: key}, nil
}

// Encrypt 加密明文并返回 base64(nonce || ciphertext)。
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	return Encrypt(plaintext, c.key)
}

// Decrypt 是 Encrypt 的逆操作。
func (c *Cipher) Decrypt(ciphertext string) (string, error) {
	return Decrypt(ciphertext, c.key)
}

// Encrypt 使用密钥（恰好 32 字节）加密明文并返回 base64(nonce || ciphertext)。
// 每次调用都会生成全新的随机 nonce。
func Encrypt(plaintext string, key []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("crypto: generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 是 Encrypt 的逆操作。对格式错误的载荷返回 ErrInvalidCiphertext，
// 认证检查失败时返回 ErrDecryptFailed。
func Decrypt(ciphertext string, key []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCiphertext, err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", fmt.Errorf("%w: payload shorter than nonce (%d < %d)", ErrInvalidCiphertext, len(raw), nonceSize)
	}
	nonce, sealed := raw[:nonceSize], raw[nonceSize:]
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDecryptFailed, err)
	}
	return string(plain), nil
}

// newGCM 从 32 字节的密钥构建 AES-256-GCM AEAD。
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: %w", err)
	}
	return gcm, nil
}
