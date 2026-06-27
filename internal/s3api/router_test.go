package s3api_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hpolthof/webdav3s/internal/meta"
	"github.com/hpolthof/webdav3s/internal/object"
	"github.com/hpolthof/webdav3s/internal/s3api"
)

// stubVerifier always authenticates as a fixed access key.
type stubVerifier struct{ accessKey string }

func (s *stubVerifier) Verify(r *http.Request, _ func(string) (string, error)) (string, error) {
	return s.accessKey, nil
}

// stubBucketService is an in-memory bucket service for testing.
type stubBucketService struct {
	buckets map[string]meta.Bucket
}

func newStubBucketService() *stubBucketService {
	return &stubBucketService{buckets: map[string]meta.Bucket{}}
}

func (s *stubBucketService) CreateBucket(_ context.Context, name, ownerUserID, locationID string) error {
	if _, ok := s.buckets[name]; ok {
		return fmt.Errorf("bucket already exists")
	}
	s.buckets[name] = meta.Bucket{ID: name, Name: name, OwnerUserID: ownerUserID, CreatedAt: time.Now()}
	return nil
}

func (s *stubBucketService) DeleteBucket(_ context.Context, name string) error {
	delete(s.buckets, name)
	return nil
}

func (s *stubBucketService) ListBuckets(_ context.Context, ownerUserID string) ([]meta.Bucket, error) {
	var result []meta.Bucket
	for _, b := range s.buckets {
		if b.OwnerUserID == ownerUserID {
			result = append(result, b)
		}
	}
	return result, nil
}

// stubObjectService — minimal no-op.
type stubObjectService struct{}

func (s *stubObjectService) Put(_ context.Context, bucketName, key, contentType string, size int64, body io.Reader) (meta.Object, error) {
	io.Copy(io.Discard, body)
	return meta.Object{Key: key, ETag: `"test"`, SizeBytes: size, ContentType: contentType, LastModified: time.Now()}, nil
}
func (s *stubObjectService) Get(_ context.Context, bucketName, key string) (meta.Object, io.ReadCloser, error) {
	return meta.Object{Key: key, SizeBytes: 5, ETag: `"test"`, LastModified: time.Now()},
		io.NopCloser(strings.NewReader("hello")), nil
}
func (s *stubObjectService) Delete(_ context.Context, bucketName, key string) error { return nil }
func (s *stubObjectService) Head(_ context.Context, bucketName, key string) (meta.Object, error) {
	return meta.Object{Key: key, SizeBytes: 5, ETag: `"test"`, LastModified: time.Now()}, nil
}
func (s *stubObjectService) List(_ context.Context, bucketName, prefix, delimiter, token string, maxKeys int) (object.ListResult, error) {
	return object.ListResult{}, nil
}

// stubMultipartService — minimal no-op.
type stubMultipartService struct{}

func (s *stubMultipartService) Create(_ context.Context, bucketName, key, contentType string) (string, error) {
	return "upload-id-123", nil
}
func (s *stubMultipartService) UploadPart(_ context.Context, bucketName, key, uploadID string, partNum int, size int64, body io.Reader) (string, error) {
	io.Copy(io.Discard, body)
	return `"part-etag"`, nil
}
func (s *stubMultipartService) Complete(_ context.Context, bucketName, key, uploadID string, parts []object.CompletePart) (meta.Object, error) {
	return meta.Object{Key: key, ETag: `"final"`, SizeBytes: 100, LastModified: time.Now()}, nil
}
func (s *stubMultipartService) Abort(_ context.Context, bucketName, uploadID string) error {
	return nil
}
func (s *stubMultipartService) ListUploads(_ context.Context, _ string) ([]meta.MultipartUpload, error) {
	return nil, nil
}
func (s *stubMultipartService) ListParts(_ context.Context, _, _ string) ([]meta.MultipartPart, error) {
	return nil, nil
}

// stubQuotaService always allows.
type stubQuotaService struct{}

