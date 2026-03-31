package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	aesKeySize   = 32 // AES-256
	aesNonceSize = 12 // GCM standard nonce
)

// GenerateKey returns a cryptographically random 32-byte AES-256 key.
func GenerateKey() ([]byte, error) {
	key := make([]byte, aesKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generating encryption key: %w", err)
	}
	return key, nil
}

// LoadOrGenerateKey reads an encryption key from path. If the file does not
// exist, a new key is generated and written with mode 0600.
func LoadOrGenerateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != aesKeySize {
			return nil, fmt.Errorf("encryption key at %s has wrong size (%d, want %d)", path, len(data), aesKeySize)
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading encryption key: %w", err)
	}

	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating key directory: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("writing encryption key: %w", err)
	}

	return key, nil
}

// Encrypt seals plaintext using AES-256-GCM. The returned ciphertext is
// formatted as [12-byte nonce][ciphertext+tag].
func Encrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	nonce := make([]byte, aesNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt opens AES-256-GCM ciphertext produced by Encrypt.
func Decrypt(key, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < aesNonceSize {
		return nil, errors.New("ciphertext too short")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	nonce := ciphertext[:aesNonceSize]
	sealed := ciphertext[aesNonceSize:]

	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
	}
	return plaintext, nil
}
