// Package crypto provides application-layer symmetric encryption
// (AES-256-GCM) for sensitive fields such as VM cloud-init passwords.
// The key is loaded from config.Crypto.EncryptionKey (base64-encoded 32
// bytes). Ciphertext format: base64(nonce || ciphertext).
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

// Errors returned by Decrypt. ErrInvalidCiphertext means the payload is not
// well-formed (bad base64 or shorter than the nonce); ErrDecryptFailed means
// the GCM authentication tag did not match (wrong key or corrupted data).
var (
	ErrInvalidCiphertext = errors.New("crypto: invalid ciphertext")
	ErrDecryptFailed     = errors.New("crypto: decryption failed: wrong key or corrupted data")
)

// aesKeySize is the required AES-256 key length in bytes.
const aesKeySize = 32

// Cipher encrypts and decrypts values with a single AES-256-GCM key.
type Cipher struct {
	key []byte
}

// NewCipher builds a Cipher from the configured encryption key. The key is
// decoded from base64 and must decode to exactly 32 bytes.
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

// Encrypt encrypts plaintext and returns base64(nonce || ciphertext).
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	return Encrypt(plaintext, c.key)
}

// Decrypt reverses Encrypt.
func (c *Cipher) Decrypt(ciphertext string) (string, error) {
	return Decrypt(ciphertext, c.key)
}

// Encrypt encrypts plaintext with key (exactly 32 bytes) and returns
// base64(nonce || ciphertext). A fresh random nonce is used per call.
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

// Decrypt reverses Encrypt. It returns ErrInvalidCiphertext for malformed
// payloads and ErrDecryptFailed when the authentication check fails.
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

// newGCM builds an AES-256-GCM AEAD from a 32-byte key.
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