func (s *stubQuotaService) Check(_ context.Context, locationID string, _ int64) error { return nil }

// stubStructureDB — just enough for auth middleware.
type stubStructureDB struct {
	users   map[string]meta.User
	buckets map[string]meta.Bucket
}

func newStubStructureDB() *stubStructureDB {
	return &stubStructureDB{
		users: map[string]meta.User{
			"TESTKEY": {ID: "u-1", AccessKey: "TESTKEY", Enabled: true, DisplayName: "Test"},
		},
		buckets: map[string]meta.Bucket{},
	}
}

func (s *stubStructureDB) GetUserByAccessKey(key string) (meta.User, error) {
	u, ok := s.users[key]
	if !ok {
		return meta.User{}, fmt.Errorf("not found")
	}
	return u, nil
}

// Remaining StructureDB methods — return zero values or errors.
func (s *stubStructureDB) AddLocation(meta.Location) error           { return nil }
func (s *stubStructureDB) GetLocation(string) (meta.Location, error) { return meta.Location{}, nil }
func (s *stubStructureDB) ListLocations() ([]meta.Location, error)   { return nil, nil }
func (s *stubStructureDB) UpdateLocation(meta.Location) error        { return nil }
func (s *stubStructureDB) DeleteLocation(string) error               { return nil }
func (s *stubStructureDB) AddUser(meta.User) error                   { return nil }
func (s *stubStructureDB) GetUser(string) (meta.User, error)         { return meta.User{}, nil }
func (s *stubStructureDB) ListUsers() ([]meta.User, error)           { return nil, nil }
func (s *stubStructureDB) UpdateUser(meta.User) error                { return nil }
func (s *stubStructureDB) DeleteUser(string) error                   { return nil }
func (s *stubStructureDB) SetUserEnabled(string, bool) error         { return nil }
func (s *stubStructureDB) SetUserWebPassword(string, string) error   { return nil }
func (s *stubStructureDB) AddBucket(meta.Bucket) error               { return nil }
func (s *stubStructureDB) UpdateBucket(meta.Bucket) error            { return nil }
func (s *stubStructureDB) GetBucket(name string) (meta.Bucket, error) {
	if b, ok := s.buckets[name]; ok {
		return b, nil
	}
	return meta.Bucket{}, fmt.Errorf("bucket %q not found", name)
}
func (s *stubStructureDB) ListBuckets() ([]meta.Bucket, error) { return nil, nil }
func (s *stubStructureDB) ListBucketsByUser(string) ([]meta.Bucket, error) {
	return nil, nil
}
func (s *stubStructureDB) DeleteBucket(string) error { return nil }
func (s *stubStructureDB) SaveToFile(string) error   { return nil }
func (s *stubStructureDB) LoadFromFile(string) error { return nil }
func (s *stubStructureDB) Close() error              { return nil }

func buildTestRouter() http.Handler {
	structDB := newStubStructureDB()
	for _, name := range []string{"my-bucket", "mybucket"} {
		structDB.buckets[name] = meta.Bucket{ID: name, Name: name, OwnerUserID: "u-1"}
	}
	deps := s3api.S3Deps{
		Auth:      &stubVerifier{accessKey: "TESTKEY"},
		Structure: structDB,
		Buckets:   newStubBucketService(),
		Objects:   &stubObjectService{},
		Multipart: &stubMultipartService{},
		Quota:     &stubQuotaService{},
		Region:    "us-east-1",
	}
	return s3api.NewRouter(deps)
}

func TestRouter_ListBuckets(t *testing.T) {
	h := buildTestRouter()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=TESTKEY/20240101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=fake")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("ListBuckets: got %d want 200", w.Code)
	}
	var result s3api.ListAllMyBucketsResult
	if err := xml.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestRouter_CreateBucket(t *testing.T) {
	h := buildTestRouter()
	req := httptest.NewRequest("PUT", "/new-bucket", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=TESTKEY/20240101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=fake")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("CreateBucket: got %d want 200", w.Code)
	}
}

