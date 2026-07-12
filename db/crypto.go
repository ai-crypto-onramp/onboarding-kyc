package db

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// ---------------------------------------------------------------------------
// Column-level encryption helpers (envelope encryption via AES-256-GCM).
//
// The master key is read from the ENCRYPTION_KEY env var (32 bytes, base64 or
// hex encoded) or derived from a passphrase via SHA-256 for dev/test use. A
// fresh random key is generated on the fly if no env var is set, which is
// suitable only for tests (data will not survive a restart).
// ---------------------------------------------------------------------------

// Encryptor provides AES-256-GCM encryption/decryption for sensitive columns.
type Encryptor struct {
	aead cipher.AEAD
}

// NewEncryptor creates an Encryptor from a 32-byte key.
func NewEncryptor(key []byte) (*Encryptor, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	return &Encryptor{aead: aead}, nil
}

// NewEncryptorFromEnv loads the encryption key from ENCRYPTION_KEY (base64 or
// hex) or derives one from ENCRYPTION_PASSPHRASE via SHA-256. If neither is set,
// a random key is generated (test-only; data won't persist across restarts).
func NewEncryptorFromEnv() (*Encryptor, error) {
	if v := os.Getenv("ENCRYPTION_KEY"); v != "" {
		if key, err := base64.StdEncoding.DecodeString(v); err == nil && len(key) == 32 {
			return NewEncryptor(key)
		}
		if key, err := hex.DecodeString(v); err == nil && len(key) == 32 {
			return NewEncryptor(key)
		}
	}
	if v := os.Getenv("ENCRYPTION_PASSPHRASE"); v != "" {
		sum := sha256.Sum256([]byte(v))
		return NewEncryptor(sum[:])
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	return NewEncryptor(key)
}

// Encrypt encrypts plaintext and returns nonce||ciphertext as a byte slice.
func (e *Encryptor) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := e.aead.Seal(nil, nonce, plaintext, nil)
	return append(nonce, ciphertext...), nil
}

// Decrypt decrypts a nonce||ciphertext blob produced by Encrypt.
func (e *Encryptor) Decrypt(blob []byte) ([]byte, error) {
	ns := e.aead.NonceSize()
	if len(blob) < ns+1 {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ciphertext := blob[:ns], blob[ns:]
	return e.aead.Open(nil, nonce, ciphertext, nil)
}

// EncryptString is a convenience wrapper returning base64-encoded ciphertext.
func (e *Encryptor) EncryptString(plaintext string) (string, error) {
	blob, err := e.Encrypt([]byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(blob), nil
}

// DecryptString decodes a base64-encoded ciphertext and returns the plaintext.
func (e *Encryptor) DecryptString(encoded string) (string, error) {
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	pt, err := e.Decrypt(blob)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}