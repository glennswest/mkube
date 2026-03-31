package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("hello, secrets!")
	ciphertext, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext should differ from plaintext")
	}

	got, err := Decrypt(key, ciphertext)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, plaintext) {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}

func TestEncryptDecryptEmpty(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	ciphertext, err := Encrypt(key, []byte{})
	if err != nil {
		t.Fatal(err)
	}

	got, err := Decrypt(key, ciphertext)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 0 {
		t.Fatalf("expected empty plaintext, got %d bytes", len(got))
	}
}

func TestDecryptTampered(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	ciphertext, err := Encrypt(key, []byte("secret data"))
	if err != nil {
		t.Fatal(err)
	}

	// Flip a byte in the ciphertext (after the nonce)
	ciphertext[aesNonceSize+1] ^= 0xff

	_, err = Decrypt(key, ciphertext)
	if err == nil {
		t.Fatal("expected error decrypting tampered ciphertext")
	}
}

func TestDecryptTooShort(t *testing.T) {
	key, _ := GenerateKey()
	_, err := Decrypt(key, []byte("short"))
	if err == nil {
		t.Fatal("expected error for short ciphertext")
	}
}

func TestDecryptWrongKey(t *testing.T) {
	key1, _ := GenerateKey()
	key2, _ := GenerateKey()

	ciphertext, err := Encrypt(key1, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = Decrypt(key2, ciphertext)
	if err == nil {
		t.Fatal("expected error decrypting with wrong key")
	}
}

func TestLoadOrGenerateKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "secret.key")

	// First call: generate
	key1, err := LoadOrGenerateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(key1) != aesKeySize {
		t.Fatalf("key length %d, want %d", len(key1), aesKeySize)
	}

	// Verify file permissions
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file permissions %o, want 0600", info.Mode().Perm())
	}

	// Second call: load existing
	key2, err := LoadOrGenerateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key1, key2) {
		t.Fatal("loaded key differs from generated key")
	}
}

func TestLoadOrGenerateKeyWrongSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.key")

	if err := os.WriteFile(path, []byte("tooshort"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadOrGenerateKey(path)
	if err == nil {
		t.Fatal("expected error for wrong-size key file")
	}
}
