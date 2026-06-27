package s3api_test

import (
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hpolthof/webdav3s/internal/s3api"
)

func TestWriteS3Error_XML(t *testing.T) {
	w := httptest.NewRecorder()
	s3api.WriteS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", "req-123", 404)

	if w.Code != 404 {
		t.Errorf("status: got %d want 404", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "xml") {
		t.Errorf("Content-Type: got %q, want xml", ct)
	}

	var e s3api.ErrorResponse
	if err := xml.NewDecoder(w.Body).Decode(&e); err != nil {
		t.Fatalf("decode XML: %v", err)
	}
	if e.Code != "NoSuchBucket" {
		t.Errorf("Code: got %q want NoSuchBucket", e.Code)
	}
	if e.Message != "The specified bucket does not exist." {
		t.Errorf("Message: got %q", e.Message)
	}
	if e.RequestID != "req-123" {
		t.Errorf("RequestID: got %q want req-123", e.RequestID)
	}
}

func TestListAllMyBucketsResult_Marshal(t *testing.T) {
	r := s3api.ListAllMyBucketsResult{
		Owner: s3api.Owner{ID: "u-1", DisplayName: "Alice"},
		Buckets: []s3api.BucketEntry{
			{Name: "my-bucket", CreationDate: "2024-01-01T00:00:00.000Z"},
		},
	}
	data, err := xml.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "my-bucket") {
		t.Errorf("expected bucket name in XML: %s", data)
	}
	if !strings.Contains(string(data), "Alice") {
		t.Errorf("expected owner DisplayName in XML: %s", data)
	}
}

func TestListBucketResult_Marshal(t *testing.T) {
	r := s3api.ListBucketResult{
		Name:        "test",
		Prefix:      "",
		MaxKeys:     1000,
		IsTruncated: false,
		Contents: []s3api.ObjectEntry{
			{Key: "hello.txt", ETag: `"abc"`, Size: 5, StorageClass: "STANDARD"},
		},
	}
	data, err := xml.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "hello.txt") {
		t.Errorf("expected key in XML: %s", data)
	}
}
