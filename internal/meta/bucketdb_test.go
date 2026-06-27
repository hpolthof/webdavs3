package meta_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/hpolthof/webdav3s/internal/meta"
)

func openBucketDB(t *testing.T) meta.BucketDB {
	t.Helper()
	db, err := meta.OpenBucketDB(":memory:")
	if err != nil {
		t.Fatalf("OpenBucketDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestBucketDB_Objects(t *testing.T) {
	db := openBucketDB(t)

	obj := meta.Object{
		ID:             "obj-1",
		Key:            "photos/sunset.jpg",
		HashPath:       "/_data/ab/abcdef1234",
		SizeBytes:      1024,
		ETag:           "\"abc123\"",
		ContentType:    "image/jpeg",
		LastModified:   time.Now().Truncate(time.Second),
		UploadComplete: true,
	}
	if err := db.PutObject(obj); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got, err := db.GetObject("photos/sunset.jpg")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if got.SizeBytes != 1024 {
		t.Errorf("SizeBytes: got %d want 1024", got.SizeBytes)
	}
	if got.ETag != obj.ETag {
		t.Errorf("ETag: got %q want %q", got.ETag, obj.ETag)
	}

	// Overwrite
	obj2 := obj
	obj2.SizeBytes = 2048
	if err := db.PutObject(obj2); err != nil {
		t.Fatalf("PutObject overwrite: %v", err)
	}
	got2, _ := db.GetObject("photos/sunset.jpg")
	if got2.SizeBytes != 2048 {
		t.Errorf("overwrite SizeBytes: got %d want 2048", got2.SizeBytes)
	}

	if err := db.DeleteObject("photos/sunset.jpg"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	_, err = db.GetObject("photos/sunset.jpg")
	if err == nil {
		t.Fatal("expected error after DeleteObject")
	}
}

func TestBucketDB_ListObjects(t *testing.T) {
	db := openBucketDB(t)

	keys := []string{"a/1.txt", "a/2.txt", "b/1.txt", "c.txt"}
	for i, k := range keys {
		db.PutObject(meta.Object{
			ID: fmt.Sprintf("o%d", i), Key: k,
			HashPath: "/_data/xx/hash", SizeBytes: 10,
			ETag: "\"e\"", ContentType: "text/plain",
			LastModified: time.Now(), UploadComplete: true,
		})
	}

	// List all
	objs, prefixes, err := db.ListObjects("", "", "", 100)
	if err != nil {
		t.Fatalf("ListObjects all: %v", err)
	}
	if len(objs) != 4 {
		t.Errorf("all objects: got %d want 4", len(objs))
	}
	_ = prefixes

	// List with prefix
	objs2, _, err := db.ListObjects("a/", "", "", 100)
	if err != nil {
		t.Fatalf("ListObjects prefix: %v", err)
	}
	if len(objs2) != 2 {
		t.Errorf("prefix a/: got %d want 2", len(objs2))
	}

	// List with delimiter — common prefixes
	_, prefixes2, err := db.ListObjects("", "/", "", 100)
	if err != nil {
		t.Fatalf("ListObjects delimiter: %v", err)
	}
	if len(prefixes2) < 2 {
		t.Errorf("delimiter /: expected at least 2 common prefixes, got %v", prefixes2)
	}
}

func TestBucketDB_Multipart(t *testing.T) {
	db := openBucketDB(t)

	up := meta.MultipartUpload{
		ID:        "upload-1",
		Key:       "large.bin",
		Initiated: time.Now().Truncate(time.Second),
	}
	if err := db.CreateMultipartUpload(up); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	got, err := db.GetMultipartUpload("upload-1")
	if err != nil {
		t.Fatalf("GetMultipartUpload: %v", err)
	}
	if got.Key != "large.bin" {
		t.Errorf("Key: got %q", got.Key)
	}

	parts := []meta.MultipartPart{
		{UploadID: "upload-1", PartNumber: 1, HashPath: "/_data/aa/h1", SizeBytes: 5242880, ETag: "\"e1\""},
		{UploadID: "upload-1", PartNumber: 2, HashPath: "/_data/bb/h2", SizeBytes: 1024, ETag: "\"e2\""},
	}
	for _, p := range parts {
		if err := db.AddPart(p); err != nil {
			t.Fatalf("AddPart %d: %v", p.PartNumber, err)
		}
	}

	listed, err := db.ListParts("upload-1")
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if len(listed) != 2 {
		t.Errorf("ListParts count: got %d want 2", len(listed))
	}

	finalObj := meta.Object{
		ID: "obj-final", Key: "large.bin",
		HashPath: "/_data/cc/hfinal", SizeBytes: 5243904,
		ETag: "\"final\"", ContentType: "application/octet-stream",
		LastModified: time.Now(), UploadComplete: true,
	}
	if err := db.CompleteMultipartUpload("upload-1", finalObj); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	// Upload should be gone after completion
	_, err = db.GetMultipartUpload("upload-1")
	if err == nil {
		t.Fatal("expected error: upload should be deleted after completion")
	}
}

func TestBucketDB_AbortMultipart(t *testing.T) {
	db := openBucketDB(t)
	db.CreateMultipartUpload(meta.MultipartUpload{ID: "u2", Key: "x.bin", Initiated: time.Now()})
	db.AddPart(meta.MultipartPart{UploadID: "u2", PartNumber: 1, HashPath: "h", SizeBytes: 10, ETag: "e"})
	if err := db.AbortMultipartUpload("u2"); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	parts, _ := db.ListParts("u2")
	if len(parts) != 0 {
		t.Errorf("expected 0 parts after abort, got %d", len(parts))
	}
}

func TestBucketDB_ChunkedObject(t *testing.T) {
	db, err := meta.OpenBucketDB(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	chunks := []meta.ChunkRef{
		{PartNumber: 1, Path: "/_parts/up1/1", Size: 1024, MD5Hex: "aaa"},
		{PartNumber: 2, Path: "/_parts/up1/2", Size: 512, MD5Hex: "bbb"},
	}
	obj := meta.Object{
		ID:             "obj-1",
		Key:            "big.bin",
		HashPath:       "",
		SizeBytes:      1536,
		ETag:           `"etag-multi-2"`,
		ContentType:    "application/octet-stream",
		LastModified:   time.Now().Truncate(time.Second),
		UploadComplete: true,
		Chunks:         chunks,
	}
	if err := db.PutObject(obj); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := db.GetObject("big.bin")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(got.Chunks))
	}
	if got.Chunks[0].Path != "/_parts/up1/1" {
		t.Errorf("chunk[0].Path = %q, want /_parts/up1/1", got.Chunks[0].Path)
	}
	if got.Chunks[1].MD5Hex != "bbb" {
		t.Errorf("chunk[1].MD5Hex = %q, want bbb", got.Chunks[1].MD5Hex)
	}
}

func TestBucketDB_ListMultipartUploads(t *testing.T) {
	db, err := meta.OpenBucketDB(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	up := meta.MultipartUpload{ID: "up1", Key: "k1", ContentType: "video/mp4", Initiated: time.Now().Truncate(time.Second)}
	if err := db.CreateMultipartUpload(up); err != nil {
		t.Fatalf("create: %v", err)
	}
	ups, err := db.ListMultipartUploads()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ups) != 1 || ups[0].ContentType != "video/mp4" {
		t.Fatalf("want 1 upload with ContentType video/mp4, got %+v", ups)
	}
}

func TestLRUCache(t *testing.T) {
	opened := 0
	factory := func(id string) (meta.BucketDB, error) {
		opened++
		return meta.OpenBucketDB(":memory:")
	}
	cache := meta.NewLRUCache(2, factory)

	db1, release1, err := cache.Get("b1")
	if err != nil || db1 == nil {
		t.Fatalf("Get b1: %v", err)
	}
	release1()

	db2, release2, _ := cache.Get("b2")
	_ = db2
	release2()
	if opened != 2 {
		t.Errorf("expected 2 opens, got %d", opened)
	}

	// b1 again — should come from cache
	db1b, release1b, _ := cache.Get("b1")
	_ = db1b
	release1b()
	if opened != 2 {
		t.Errorf("expected still 2 opens, got %d", opened)
	}

	// b3 causes eviction of LRU (b2, since b1 was just accessed)
	_, release3, _ := cache.Get("b3")
	release3()
	if opened != 3 {
		t.Errorf("expected 3 opens after eviction, got %d", opened)
	}
}