func TestRouter_PutObject(t *testing.T) {
	h := buildTestRouter()
	body := strings.NewReader("hello")
	req := httptest.NewRequest("PUT", "/my-bucket/hello.txt", body)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=TESTKEY/20240101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=fake")
	req.Header.Set("Content-Type", "text/plain")
	req.ContentLength = 5
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("PutObject: got %d want 200, body: %s", w.Code, w.Body.String())
	}
}

func TestRouter_GetObject(t *testing.T) {
	h := buildTestRouter()
	req := httptest.NewRequest("GET", "/my-bucket/hello.txt", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=TESTKEY/20240101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=fake")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GetObject: got %d want 200", w.Code)
	}
	if w.Body.String() != "hello" {
		t.Errorf("GetObject body: got %q want hello", w.Body.String())
	}
}

func TestRouter_HeadObject(t *testing.T) {
	h := buildTestRouter()
	req := httptest.NewRequest("HEAD", "/my-bucket/hello.txt", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=TESTKEY/20240101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=fake")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("HeadObject: got %d want 200", w.Code)
	}
}

func TestRouter_DeleteObject(t *testing.T) {
	h := buildTestRouter()
	req := httptest.NewRequest("DELETE", "/my-bucket/hello.txt", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=TESTKEY/20240101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=fake")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("DeleteObject: got %d want 204", w.Code)
	}
}

func TestRouter_CreateMultipartUpload(t *testing.T) {
	h := buildTestRouter()
	req := httptest.NewRequest("POST", "/my-bucket/big.bin?uploads", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=TESTKEY/20240101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=fake")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("CreateMultipart: got %d want 200, body: %s", w.Code, w.Body.String())
	}
	var result s3api.InitiateMultipartUploadResult
	if err := xml.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.UploadID != "upload-id-123" {
		t.Errorf("UploadId: got %q", result.UploadID)
	}
}

func TestRouter_AuthFailure(t *testing.T) {
	// Build router with a verifier that always fails
	deps := s3api.S3Deps{
		Auth:      &failVerifier{},
		Structure: newStubStructureDB(),
		Buckets:   newStubBucketService(),
		Objects:   &stubObjectService{},
		Multipart: &stubMultipartService{},
		Quota:     &stubQuotaService{},
		Region:    "us-east-1",
	}
	h := s3api.NewRouter(deps)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("AuthFailure: got %d want 403", w.Code)
	}
}

func TestRouter_ListMultipartUploads(t *testing.T) {
	h := buildTestRouter()
	req := httptest.NewRequest("GET", "/mybucket?uploads", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=TESTKEY/20240101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=fake")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRouter_ListParts(t *testing.T) {
	h := buildTestRouter()
	req := httptest.NewRequest("GET", "/mybucket/mykey?uploadId=abc123", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=TESTKEY/20240101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=fake")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRouter_AccessDeniedForOtherOwnersBucket(t *testing.T) {
	structDB := newStubStructureDB()
	structDB.buckets["owned-by-other"] = meta.Bucket{ID: "owned-by-other", Name: "owned-by-other", OwnerUserID: "u-2"}
	deps := s3api.S3Deps{
		Auth:      &stubVerifier{accessKey: "TESTKEY"},
		Structure: structDB,
		Buckets:   newStubBucketService(),
		Objects:   &stubObjectService{},
		Multipart: &stubMultipartService{},
		Quota:     &stubQuotaService{},
		Region:    "us-east-1",
	}
	h := s3api.NewRouter(deps)

	req := httptest.NewRequest("GET", "/owned-by-other/object.txt", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=TESTKEY/20240101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=fake")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 AccessDenied, got %d: %s", w.Code, w.Body.String())
	}
}

// failVerifier always returns an error.
type failVerifier struct{}

func (f *failVerifier) Verify(r *http.Request, _ func(string) (string, error)) (string, error) {
	return "", fmt.Errorf("auth failed")
}
