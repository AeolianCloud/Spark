package crypto

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"spark/config"
)

// testKey 是确定性的 32 字节 AES 密钥（字节 0..31）。
var testKey = func() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}()

func TestEncryptDecryptRoundTrip(t *testing.T) {
	plaintext := "p@ssw0rd-中文-!@#$"

	ct, err := Encrypt(plaintext, testKey)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if ct == "" || ct == plaintext {
		t.Fatalf("ciphertext should be non-empty and differ from plaintext")
	}

	got, err := Decrypt(ct, testKey)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("Decrypt = %q, want %q", got, plaintext)
	}
}

func TestDecryptWrongKey(t *testing.T) {
	ct, err := Encrypt("secret", testKey)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	wrong := make([]byte, 32)
	wrong[0] = 0xFF
	if _, err := Decrypt(ct, wrong); !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("Decrypt with wrong key: err = %v, want ErrDecryptFailed", err)
	}
}

func TestDecryptTamperedCiphertext(t *testing.T) {
	ct, err := Encrypt("secret", testKey)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}
	raw[len(raw)-1] ^= 0x01 // 翻转一个密文字节，标签校验必须失败

	tampered := base64.StdEncoding.EncodeToString(raw)
	if _, err := Decrypt(tampered, testKey); !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("Decrypt tampered: err = %v, want ErrDecryptFailed", err)
	}
}

func TestDecryptInvalidCiphertext(t *testing.T) {
	for _, in := range []string{"", "not-base64!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := Decrypt(in, testKey); !errors.Is(err, ErrInvalidCiphertext) {
			t.Fatalf("Decrypt(%q): err = %v, want ErrInvalidCiphertext", in, err)
		}
	}
}

func TestEncryptUniqueNonce(t *testing.T) {
	a, err := Encrypt("same plaintext", testKey)
	if err != nil {
		t.Fatalf("Encrypt a: %v", err)
	}
	b, err := Encrypt("same plaintext", testKey)
	if err != nil {
		t.Fatalf("Encrypt b: %v", err)
	}
	if a == b {
		t.Fatalf("two encryptions of the same plaintext produced identical ciphertexts (nonce reuse)")
	}
}

func TestInvalidKeySizes(t *testing.T) {
	if _, err := Encrypt("x", nil); err == nil {
		t.Fatal("Encrypt with nil key should fail")
	}
	if _, err := Encrypt("x", []byte("too-short")); err == nil {
		t.Fatal("Encrypt with short key should fail")
	}
}

func TestNewCipherFromConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Crypto.EncryptionKey = base64.StdEncoding.EncodeToString(testKey)

	c, err := NewCipher(cfg)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	ct, err := c.Encrypt("hello")
	if err != nil {
		t.Fatalf("Cipher.Encrypt: %v", err)
	}
	got, err := c.Decrypt(ct)
	if err != nil {
		t.Fatalf("Cipher.Decrypt: %v", err)
	}
	if got != "hello" {
		t.Fatalf("round trip = %q, want %q", got, "hello")
	}
}

func TestNewCipherInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"not base64", "!!!not-base64!!!"},
		{"wrong length", base64.StdEncoding.EncodeToString([]byte("short-key"))},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Crypto.EncryptionKey = tc.key
			if _, err := NewCipher(cfg); err == nil {
				t.Fatalf("NewCipher with key %q should fail", tc.key)
			}
		})
	}

	if _, err := NewCipher(nil); err == nil {
		t.Fatal("NewCipher(nil) should fail")
	}
}

func TestCipherErrorWrappingMentionsCause(t *testing.T) {
	cfg := config.Default()
	cfg.Crypto.EncryptionKey = base64.StdEncoding.EncodeToString(testKey)
	c, err := NewCipher(cfg)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	if _, err := c.Decrypt("garbage"); err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("Decrypt(garbage) error should mention the base64 cause, got %v", err)
	}
}
