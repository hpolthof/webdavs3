package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ErrUnknownAccessKey is returned by the lookup function when the access key is not found.
var ErrUnknownAccessKey = errors.New("unknown access key")

// ErrMissingAuthHeader is returned when the Authorization header is absent.
var ErrMissingAuthHeader = errors.New("missing Authorization header")

// ErrInvalidAuthHeader is returned when the Authorization header cannot be parsed.
var ErrInvalidAuthHeader = errors.New("invalid Authorization header format")

// ErrSignatureMismatch is returned when the computed signature does not match.
var ErrSignatureMismatch = errors.New("signature mismatch")

// ErrRequestExpired is returned when the x-amz-date header is outside the
// allowed clock-skew window.
var ErrRequestExpired = errors.New("request timestamp outside allowed window")

// ErrContentHashMismatch is returned when the request body SHA-256 does not
// match the x-amz-content-sha256 header.
var ErrContentHashMismatch = errors.New("payload hash mismatch")

// Verifier verifies AWS Signature V4 on incoming HTTP requests.
type Verifier interface {
	Verify(r *http.Request, lookupSecret func(accessKey string) (string, error)) (accessKey string, err error)
}

type verifier struct {
	region  string
	service string
	now     func() time.Time
}

// NewVerifier returns a Verifier for the given region and service name.
func NewVerifier(region, service string) Verifier {
	return &verifier{region: region, service: service, now: time.Now}
}

// NewVerifierWithClock returns a Verifier that uses the given clock function
// for the request timestamp window check. It is intended for tests with fixed
// test vectors.
func NewVerifierWithClock(region, service string, now func() time.Time) Verifier {
	return &verifier{region: region, service: service, now: now}
}

// Verify parses the Authorization header, derives the signing key, reconstructs
// the canonical request and string-to-sign, and compares signatures in constant time.
func (v *verifier) Verify(r *http.Request, lookupSecret func(string) (string, error)) (string, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", ErrMissingAuthHeader
	}
	if !strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256 ") {
		return "", ErrInvalidAuthHeader
	}

	// Parse credential, signedHeaders, signature
	credential, signedHeaders, signature, err := parseAuthHeader(authHeader)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidAuthHeader, err)
	}

	// Credential format: AKID/date/region/service/aws4_request
	parts := strings.SplitN(credential, "/", 5)
	if len(parts) != 5 {
		return "", fmt.Errorf("%w: malformed credential %q", ErrInvalidAuthHeader, credential)
	}
	accessKey, date := parts[0], parts[1]

	// Validate credential scope region and service
	if parts[2] != v.region || parts[3] != v.service {
		return "", fmt.Errorf("%w: credential scope region/service mismatch", ErrInvalidAuthHeader)
	}

	secretKey, err := lookupSecret(accessKey)
	if err != nil {
		return "", fmt.Errorf("lookup secret: %w", err)
	}

	// Obtain datetime from x-amz-date header (case-insensitive lookup)
	amzDate := r.Header.Get("x-amz-date")

	// Reject requests outside the allowed clock-skew window.
	if err := v.checkClockSkew(amzDate, 15*time.Minute); err != nil {
		return "", err
	}

	// Verify payload integrity for signed, non-streaming bodies.
	if err := verifyPayloadHash(r); err != nil {
		return "", err
	}

	// Build canonical request
	canonicalReq := buildCanonicalRequest(r, signedHeaders)
	canonicalHash := hashSHA256([]byte(canonicalReq))

	// String to sign
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		strings.Join([]string{date, v.region, v.service, "aws4_request"}, "/"),
		canonicalHash,
	}, "\n")

	// Derive signing key: HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region), service), "aws4_request")
	signingKey := deriveSigningKey(secretKey, date, v.region, v.service)
	computedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	// Constant-time compare
	if !hmac.Equal([]byte(computedSig), []byte(signature)) {
		return "", ErrSignatureMismatch
	}
	return accessKey, nil
}

func parseAuthHeader(h string) (credential, signedHeaders, signature string, err error) {
	// Strip prefix "AWS4-HMAC-SHA256 "
	h = strings.TrimPrefix(h, "AWS4-HMAC-SHA256 ")
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		switch {
		case hasPrefixCaseInsensitive(part, "Credential="):
			credential = trimPrefixCaseInsensitive(part, "Credential=")
			credential = strings.TrimSpace(credential)
		case hasPrefixCaseInsensitive(part, "SignedHeaders="):
			signedHeaders = trimPrefixCaseInsensitive(part, "SignedHeaders=")
			signedHeaders = strings.TrimSpace(signedHeaders)
		case hasPrefixCaseInsensitive(part, "Signature="):
			signature = trimPrefixCaseInsensitive(part, "Signature=")
			signature = strings.TrimSpace(signature)
		}
	}
	if credential == "" || signedHeaders == "" || signature == "" {
		return "", "", "", fmt.Errorf("incomplete Authorization header: credential=%q signedHeaders=%q signature=%q", credential, signedHeaders, signature)
	}
	return
}

