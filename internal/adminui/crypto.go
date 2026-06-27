package adminui

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// EncryptPassword encrypts plaintext with AES-256-GCM using the given
// base64-encoded 32-byte key. The returned string is base64(nonce || ciphertext).
func EncryptPassword(plaintext, base64Key string) (string, error) {
	key, err := decodeKey(base64Key)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	// Seal appends ciphertext+tag to nonce.
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptPassword decrypts a ciphertext produced by EncryptPassword.
// ciphertext is base64(nonce || ciphertext+tag).
func DecryptPassword(ciphertext, base64Key string) (string, error) {
	key, err := decodeKey(base64Key)
	if err != nil {
		return "", err
	}

	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decode base64 ciphertext: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", fmt.Errorf("ciphertext too short: %d bytes (need at least %d)", len(raw), nonceSize)
	}

	nonce, ct := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}

// encodePassword encrypts plaintext with AES-256-GCM using base64Key.
// It returns an error if base64Key is empty, because storing credentials
// without encryption is not permitted.
func encodePassword(plaintext, base64Key string) (string, error) {
	if base64Key == "" {
		return "", fmt.Errorf("encryption key is not configured; refusing to store credentials in plaintext")
	}
	return EncryptPassword(plaintext, base64Key)
}

// decodeKey decodes a base64-encoded key and validates it is 32 bytes (AES-256).
func decodeKey(base64Key string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("decode base64 key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes for AES-256 (got %d)", len(key))
	}
	return key, nil
}
