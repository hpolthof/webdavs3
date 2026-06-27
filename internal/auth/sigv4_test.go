package auth_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hpolthof/webdavs3/internal/auth"
)

// Test vectors from AWS Signature V4 Test Suite
// https://docs.aws.amazon.com/general/latest/gr/sigv4-test-suite.html
// Using the "get-vanilla" example.

const (
	testAccessKey = "AKIDEXAMPLE"
	testSecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
)

func TestVerifier_GetVanilla(t *testing.T) {
	// Canonical request from AWS test suite (get-vanilla):
	// GET / HTTP/1.1
	// host: example.amazonaws.com
	// x-amz-date: 20150830T123600Z
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	req.Header.Set("host", "example.amazonaws.com")
	req.Header.Set("x-amz-date", "20150830T123600Z")
	// Authorization header: self-derived expected signature for this test vector
	req.Header.Set("Authorization",
		`AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, `+
			`SignedHeaders=host;x-amz-date, `+
			`Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31`)

	// Freeze the clock to the request timestamp so the clock-skew check passes
	// for this historical AWS test vector.
	fixed := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	v := auth.NewVerifierWithClock("us-east-1", "service", func() time.Time { return fixed })
	accessKey, err := v.Verify(req, func(ak string) (string, error) {
		if ak == testAccessKey {
			return testSecretKey, nil
		}
		return "", auth.ErrUnknownAccessKey
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if accessKey != testAccessKey {
		t.Errorf("accessKey: got %q want %q", accessKey, testAccessKey)
	}
}

func TestVerifier_WrongSecret(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	req.Header.Set("host", "example.amazonaws.com")
	req.Header.Set("x-amz-date", "20150830T123600Z")
	req.Header.Set("Authorization",
		`AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, `+
			`SignedHeaders=host;x-amz-date, `+
			`Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31`)

	fixed := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	v := auth.NewVerifierWithClock("us-east-1", "service", func() time.Time { return fixed })
	_, err := v.Verify(req, func(ak string) (string, error) {
		return "wrong-secret-key", nil
	})
	if err == nil {
		t.Fatal("expected signature mismatch error")
	}
}

func TestVerifier_MissingAuthHeader(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://s3.example.com/bucket/key", nil)
	v := auth.NewVerifier("us-east-1", "s3")
	_, err := v.Verify(req, func(ak string) (string, error) { return "", nil })
	if err == nil {
		t.Fatal("expected error for missing Authorization header")
	}
}

func TestVerifier_InvalidAuthScheme(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://s3.example.com/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	v := auth.NewVerifier("us-east-1", "s3")
	_, err := v.Verify(req, func(ak string) (string, error) { return "", nil })
	if err == nil {
		t.Fatal("expected error for non-SigV4 Authorization scheme")
	}
}

func TestVerifier_RequestExpired(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://s3.example.com/bucket/key", nil)
	req.Header.Set("host", "s3.example.com")
	req.Header.Set("x-amz-date", "20150830T123600Z")
	req.Header.Set("Authorization",
		`AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/s3/aws4_request, `+
			`SignedHeaders=host;x-amz-date, `+
			`Signature=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`)

	v := auth.NewVerifier("us-east-1", "s3")
	_, err := v.Verify(req, func(ak string) (string, error) { return testSecretKey, nil })
	if !strings.Contains(err.Error(), auth.ErrRequestExpired.Error()) {
		t.Fatalf("expected request expired error, got %v", err)
	}
}

func TestVerifier_PayloadHashMismatch(t *testing.T) {
	body := []byte("hello world")
	h := sha256.Sum256([]byte("other"))
	wrongHash := hex.EncodeToString(h[:])

	req, _ := http.NewRequest("PUT", "https://s3.example.com/bucket/key", bytes.NewReader(body))
	req.Header.Set("host", "s3.example.com")
	req.Header.Set("x-amz-date", time.Now().UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", wrongHash)
	req.Header.Set("Authorization",
		`AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/`+time.Now().UTC().Format("20060102")+`/us-east-1/s3/aws4_request, `+
			`SignedHeaders=host;x-amz-date;x-amz-content-sha256, `+
			`Signature=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`)

	v := auth.NewVerifier("us-east-1", "s3")
	_, err := v.Verify(req, func(ak string) (string, error) { return testSecretKey, nil })
	if !strings.Contains(err.Error(), auth.ErrContentHashMismatch.Error()) {
		t.Fatalf("expected content hash mismatch error, got %v", err)
	}
}

func TestVerifier_PayloadHashVerified(t *testing.T) {
	body := []byte("hello world")
	h := sha256.Sum256(body)
	rightHash := hex.EncodeToString(h[:])

	now := time.Now().UTC()
	req, _ := http.NewRequest("PUT", "https://s3.example.com/bucket/key", bytes.NewReader(body))
	req.Header.Set("host", "s3.example.com")
	req.Header.Set("x-amz-date", now.Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", rightHash)
	// Wrong signature on purpose; we only want to ensure the body is restored
	// after hashing and is available for downstream handlers.
	req.Header.Set("Authorization",
		`AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/`+now.Format("20060102")+`/us-east-1/s3/aws4_request, `+
			`SignedHeaders=host;x-amz-date;x-amz-content-sha256, `+
			`Signature=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`)

	v := auth.NewVerifier("us-east-1", "s3")
	_, _ = v.Verify(req, func(ak string) (string, error) { return testSecretKey, nil })

	// Body must still be readable after verification.
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body after verify: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body after verify: got %q want %q", got, body)
	}
}
