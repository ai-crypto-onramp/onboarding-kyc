package db

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestLoadMigrations(t *testing.T) {
	migs, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("expected at least one migration")
	}
	if migs[0].Version != 1 {
		t.Errorf("first migration version: want 1 got %d", migs[0].Version)
	}
	if migs[0].UpSQL == "" {
		t.Error("first migration has empty UpSQL")
	}
	if migs[0].DownSQL == "" {
		t.Error("first migration has empty DownSQL")
	}
}

func TestLoadMigrationsContainsKycTables(t *testing.T) {
	migs, _ := LoadMigrations()
	for _, want := range []string{"kyc_applications", "documents", "liveness_sessions", "sanctions_hits", "kyc_decisions", "webhook_events", "audit_events"} {
		if !strings.Contains(migs[0].UpSQL, want) {
			t.Errorf("up migration missing table %q", want)
		}
	}
	if !strings.Contains(migs[0].UpSQL, "CREATE TABLE") {
		t.Error("up migration does not contain CREATE TABLE")
	}
	if !strings.Contains(migs[0].DownSQL, "DROP TABLE") {
		t.Error("down migration does not contain DROP TABLE")
	}
}

func TestDefaultConfigFromEnv(t *testing.T) {
	t.Setenv("DB_URL", "postgres://user:pass@localhost:5432/testdb")
	t.Setenv("DB_MAX_CONNS", "10")
	t.Setenv("DB_MIN_CONNS", "1")
	t.Setenv("DB_CONNECT_TIMEOUT", "3s")
	cfg := DefaultConfig()
	if cfg.DSN != "postgres://user:pass@localhost:5432/testdb" {
		t.Errorf("DSN: %q", cfg.DSN)
	}
	if cfg.MaxConns != 10 {
		t.Errorf("MaxConns: %d", cfg.MaxConns)
	}
	if cfg.MinConns != 1 {
		t.Errorf("MinConns: %d", cfg.MinConns)
	}
	if cfg.ConnectTimeout.Seconds() != 3 {
		t.Errorf("ConnectTimeout: %v", cfg.ConnectTimeout)
	}
}

func TestDefaultConfigDefaults(t *testing.T) {
	os.Unsetenv("DB_URL")
	os.Unsetenv("DB_MAX_CONNS")
	cfg := DefaultConfig()
	if cfg.DSN != "" {
		t.Errorf("DSN should be empty, got %q", cfg.DSN)
	}
	if cfg.MaxConns != 20 {
		t.Errorf("default MaxConns: %d", cfg.MaxConns)
	}
}

func TestPoolRequiresDSN(t *testing.T) {
	t.Setenv("DB_URL", "")
	_, err := Pool(t.Context(), &Config{DSN: ""})
	if err == nil {
		t.Fatal("expected error for empty DSN")
	}
	if !strings.Contains(err.Error(), "DB_URL") {
		t.Errorf("error should mention DB_URL, got %q", err.Error())
	}
}

// --- Encryption helpers ---

func TestEncryptorRoundTrip(t *testing.T) {
	enc, err := NewEncryptorFromEnv()
	if err != nil {
		t.Fatalf("NewEncryptorFromEnv: %v", err)
	}
	plaintext := []byte("my-secret-totp-key-12345")
	blob, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(blob, plaintext) {
		t.Error("ciphertext equals plaintext")
	}
	dec, err := enc.Decrypt(blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(dec, plaintext) {
		t.Errorf("round-trip mismatch: want %q got %q", plaintext, dec)
	}
}

func TestEncryptorStringRoundTrip(t *testing.T) {
	enc, _ := NewEncryptorFromEnv()
	original := "super-secret-api-key-value"
	encoded, err := enc.EncryptString(original)
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	decoded, err := enc.DecryptString(encoded)
	if err != nil {
		t.Fatalf("DecryptString: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip: want %q got %q", original, decoded)
	}
}

func TestEncryptorDifferentCiphertexts(t *testing.T) {
	enc, _ := NewEncryptorFromEnv()
	pt := []byte("same-plaintext")
	c1, _ := enc.Encrypt(pt)
	c2, _ := enc.Encrypt(pt)
	if bytes.Equal(c1, c2) {
		t.Error("two encryptions of the same plaintext produced identical ciphertext (nonce reuse)")
	}
}

func TestEncryptorInvalidKeyLength(t *testing.T) {
	_, err := NewEncryptor([]byte("too-short"))
	if err == nil {
		t.Fatal("expected error for short key")
	}
	if !strings.Contains(err.Error(), "32 bytes") {
		t.Errorf("error should mention 32 bytes, got %q", err.Error())
	}
}

func TestEncryptorFromPassphrase(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "")
	t.Setenv("ENCRYPTION_PASSPHRASE", "my-passphrase")
	enc, err := NewEncryptorFromEnv()
	if err != nil {
		t.Fatalf("from passphrase: %v", err)
	}
	pt := []byte("secret")
	blob, err := enc.Encrypt(pt)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	dec, err := enc.Decrypt(blob)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(dec) != "secret" {
		t.Errorf("round-trip: want 'secret' got %q", dec)
	}
}

func TestEncryptorFromBase64Key(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("ENCRYPTION_PASSPHRASE", "")
	enc, err := NewEncryptorFromEnv()
	if err != nil {
		t.Fatalf("from base64 key: %v", err)
	}
	blob, _ := enc.Encrypt([]byte("x"))
	if _, err := enc.Decrypt(blob); err != nil {
		t.Fatalf("decrypt with base64 key: %v", err)
	}
}

func TestDecryptShortInput(t *testing.T) {
	enc, _ := NewEncryptorFromEnv()
	_, err := enc.Decrypt([]byte("short"))
	if err == nil {
		t.Fatal("expected error for short ciphertext")
	}
}

func TestDecryptStringInvalidBase64(t *testing.T) {
	enc, _ := NewEncryptorFromEnv()
	_, err := enc.DecryptString("!!!not-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}