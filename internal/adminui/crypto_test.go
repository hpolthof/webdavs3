package adminui

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	// 32-byte AES-256 key, base64-encoded.
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))

	plaintext := "correct horse battery staple"

	ciphertext, err := EncryptPassword(plaintext, key)
	if err != nil {
		t.Fatalf("EncryptPassword: %v", err)
	}
	if ciphertext == "" {
		t.Fatal("expected non-empty ciphertext")
	}
	if ciphertext == plaintext {
		t.Fatal("ciphertext equals plaintext — not encrypted")
	}

	recovered, err := DecryptPassword(ciphertext, key)
	if err != nil {
		t.Fatalf("DecryptPassword: %v", err)
	}
	if recovered != plaintext {
		t.Errorf("round-trip mismatch: got %q want %q", recovered, plaintext)
	}
}

func TestEncryptDecrypt_DifferentNonces(t *testing.T) {
	// Two encryptions of the same plaintext must produce different ciphertexts.
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	plaintext := "same text"

	ct1, err := EncryptPassword(plaintext, key)
	if err != nil {
		t.Fatalf("first encrypt: %v", err)
	}
	ct2, err := EncryptPassword(plaintext, key)
	if err != nil {
		t.Fatalf("second encrypt: %v", err)
	}
	if ct1 == ct2 {
		t.Error("two encryptions of the same plaintext must not produce the same ciphertext")
	}
}

func TestDecryptPassword_TamperedCiphertext(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	ct, _ := EncryptPassword("hello", key)

	// Flip a byte in the raw decoded bytes and re-encode.
	raw, _ := base64.StdEncoding.DecodeString(ct)
	raw[len(raw)-1] ^= 0xff
	tampered := base64.StdEncoding.EncodeToString(raw)

	_, err := DecryptPassword(tampered, key)
	if err == nil {
		t.Error("expected error decrypting tampered ciphertext, got nil")
	}
}

func TestDecryptPassword_WrongKey(t *testing.T) {
	key1 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	key2Bytes := make([]byte, 32)
	key2Bytes[0] = 1
	key2 := base64.StdEncoding.EncodeToString(key2Bytes)

	ct, _ := EncryptPassword("hello", key1)
	_, err := DecryptPassword(ct, key2)
	if err == nil {
		t.Error("expected error decrypting with wrong key, got nil")
	}
}

func TestEncryptPassword_InvalidKey(t *testing.T) {
	// Not valid base64.
	_, err := EncryptPassword("hello", "not-base64!!!")
	if err == nil {
		t.Error("expected error for invalid base64 key")
	}
}

func TestEncryptPassword_WrongKeyLength(t *testing.T) {
	// Valid base64 but only 16 bytes (AES-128, not 256).
	key := base64.StdEncoding.EncodeToString(make([]byte, 16))
	_, err := EncryptPassword("hello", key)
	if err == nil {
		t.Error("expected error for 16-byte key (need 32 for AES-256)")
	}
}

func TestEncryptPassword_EmptyPlaintext(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	ct, err := EncryptPassword("", key)
	if err != nil {
		t.Fatalf("EncryptPassword empty: %v", err)
	}
	// Should still round-trip.
	got, err := DecryptPassword(ct, key)
	if err != nil {
		t.Fatalf("DecryptPassword empty: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestDecryptPassword_NotBase64(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	_, err := DecryptPassword("not-valid-base64!!!", key)
	if err == nil {
		t.Error("expected error for non-base64 ciphertext")
	}
}

func TestDecryptPassword_TooShort(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	// Valid base64 but only 5 bytes (shorter than the 12-byte nonce).
	ct := base64.StdEncoding.EncodeToString([]byte("hello"))
	_, err := DecryptPassword(ct, key)
	if err == nil {
		t.Error("expected error for ciphertext shorter than nonce")
	}
}

func TestEncryptPassword_OutputIsBase64(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	ct, err := EncryptPassword("test", key)
	if err != nil {
		t.Fatalf("EncryptPassword: %v", err)
	}
	// Must be valid standard base64.
	if _, err := base64.StdEncoding.DecodeString(ct); err != nil {
		t.Errorf("output is not valid base64: %v", err)
	}
	// Must not contain padding issues or whitespace.
	if strings.ContainsAny(ct, " \n\t\r") {
		t.Error("output contains whitespace")
	}
}
