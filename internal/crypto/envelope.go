// Package crypto provides column-level envelope encryption helpers for PII
// fields stored in the database. An envelope cipher encrypts plaintext with a
// random data key using AES-256-GCM, then wraps (encrypts) the data key with a
// master key also using AES-256-GCM. The ciphertext and wrapped key are
// returned together so the plaintext can be recovered only by a caller with
// the master key. This keeps the master key off the data plane: only the
// wrapped data key is persisted alongside the ciphertext.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MasterKeyLen is the required length of an AES-256 master key.
const MasterKeyLen = 32

// Envelope holds an encrypted value. WrappedKey and Ciphertext are raw bytes
// on the wire; the JSON serialization below is the persisted form for a DB
// column.
type Envelope struct {
	WrappedKey []byte `json:"wk"`
	Ciphertext []byte `json:"ct"`
	Nonce      []byte `json:"n"`
}

// Cipher is an envelope encryptor keyed by a single master key.
type Cipher struct {
	masterKey []byte
}

// NewCipher returns a Cipher backed by the supplied AES-256 master key.
func NewCipher(masterKey []byte) (*Cipher, error) {
	if len(masterKey) != MasterKeyLen {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", MasterKeyLen, len(masterKey))
	}
	return &Cipher{masterKey: masterKey}, nil
}

// Encrypt encrypts plaintext under a fresh random data key and wraps the data
// key under the master key. The returned Envelope can be serialized with
// Marshal for storage.
func (c *Cipher) Encrypt(plaintext []byte) (*Envelope, error) {
	dataKey := make([]byte, MasterKeyLen)
	if _, err := io.ReadFull(rand.Reader, dataKey); err != nil {
		return nil, fmt.Errorf("read data key: %w", err)
	}

	dataCipher, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, fmt.Errorf("new data cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(dataCipher)
	if err != nil {
		return nil, fmt.Errorf("new data gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	wrappedKey, err := wrapKey(c.masterKey, dataKey)
	if err != nil {
		return nil, err
	}
	return &Envelope{WrappedKey: wrappedKey, Ciphertext: ciphertext, Nonce: nonce}, nil
}

// Decrypt recovers plaintext from an Envelope produced by Encrypt.
func (c *Cipher) Decrypt(env *Envelope) ([]byte, error) {
	if env == nil {
		return nil, errors.New("nil envelope")
	}
	dataKey, err := unwrapKey(c.masterKey, env.WrappedKey)
	if err != nil {
		return nil, fmt.Errorf("unwrap data key: %w", err)
	}
	dataCipher, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, fmt.Errorf("new data cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(dataCipher)
	if err != nil {
		return nil, fmt.Errorf("new data gcm: %w", err)
	}
	if len(env.Nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("nonce len = %d, want %d", len(env.Nonce), gcm.NonceSize())
	}
	plaintext, err := gcm.Open(nil, env.Nonce, env.Ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("open ciphertext: %w", err)
	}
	return plaintext, nil
}

// Marshal serializes env into a compact JSON string suitable for storing in a
// TEXT or JSONB column.
func Marshal(env *Envelope) (string, error) {
	b, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("marshal envelope: %w", err)
	}
	return string(b), nil
}

// Unmarshal parses a JSON string previously produced by Marshal back into an
// Envelope.
func Unmarshal(s string) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal([]byte(s), &env); nil != err {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	return &env, nil
}

// EncodeBase64 returns a base64-encoded JSON form for environments that prefer
// opaque string blobs over JSONB.
func EncodeBase64(env *Envelope) (string, error) {
	b, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("marshal envelope: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// DecodeBase64 is the inverse of EncodeBase64.
func DecodeBase64(s string) (*Envelope, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); nil != err {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	return &env, nil
}

func wrapKey(masterKey, dataKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("new master cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new master gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read wrap nonce: %w", err)
	}
	wrapped := gcm.Seal(nonce, nonce, dataKey, nil)
	return wrapped, nil
}

func unwrapKey(masterKey, wrapped []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("new master cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new master gcm: %w", err)
	}
	if len(wrapped) < gcm.NonceSize() {
		return nil, errors.New("wrapped key too short")
	}
	nonce, ciphertext := wrapped[:gcm.NonceSize()], wrapped[gcm.NonceSize():]
	dataKey, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("open wrapped key: %w", err)
	}
	return dataKey, nil
}