func hasPrefixCaseInsensitive(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func trimPrefixCaseInsensitive(s, prefix string) string {
	if hasPrefixCaseInsensitive(s, prefix) {
		return s[len(prefix):]
	}
	return s
}

func buildCanonicalRequest(r *http.Request, signedHeadersStr string) string {
	method := r.Method

	// Canonical URI: path-encoded; must not be empty
	uri := r.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}

	// Canonical query string: sorted by key then value
	canonicalQS := buildCanonicalQueryString(r)

	// Canonical headers: only those listed in signedHeaders
	signedHeaderList := strings.Split(signedHeadersStr, ";")
	var canonicalHeaders strings.Builder
	for _, h := range signedHeaderList {
		var val string
		if h == "host" {
			// r.Host is set by the HTTP server; for constructed requests use URL.Host
			val = r.Host
			if val == "" && r.URL != nil {
				val = r.URL.Host
			}
		} else {
			val = r.Header.Get(h)
		}
		canonicalHeaders.WriteString(h)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.TrimSpace(val))
		canonicalHeaders.WriteByte('\n')
	}

	// Payload hash: trust x-amz-content-sha256 header; fall back to SHA256 of empty body
	payloadHash := r.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		payloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // SHA256("")
	}

	return strings.Join([]string{
		method,
		uri,
		canonicalQS,
		canonicalHeaders.String(),
		signedHeadersStr,
		payloadHash,
	}, "\n")
}

func buildCanonicalQueryString(r *http.Request) string {
	q := r.URL.Query()
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode percent-encodes a string per AWS SigV4 spec (RFC 3986 unreserved chars left unencoded).
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~'
}

func deriveSigningKey(secretKey, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// DeriveSigningKey derives the AWS SigV4 signing key for the given credential
// scope components. It is exported so the S3 API layer can verify streaming
// (aws-chunked) payload signatures.
func DeriveSigningKey(secretKey, date, region, service string) []byte {
	return deriveSigningKey(secretKey, date, region, service)
}

// ParseAuthHeader parses an AWS SigV4 Authorization header.
func ParseAuthHeader(h string) (credential, signedHeaders, signature string, err error) {
	return parseAuthHeader(h)
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// HMACSHA256 returns the HMAC-SHA256 of data using key.
func HMACSHA256(key, data []byte) []byte {
	return hmacSHA256(key, data)
}

func hashSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// HashSHA256 returns the hex-encoded SHA-256 hash of data.
func HashSHA256(data []byte) string {
	return hashSHA256(data)
}

// checkClockSkew parses amzDate (ISO8601 basic: YYYYMMDD'T'HHMMSS'Z') and
// returns ErrRequestExpired if it differs from the verifier's clock by more
// than window.
func (v *verifier) checkClockSkew(amzDate string, window time.Duration) error {
	if amzDate == "" {
		return fmt.Errorf("%w: missing x-amz-date", ErrRequestExpired)
	}
	t, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return fmt.Errorf("%w: invalid x-amz-date %q", ErrRequestExpired, amzDate)
	}
	now := time.Now()
	if v.now != nil {
		now = v.now()
	}
	diff := now.Sub(t)
	if diff < 0 {
		diff = -diff
	}
	if diff > window {
		return fmt.Errorf("%w: x-amz-date %q differs by %s", ErrRequestExpired, amzDate, diff)
	}
	return nil
}

// verifyPayloadHash reads the request body when x-amz-content-sha256 contains
// a literal SHA-256, compares it with the actual body hash, and restores the
// body so downstream handlers can read it. UNSIGNED-PAYLOAD and streaming
// payloads are skipped.
func verifyPayloadHash(r *http.Request) error {
	contentHash := r.Header.Get("x-amz-content-sha256")
	if contentHash == "" || contentHash == "UNSIGNED-PAYLOAD" || contentHash == "STREAMING-AWS4-HMAC-SHA256-PAYLOAD" {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	computed := hashSHA256(body)
	if !hmac.Equal([]byte(computed), []byte(contentHash)) {
		return fmt.Errorf("%w: computed %s expected %s", ErrContentHashMismatch, computed, contentHash)
	}
	return nil
}
