package s3api

import (
	"bytes"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/hpolthof/webdav3s/internal/auth"
)

func TestAWSChunkedReader_DecodeWithoutVerification(t *testing.T) {
	payload := strings.Join([]string{
		"7;chunk-signature=aaa",
		"hello w",
		"4;chunk-signature=bbb",
		"orld",
		"0;chunk-signature=ccc",
		"",
		"",
	}, "\r\n")

	r := newAWSChunkedReader(strings.NewReader(payload), nil, "seed", "", "")
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("decoded body: got %q want %q", got, "hello world")
	}
}

func TestAWSChunkedReader_VerifiesSignatures(t *testing.T) {
	secretKey := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	date := "20150830"
	region := "us-east-1"
	service := "service"
	amzDate := "20150830T123600Z"
	scope := date + "/" + region + "/" + service + "/aws4_request"
	signingKey := auth.DeriveSigningKey(secretKey, date, region, service)

	// Compute seed signature (request signature) for an empty-body request.
	canonicalReq := strings.Join([]string{
		"PUT",
		"/",
		"",
		"host:example.amazonaws.com\nx-amz-date:" + amzDate + "\nx-amz-content-sha256:" + "STREAMING-AWS4-HMAC-SHA256-PAYLOAD" + "\n",
		"host;x-amz-date;x-amz-content-sha256",
		"STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
	}, "\n")
	canonicalHash := auth.HashSHA256([]byte(canonicalReq))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		canonicalHash,
	}, "\n")
	seedSig := hex.EncodeToString(auth.HMACSHA256(signingKey, []byte(stringToSign)))

	// Build a two-chunk payload and compute each chunk signature.
	chunk1 := []byte("hello w")
	chunk2 := []byte("orld")

	sig1 := chunkSig(signingKey, amzDate, scope, seedSig, chunk1)
	sig2 := chunkSig(signingKey, amzDate, scope, sig1, chunk2)
	finalSig := chunkSig(signingKey, amzDate, scope, sig2, []byte{})

	payload := strings.Join([]string{
		"7;chunk-signature=" + sig1,
		string(chunk1),
		"4;chunk-signature=" + sig2,
		string(chunk2),
		"0;chunk-signature=" + finalSig,
		"",
		"",
	}, "\r\n")

	r := newAWSChunkedReader(strings.NewReader(payload), signingKey, seedSig, amzDate, scope)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("decoded body: got %q want %q", got, "hello world")
	}
}

func TestAWSChunkedReader_RejectsBadSignature(t *testing.T) {
	secretKey := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	date := "20150830"
	region := "us-east-1"
	service := "service"
	amzDate := "20150830T123600Z"
	scope := date + "/" + region + "/" + service + "/aws4_request"
	signingKey := auth.DeriveSigningKey(secretKey, date, region, service)

	payload := strings.Join([]string{
		"5;chunk-signature=0000000000000000000000000000000000000000000000000000000000000000",
		"hello",
		"0;chunk-signature=0000000000000000000000000000000000000000000000000000000000000000",
		"",
		"",
	}, "\r\n")

	r := newAWSChunkedReader(strings.NewReader(payload), signingKey, "seed", amzDate, scope)
	_, err := io.ReadAll(r)
	if err == nil {
		t.Fatal("expected chunk signature mismatch error")
	}
	if !strings.Contains(err.Error(), "chunk signature mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func chunkSig(signingKey []byte, amzDate, scope, prevSig string, data []byte) string {
	emptyHash := auth.HashSHA256([]byte{})
	dataHash := auth.HashSHA256(data)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256-PAYLOAD",
		amzDate,
		scope,
		prevSig,
		emptyHash,
		dataHash,
	}, "\n")
	return hex.EncodeToString(auth.HMACSHA256(signingKey, []byte(stringToSign)))
}

func TestAWSChunkedReader_HandlesSingleChunk(t *testing.T) {
	payload := strings.Join([]string{
		"5;chunk-signature=aaa",
		"hello",
		"0;chunk-signature=bbb",
		"",
		"",
	}, "\r\n")

	r := newAWSChunkedReader(strings.NewReader(payload), nil, "seed", "", "")
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, []byte("hello")) {
		t.Errorf("got %q want %q", got, "hello")
	}
}

func TestAWSChunkedReader_EmptyPayload(t *testing.T) {
	payload := "0;chunk-signature=aaa\r\n\r\n"
	r := newAWSChunkedReader(strings.NewReader(payload), nil, "seed", "", "")
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty body, got %q", got)
	}
}

func TestAWSChunkedReader_PreservesBinaryData(t *testing.T) {
	data := bytes.Repeat([]byte{0xab, 0xcd}, 1024)
	size := len(data)
	payload := strings.Join([]string{
		string(intBytes(size)) + ";chunk-signature=aaa",
		string(data),
		"0;chunk-signature=bbb",
		"",
		"",
	}, "\r\n")

	r := newAWSChunkedReader(strings.NewReader(payload), nil, "seed", "", "")
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("decoded binary data mismatch: got %d bytes want %d", len(got), size)
	}
}

func intBytes(n int) []byte {
	return []byte(strconvItoa(n))
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digit := n % 16
		if digit < 10 {
			digits = append(digits, byte('0'+digit))
		} else {
			digits = append(digits, byte('a'+digit-10))
		}
		n /= 16
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}
