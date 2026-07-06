package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

func mustKey(t *testing.T, hexKey string) []byte {
	t.Helper()
	k, err := hex.DecodeString(hexKey)
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	return k
}

func TestNewCipherRejectsBadKeyLen(t *testing.T) {
	cases := []int{0, 16, 31, 33}
	for _, n := range cases {
		if _, err := NewCipher(make([]byte, n)); err == nil {
			t.Errorf("NewCipher(%d-byte key) expected error, got nil", n)
		}
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	master := mustKey(t, strings.Repeat("ab", 32))
	c, err := NewCipher(master)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	plaintexts := [][]byte{
		[]byte(""),
		[]byte("user@example.com"),
		[]byte("123-45-6789"),
		bytes.Repeat([]byte{0x41}, 4096),
	}
	for _, pt := range plaintexts {
		env, err := c.Encrypt(pt)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", pt, err)
		}
		got, err := c.Decrypt(env)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if !bytes.Equal(got, pt) {
			t.Errorf("round-trip mismatch: got %q, want %q", got, pt)
		}
	}
}

func TestEnvelopeMarshalRoundTrip(t *testing.T) {
	master := mustKey(t, strings.Repeat("ab", 32))
	c, err := NewCipher(master)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	pt := []byte("John Q. Public")

	env, err := c.Encrypt(pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	s, err := Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	env2, err := Unmarshal(s)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, err := c.Decrypt(env2)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Errorf("marshal round-trip mismatch: got %q, want %q", got, pt)
	}
}

func TestEnvelopeBase64RoundTrip(t *testing.T) {
	master := mustKey(t, strings.Repeat("ab", 32))
	c, err := NewCipher(master)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	pt := []byte("Jane Roe")

	env, err := c.Encrypt(pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	s, err := EncodeBase64(env)
	if err != nil {
		t.Fatalf("EncodeBase64: %v", err)
	}
	env2, err := DecodeBase64(s)
	if err != nil {
		t.Fatalf("DecodeBase64: %v", err)
	}
	got, err := c.Decrypt(env2)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Errorf("base64 round-trip mismatch: got %q, want %q", got, pt)
	}
}

func TestEnvelopeFreshKeyPerEncryption(t *testing.T) {
	master := mustKey(t, strings.Repeat("ab", 32))
	c, err := NewCipher(master)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	pt := []byte("same plaintext")

	env1, err := c.Encrypt(pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	env2, err := c.Encrypt(pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(env1.Ciphertext, env2.Ciphertext) {
		t.Error("two encryptions of the same plaintext produced identical ciphertext")
	}
	if bytes.Equal(env1.WrappedKey, env2.WrappedKey) {
		t.Error("two encryptions reused the same wrapped key")
	}
}

func TestEnvelopeWrongMasterKeyFails(t *testing.T) {
	master1 := mustKey(t, strings.Repeat("ab", 32))
	master2 := make([]byte, MasterKeyLen)
	if _, err := rand.Read(master2); err != nil {
		t.Fatalf("rand: %v", err)
	}
	c1, err := NewCipher(master1)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	c2, err := NewCipher(master2)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	env, err := c1.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := c2.Decrypt(env); err == nil {
		t.Error("Decrypt with wrong master key expected to fail")
	}
}

func TestDecryptNilEnvelope(t *testing.T) {
	c, err := NewCipher(mustKey(t, strings.Repeat("ab", 32)))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	if _, err := c.Decrypt(nil); err == nil {
		t.Error("Decrypt(nil) expected error")
	}
}

func TestUnmarshalInvalidJSON(t *testing.T) {
	if _, err := Unmarshal("not-json"); err == nil {
		t.Error("Unmarshal expected error for invalid JSON")
	}
}