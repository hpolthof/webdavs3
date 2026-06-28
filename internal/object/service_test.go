package object_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpolthof/webdavs3/internal/meta"
	"github.com/hpolthof/webdavs3/internal/object"
	"github.com/hpolthof/webdavs3/internal/repair"
	wdv "github.com/hpolthof/webdavs3/internal/webdav"
)

// mockWDV — minimal in-memory WebDAV.
type mockWDV struct{ files map[string][]byte }

func newMockWDV() *mockWDV { return &mockWDV{files: map[string][]byte{}} }

func (m *mockWDV) Upload(_ context.Context, p string, r io.Reader, _ int64) error {
	d, _ := io.ReadAll(r)
	m.files[p] = d
	return nil
}
func (m *mockWDV) Download(_ context.Context, p string) (io.ReadCloser, error) {
	d, ok := m.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	return io.NopCloser(bytes.NewReader(d)), nil
}
func (m *mockWDV) Delete(_ context.Context, p string) error {
	delete(m.files, p)
	prefix := p + "/"
	for k := range m.files {
		if strings.HasPrefix(k, prefix) {
			delete(m.files, k)
		}
	}
	return nil
}
func (m *mockWDV) MkdirAll(_ context.Context, _ string) error { return nil }
func (m *mockWDV) Exists(_ context.Context, p string) (bool, error) {
	_, ok := m.files[p]
	return ok, nil
}
func (m *mockWDV) Rename(_ context.Context, oldpath, newpath string, _ bool) error {
	data, ok := m.files[oldpath]
	if !ok {
		return fmt.Errorf("not found: %s", oldpath)
	}
	delete(m.files, oldpath)
	m.files[newpath] = data
	return nil
}
func (m *mockWDV) DownloadToFile(_ context.Context, p, dest string) error {
	d, ok := m.files[p]
	if !ok {
		return fmt.Errorf("not found: %s", p)
	}
	return os.WriteFile(dest, d, 0600)
}
func (m *mockWDV) UploadFromFile(_ context.Context, p, src string) error {
	d, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	m.files[p] = d
	return nil
}
func (m *mockWDV) Ping(_ context.Context) error { return nil }

func (m *mockWDV) ReadDir(_ context.Context, path string) ([]string, error) {
	prefix := strings.TrimSuffix(path, "/") + "/"
	seen := map[string]struct{}{}
	for k := range m.files {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if idx := strings.Index(rest, "/"); idx >= 0 {
			seen[rest[:idx]] = struct{}{}
		} else {
			seen[rest] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	return names, nil
}
func (m *mockWDV) ReadDirInfo(_ context.Context, path string) ([]os.FileInfo, error) {
	names, err := m.ReadDir(context.Background(), path)
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(names))
	for _, name := range names {
		infos = append(infos, mockFileInfo{name: name})
	}
	return infos, nil
}
func (m *mockWDV) Stat(_ context.Context, _ string) (os.FileInfo, error) { return nil, nil }

var _ wdv.Client = (*mockWDV)(nil)

type mockFileInfo struct{ name string }

func (m mockFileInfo) Name() string       { return m.name }
func (m mockFileInfo) Size() int64        { return 0 }
func (m mockFileInfo) Mode() os.FileMode  { return 0 }
func (m mockFileInfo) ModTime() time.Time { return time.Time{} }
func (m mockFileInfo) IsDir() bool        { return false }
func (m mockFileInfo) Sys() any           { return nil }

func TestMockWDV_DeleteDirectory(t *testing.T) {
	wdc := newMockWDV()
	wdc.files["/_parts/up1/1"] = []byte("a")
	wdc.files["/_parts/up1/2"] = []byte("b")
	wdc.files["/_parts/up2/1"] = []byte("c")

	_ = wdc.Delete(context.Background(), "/_parts/up1")

	if _, ok := wdc.files["/_parts/up1/1"]; ok {
		t.Error("/_parts/up1/1 should be deleted")
	}
	if _, ok := wdc.files["/_parts/up1/2"]; ok {
		t.Error("/_parts/up1/2 should be deleted")
	}
	if _, ok := wdc.files["/_parts/up2/1"]; !ok {
		t.Error("/_parts/up2/1 should be kept")
	}
}

func setupObjectService(t *testing.T) (object.ObjectService, meta.StructureDB, meta.StatsDB, *mockWDV) {
	t.Helper()
	structDB, _ := meta.OpenStructureDB(":memory:")
	statsDB, _ := meta.OpenStatsDB(":memory:", "d1")
	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "http://dav", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 1 << 30, CreatedAt: time.Now(),
	})
	structDB.AddUser(meta.User{
		ID: "u-1", AccessKey: "AK", SecretKeyHash: "h",
		DisplayName: "Alice", Enabled: true, CreatedAt: time.Now(),
	})
	structDB.AddBucket(meta.Bucket{
		ID: "bkt-1", Name: "test-bucket", OwnerUserID: "u-1",
		WebDAVLocationID: "loc-1", CreatedAt: time.Now(),
	})
	wdc := newMockWDV()
	cache := meta.NewLRUCache(4, func(id string) (meta.BucketDB, error) {
		return meta.OpenBucketDB(":memory:")
	})
	svc := object.New(wdc, cache, statsDB, structDB, t.TempDir(), nil, 0, 0)
	t.Cleanup(func() { structDB.Close(); statsDB.Close() })
	return svc, structDB, statsDB, wdc
}

func TestObjectService_PutGet(t *testing.T) {
	svc, _, _, wdc := setupObjectService(t)
	ctx := context.Background()

	content := "hello object store"
	obj, err := svc.Put(ctx, "test-bucket", "hello.txt", "text/plain",
		int64(len(content)), strings.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if obj.SizeBytes != int64(len(content)) {
		t.Errorf("SizeBytes: got %d want %d", obj.SizeBytes, len(content))
	}
	if obj.ETag == "" {
		t.Error("ETag should not be empty")
	}

	// Data file should exist on mock WebDAV
	dataPath := "/_data/" + obj.HashPath[len("/_data/"):]
	_ = dataPath
	found := false
	for k := range wdc.files {
		if len(k) > 6 && k[:6] == "/_data" {
			found = true
		}
	}
	if !found {
		t.Error("expected data file on mock WebDAV under /_data/")
	}

	gotObj, rc, err := svc.Get(ctx, "test-bucket", "hello.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	gotData, _ := io.ReadAll(rc)
	if string(gotData) != content {
		t.Errorf("Get content: got %q want %q", gotData, content)
	}
	if gotObj.Key != "hello.txt" {
		t.Errorf("Get key: got %q want hello.txt", gotObj.Key)
	}
}

func TestObjectService_PutBlockedWhenBucketRepairing(t *testing.T) {
	structDB, _ := meta.OpenStructureDB(":memory:")
	statsDB, _ := meta.OpenStatsDB(":memory:", "repairing")
	defer structDB.Close()
	defer statsDB.Close()
	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "http://dav", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 1 << 30, CreatedAt: time.Now(),
	})
	structDB.AddBucket(meta.Bucket{
		ID: "bkt-1", Name: "test-bucket", OwnerUserID: "u-1",
		WebDAVLocationID: "loc-1", CreatedAt: time.Now(),
	})

	wdc := newMockWDV()
	cache := meta.NewLRUCache(4, func(id string) (meta.BucketDB, error) {
		return meta.OpenBucketDB(":memory:")
	})
	repairMgr := repair.NewManager()
	repairMgr.MarkRepairing("bkt-1", "remote metadata corrupt")
	svc := object.NewWithRepair(wdc, cache, statsDB, structDB, t.TempDir(), nil, 0, 0, repairMgr)

	_, err := svc.Put(context.Background(), "test-bucket", "blocked.txt", "text/plain", 4, strings.NewReader("data"))
	if !errors.Is(err, repair.ErrUnavailable) {
		t.Fatalf("Put err = %v, want ErrUnavailable", err)
	}
	for path := range wdc.files {
		if strings.HasPrefix(path, "/_data/") {
			t.Fatalf("blocked Put uploaded object data to %s", path)
		}
	}
}

func TestObjectService_Head(t *testing.T) {
	svc, _, _, _ := setupObjectService(t)
	ctx := context.Background()

	svc.Put(ctx, "test-bucket", "meta.txt", "text/plain", 5, strings.NewReader("hello"))

	obj, err := svc.Head(ctx, "test-bucket", "meta.txt")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if obj.SizeBytes != 5 {
		t.Errorf("Head SizeBytes: got %d want 5", obj.SizeBytes)
	}
}

func TestObjectService_Delete(t *testing.T) {
	svc, _, _, _ := setupObjectService(t)
	ctx := context.Background()

	svc.Put(ctx, "test-bucket", "del.txt", "text/plain", 3, strings.NewReader("bye"))

	if err := svc.Delete(ctx, "test-bucket", "del.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, _, err := svc.Get(ctx, "test-bucket", "del.txt")
	if err == nil {
		t.Fatal("expected error after Delete, got nil")
	}
}

func TestObjectService_DeleteCleansDataFile(t *testing.T) {
	svc, _, _, wdc := setupObjectService(t)
	ctx := context.Background()

	obj, err := svc.Put(ctx, "test-bucket", "file.txt", "text/plain", 5, strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := wdc.files[obj.HashPath]; !ok {
		t.Fatal("data file should exist before delete")
	}

	if err := svc.Delete(ctx, "test-bucket", "file.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := wdc.files[obj.HashPath]; ok {
		t.Error("data file should be deleted when no other key references it")
	}
}

func TestObjectService_CleansStaleTempFilesOnStartup(t *testing.T) {
	dir := t.TempDir()

	stale := filepath.Join(dir, "put-1234567890.bin")
	recent := filepath.Join(dir, "put-0987654321.bin")
	other := filepath.Join(dir, "other-file.bin")

	if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
		t.Fatalf("write stale temp: %v", err)
	}
	oldTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(stale, oldTime, oldTime); err != nil {
		t.Fatalf("set stale mtime: %v", err)
	}

	if err := os.WriteFile(recent, []byte("recent"), 0600); err != nil {
		t.Fatalf("write recent temp: %v", err)
	}
	if err := os.WriteFile(other, []byte("other"), 0600); err != nil {
		t.Fatalf("write other file: %v", err)
	}

	structDB, _ := meta.OpenStructureDB(":memory:")
	statsDB, _ := meta.OpenStatsDB(":memory:", "cleanup")
	defer structDB.Close()
	defer statsDB.Close()
	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "http://dav", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 1 << 30, CreatedAt: time.Now(),
	})
	wdc := newMockWDV()
	cache := meta.NewLRUCache(4, func(id string) (meta.BucketDB, error) {
		return meta.OpenBucketDB(":memory:")
	})
	_ = object.New(wdc, cache, statsDB, structDB, dir, nil, 0, 0)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale put-*.bin should be removed, got err=%v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent put-*.bin should be kept: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("non-matching file should be kept: %v", err)
	}
}

func TestObjectService_DeleteDedupGuard(t *testing.T) {
	svc, _, _, wdc := setupObjectService(t)
	ctx := context.Background()

	content := "shared content"
	objA, _ := svc.Put(ctx, "test-bucket", "a.txt", "text/plain", int64(len(content)), strings.NewReader(content))
	objB, _ := svc.Put(ctx, "test-bucket", "b.txt", "text/plain", int64(len(content)), strings.NewReader(content))
	if objA.HashPath != objB.HashPath {
		t.Skip("content not deduplicated — SHA-256 paths differ unexpectedly")
	}

	if err := svc.Delete(ctx, "test-bucket", "a.txt"); err != nil {
		t.Fatalf("Delete a.txt: %v", err)
	}
	if _, ok := wdc.files[objA.HashPath]; !ok {
		t.Error("data file must NOT be deleted while b.txt still references it")
	}

	if err := svc.Delete(ctx, "test-bucket", "b.txt"); err != nil {
		t.Fatalf("Delete b.txt: %v", err)
	}
	if _, ok := wdc.files[objA.HashPath]; ok {
		t.Error("data file should be deleted after last reference is removed")
	}
}

func TestObjectService_DeleteChunkFiles(t *testing.T) {
	structDB, _ := meta.OpenStructureDB(":memory:")
	statsDB, _ := meta.OpenStatsDB(":memory:", "dc")
	defer structDB.Close()
	defer statsDB.Close()

	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "http://dav", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 1 << 30, CreatedAt: time.Now(),
	})
	structDB.AddUser(meta.User{
		ID: "u-1", AccessKey: "AK", SecretKeyHash: "h",
		DisplayName: "Alice", Enabled: true, CreatedAt: time.Now(),
	})
	structDB.AddBucket(meta.Bucket{
		ID: "bkt-1", Name: "test-bucket", OwnerUserID: "u-1",
		WebDAVLocationID: "loc-1", CreatedAt: time.Now(),
	})

	wdc := newMockWDV()
	cache := meta.NewLRUCache(4, func(id string) (meta.BucketDB, error) {
		return meta.OpenBucketDB(":memory:")
	})
	dir := t.TempDir()
	svc := object.New(wdc, cache, statsDB, structDB, dir, nil, 0, 0)
	mp := object.NewMultipartService(wdc, cache, statsDB, structDB, dir)
	ctx := context.Background()

	uploadID, _ := mp.Create(ctx, "test-bucket", "big.bin", "application/octet-stream")
	etag1, _ := mp.UploadPart(ctx, "test-bucket", "big.bin", uploadID, 1, 3, strings.NewReader("aaa"))
	etag2, _ := mp.UploadPart(ctx, "test-bucket", "big.bin", uploadID, 2, 3, strings.NewReader("bbb"))
	obj, err := mp.Complete(ctx, "test-bucket", "big.bin", uploadID, []object.CompletePart{
		{PartNumber: 1, ETag: etag1},
		{PartNumber: 2, ETag: etag2},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for _, c := range obj.Chunks {
		if _, ok := wdc.files[c.Path]; !ok {
			t.Errorf("chunk %s should exist before delete", c.Path)
		}
	}

	if err := svc.Delete(ctx, "test-bucket", "big.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, c := range obj.Chunks {
		if _, ok := wdc.files[c.Path]; ok {
			t.Errorf("chunk %s should be deleted after object delete", c.Path)
		}
	}
}

func TestObjectService_GarbageCollect(t *testing.T) {
	newGCSetup := func(t *testing.T) (object.GarbageCollector, *mockWDV, object.MultipartService) {
		t.Helper()
		structDB, _ := meta.OpenStructureDB(":memory:")
		statsDB, _ := meta.OpenStatsDB(":memory:", "gc1")
		t.Cleanup(func() { structDB.Close(); statsDB.Close() })
		structDB.AddLocation(meta.Location{
			ID: "loc-1", URL: "http://dav", Username: "u", PasswordEnc: "e",
			DisplayName: "L", QuotaBytes: 1 << 30, CreatedAt: time.Now(),
		})
		structDB.AddUser(meta.User{
			ID: "u-1", AccessKey: "AK", SecretKeyHash: "h",
			DisplayName: "Alice", Enabled: true, CreatedAt: time.Now(),
		})
		structDB.AddBucket(meta.Bucket{
			ID: "bkt-1", Name: "test-bucket", OwnerUserID: "u-1",
			WebDAVLocationID: "loc-1", CreatedAt: time.Now(),
		})
		wdc := newMockWDV()
		cache := meta.NewLRUCache(4, func(id string) (meta.BucketDB, error) {
			return meta.OpenBucketDB(":memory:")
		})
		dir := t.TempDir()
		svc := object.New(wdc, cache, statsDB, structDB, dir, nil, 0, 0)
		mp := object.NewMultipartService(wdc, cache, statsDB, structDB, dir)
		gc, ok := svc.(object.GarbageCollector)
		if !ok {
			t.Fatal("object service does not implement GarbageCollector")
		}
		return gc, wdc, mp
	}

	t.Run("orphan _data file deleted", func(t *testing.T) {
		gc, wdc, _ := newGCSetup(t)
		orphan := "/_data/de/deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
		wdc.files[orphan] = []byte("stale data")
		deleted, err := gc.GarbageCollect(context.Background(), "loc-1")
		if err != nil {
			t.Fatalf("GarbageCollect: %v", err)
		}
		if deleted != 1 {
			t.Errorf("expected 1 deleted, got %d", deleted)
		}
		if _, ok := wdc.files[orphan]; ok {
			t.Error("orphan /_data/ file should be deleted")
		}
	})

	t.Run("orphan _parts dir deleted", func(t *testing.T) {
		gc, wdc, _ := newGCSetup(t)
		wdc.files["/_parts/dead-upload-id/1"] = []byte("stale part")
		wdc.files["/_parts/dead-upload-id/2"] = []byte("stale part 2")
		deleted, err := gc.GarbageCollect(context.Background(), "loc-1")
		if err != nil {
			t.Fatalf("GarbageCollect: %v", err)
		}
		if deleted != 1 {
			t.Errorf("expected 1 deleted, got %d", deleted)
		}
		if _, ok := wdc.files["/_parts/dead-upload-id/1"]; ok {
			t.Error("orphan part file should be deleted")
		}
	})

	t.Run("active upload _parts dir kept", func(t *testing.T) {
		gc, wdc, mp := newGCSetup(t)
		ctx := context.Background()
		uploadID, _ := mp.Create(ctx, "test-bucket", "big.bin", "application/octet-stream")
		mp.UploadPart(ctx, "test-bucket", "big.bin", uploadID, 1, 3, strings.NewReader("aaa"))
		partPath := fmt.Sprintf("/_parts/%s/1", uploadID)

		deleted, err := gc.GarbageCollect(ctx, "loc-1")
		if err != nil {
			t.Fatalf("GarbageCollect: %v", err)
		}
		if deleted != 0 {
			t.Errorf("expected 0 deleted, got %d", deleted)
		}
		if _, ok := wdc.files[partPath]; !ok {
			t.Errorf("active upload part %s should not be deleted", partPath)
		}
	})

	t.Run("completed multipart object chunks kept", func(t *testing.T) {
		gc, wdc, mp := newGCSetup(t)
		ctx := context.Background()
		uploadID, _ := mp.Create(ctx, "test-bucket", "big.bin", "application/octet-stream")
		etag1, _ := mp.UploadPart(ctx, "test-bucket", "big.bin", uploadID, 1, 3, strings.NewReader("aaa"))
		etag2, _ := mp.UploadPart(ctx, "test-bucket", "big.bin", uploadID, 2, 3, strings.NewReader("bbb"))
		mpObj, err := mp.Complete(ctx, "test-bucket", "big.bin", uploadID, []object.CompletePart{
			{PartNumber: 1, ETag: etag1},
			{PartNumber: 2, ETag: etag2},
		})
		if err != nil {
			t.Fatalf("Complete multipart: %v", err)
		}

		deleted, err := gc.GarbageCollect(ctx, "loc-1")
		if err != nil {
			t.Fatalf("GarbageCollect: %v", err)
		}
		if deleted != 0 {
			t.Errorf("expected 0 deleted, got %d", deleted)
		}
		for _, c := range mpObj.Chunks {
			if _, ok := wdc.files[c.Path]; !ok {
				t.Errorf("chunk %s should not be deleted by GC", c.Path)
			}
		}
	})

	t.Run("missing _parts dir is not an error", func(t *testing.T) {
		gc, _, _ := newGCSetup(t)
		_, err := gc.GarbageCollect(context.Background(), "loc-1")
		if err != nil {
			t.Fatalf("GarbageCollect with no _parts dir should not error: %v", err)
		}
	})
}

func TestObjectService_GetNotFound(t *testing.T) {
	svc, _, _, _ := setupObjectService(t)
	ctx := context.Background()

	_, _, err := svc.Get(ctx, "test-bucket", "nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for missing object")
	}
}

func TestMultipartService_FullFlow(t *testing.T) {
	svc, structDB, _, _ := setupObjectService(t)
	_ = svc
	_ = structDB
	ctx := context.Background()

	// Wrap the same cache + wdc used by svc — we need a MultipartService.
	// setupObjectService returns an ObjectService; build a separate MultipartService
	// sharing the same dependencies.
	wdc2 := newMockWDV()
	cache2 := meta.NewLRUCache(4, func(id string) (meta.BucketDB, error) {
		return meta.OpenBucketDB(":memory:")
	})
	statsDB2, _ := meta.OpenStatsDB(":memory:", "d2")
	defer statsDB2.Close()

	// Re-open a structure DB with the same bucket for the multipart service.
	structDB2, _ := meta.OpenStructureDB(":memory:")
	defer structDB2.Close()
	structDB2.AddLocation(meta.Location{
		ID: "loc-1", URL: "http://dav", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 1 << 30, CreatedAt: time.Now(),
	})
	structDB2.AddUser(meta.User{
		ID: "u-1", AccessKey: "AK", SecretKeyHash: "h",
		DisplayName: "Alice", Enabled: true, CreatedAt: time.Now(),
	})
	structDB2.AddBucket(meta.Bucket{
		ID: "bkt-1", Name: "test-bucket", OwnerUserID: "u-1",
		WebDAVLocationID: "loc-1", CreatedAt: time.Now(),
	})

	mp := object.NewMultipartService(wdc2, cache2, statsDB2, structDB2, t.TempDir())

	uploadID, err := mp.Create(ctx, "test-bucket", "large.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if uploadID == "" {
		t.Fatal("expected non-empty uploadID")
	}

	// Upload two parts (min 5 MiB each in real S3 but we test with small data).
	part1Data := strings.Repeat("A", 1024)
	etag1, err := mp.UploadPart(ctx, "test-bucket", "large.bin", uploadID, 1,
		int64(len(part1Data)), strings.NewReader(part1Data))
	if err != nil {
		t.Fatalf("UploadPart 1: %v", err)
	}

	part2Data := strings.Repeat("B", 512)
	etag2, err := mp.UploadPart(ctx, "test-bucket", "large.bin", uploadID, 2,
		int64(len(part2Data)), strings.NewReader(part2Data))
	if err != nil {
		t.Fatalf("UploadPart 2: %v", err)
	}

	finalObj, err := mp.Complete(ctx, "test-bucket", "large.bin", uploadID, []object.CompletePart{
		{PartNumber: 1, ETag: etag1},
		{PartNumber: 2, ETag: etag2},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if finalObj.SizeBytes != int64(len(part1Data)+len(part2Data)) {
		t.Errorf("final SizeBytes: got %d want %d", finalObj.SizeBytes, len(part1Data)+len(part2Data))
	}
	if finalObj.Key != "large.bin" {
		t.Errorf("final Key: got %q want large.bin", finalObj.Key)
	}
}

func TestMultipartService_UploadPartBlockedWhenBucketRepairing(t *testing.T) {
	wdc := newMockWDV()
	cache := meta.NewLRUCache(4, func(id string) (meta.BucketDB, error) {
		return meta.OpenBucketDB(":memory:")
	})
	statsDB, _ := meta.OpenStatsDB(":memory:", "mp-repairing")
	defer statsDB.Close()
	structDB, _ := meta.OpenStructureDB(":memory:")
	defer structDB.Close()
	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "http://dav", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 0, CreatedAt: time.Now(),
	})
	structDB.AddBucket(meta.Bucket{
		ID: "bkt-2", Name: "repair-bucket", OwnerUserID: "u-1",
		WebDAVLocationID: "loc-1", CreatedAt: time.Now(),
	})

	repairMgr := repair.NewManager()
	mp := object.NewMultipartServiceWithRepair(wdc, cache, statsDB, structDB, t.TempDir(), repairMgr)
	uploadID, err := mp.Create(context.Background(), "repair-bucket", "x.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	repairMgr.MarkRepairing("bkt-2", "remote metadata corrupt")
	_, err = mp.UploadPart(context.Background(), "repair-bucket", "x.bin", uploadID, 1, 3, strings.NewReader("abc"))
	if !errors.Is(err, repair.ErrUnavailable) {
		t.Fatalf("UploadPart err = %v, want ErrUnavailable", err)
	}
	for path := range wdc.files {
		if strings.HasPrefix(path, "/_parts/") {
			t.Fatalf("blocked UploadPart uploaded part data to %s", path)
		}
	}
}

func TestMultipartService_Abort(t *testing.T) {
	wdc := newMockWDV()
	cache := meta.NewLRUCache(4, func(id string) (meta.BucketDB, error) {
		return meta.OpenBucketDB(":memory:")
	})
	statsDB, _ := meta.OpenStatsDB(":memory:", "d3")
	defer statsDB.Close()
	structDB, _ := meta.OpenStructureDB(":memory:")
	defer structDB.Close()
	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "http://dav", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 0, CreatedAt: time.Now(),
	})
	structDB.AddBucket(meta.Bucket{
		ID: "bkt-2", Name: "abort-bucket", OwnerUserID: "u-1",
		WebDAVLocationID: "loc-1", CreatedAt: time.Now(),
	})

	mp := object.NewMultipartService(wdc, cache, statsDB, structDB, t.TempDir())
	ctx := context.Background()

	uploadID, _ := mp.Create(ctx, "abort-bucket", "x.bin", "application/octet-stream")
	mp.UploadPart(ctx, "abort-bucket", "x.bin", uploadID, 1, 3, strings.NewReader("abc"))

	if err := mp.Abort(ctx, "abort-bucket", uploadID); err != nil {
		t.Fatalf("Abort: %v", err)
	}
}

func TestObjectService_List(t *testing.T) {
	svc, _, _, _ := setupObjectService(t)
	ctx := context.Background()

	keys := []string{"photos/2024/a.jpg", "photos/2024/b.jpg", "photos/2025/c.jpg", "docs/readme.txt"}
	for _, k := range keys {
		_, err := svc.Put(ctx, "test-bucket", k, "application/octet-stream", 4, strings.NewReader("data"))
		if err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}

	// List all
	result, err := svc.List(ctx, "test-bucket", "", "", "", 100)
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(result.Objects) != 4 {
		t.Errorf("List all: got %d want 4", len(result.Objects))
	}

	// List with prefix
	result2, err := svc.List(ctx, "test-bucket", "photos/", "", "", 100)
	if err != nil {
		t.Fatalf("List prefix: %v", err)
	}
	if len(result2.Objects) != 3 {
		t.Errorf("List prefix photos/: got %d want 3", len(result2.Objects))
	}

	// List with prefix + delimiter → common prefixes
	result3, err := svc.List(ctx, "test-bucket", "photos/", "/", "", 100)
	if err != nil {
		t.Fatalf("List delimiter: %v", err)
	}
	if len(result3.Objects) != 0 {
		t.Errorf("List delimiter: expected 0 direct objects, got %d", len(result3.Objects))
	}
	if len(result3.CommonPrefixes) != 2 {
		t.Errorf("List delimiter: expected 2 common prefixes, got %v", result3.CommonPrefixes)
	}

	// Truncation: maxKeys=2
	result4, err := svc.List(ctx, "test-bucket", "", "", "", 2)
	if err != nil {
		t.Fatalf("List truncated: %v", err)
	}
	if len(result4.Objects) != 2 {
		t.Errorf("List truncated: got %d want 2", len(result4.Objects))
	}
	if !result4.IsTruncated {
		t.Error("expected IsTruncated=true")
	}
	if result4.NextContinuationToken == "" {
		t.Error("expected NextContinuationToken to be set")
	}

	// Continue from token
	result5, err := svc.List(ctx, "test-bucket", "", "", result4.NextContinuationToken, 100)
	if err != nil {
		t.Fatalf("List continuation: %v", err)
	}
	if len(result5.Objects) != 2 {
		t.Errorf("List continuation: got %d want 2", len(result5.Objects))
	}
}

func TestObjectService_GetChunked(t *testing.T) {
	// Build a mock webdav client that serves two part files.
	wdc := newMockWDV()
	wdc.files["/_parts/up1/1"] = []byte("hello ")
	wdc.files["/_parts/up1/2"] = []byte("world")

	// Pre-populate BucketDB with a chunked object.
	structDB, _ := meta.OpenStructureDB(":memory:")
	defer structDB.Close()
	statsDB, _ := meta.OpenStatsDB(":memory:", "d-chunked")
	defer statsDB.Close()
	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "http://dav", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 1 << 30, CreatedAt: time.Now(),
	})
	structDB.AddUser(meta.User{
		ID: "u-1", AccessKey: "AK", SecretKeyHash: "h",
		DisplayName: "Alice", Enabled: true, CreatedAt: time.Now(),
	})
	structDB.AddBucket(meta.Bucket{
		ID: "bkt-chunked", Name: "chunk-bucket", OwnerUserID: "u-1",
		WebDAVLocationID: "loc-1", CreatedAt: time.Now(),
	})

	cache := meta.NewLRUCache(4, func(id string) (meta.BucketDB, error) {
		return meta.OpenBucketDB(":memory:")
	})

	// Pre-populate the bucket DB with a chunked object.
	bdb, release, err := cache.Get("bkt-chunked")
	if err != nil {
		t.Fatalf("open bdb: %v", err)
	}
	chunks := []meta.ChunkRef{
		{PartNumber: 1, Path: "/_parts/up1/1", Size: 6},
		{PartNumber: 2, Path: "/_parts/up1/2", Size: 5},
	}
	obj := meta.Object{
		ID: "obj1", Key: "hello.txt", HashPath: "", SizeBytes: 11,
		ETag: `"etag"`, ContentType: "text/plain",
		LastModified: time.Now(), UploadComplete: true, Chunks: chunks,
	}
	if err := bdb.PutObject(obj); err != nil {
		release()
		t.Fatalf("put: %v", err)
	}
	release()

	svc := object.New(wdc, cache, statsDB, structDB, t.TempDir(), nil, 0, 0)
	_, rc, err := svc.Get(context.Background(), "chunk-bucket", "hello.txt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()

	data, _ := io.ReadAll(rc)
	if string(data) != "hello world" {
		t.Errorf("got %q, want %q", string(data), "hello world")
	}
}

type recordingMultipartService struct {
	completeCalled bool
	mu             sync.Mutex
}

func (r *recordingMultipartService) Create(_ context.Context, _, _, _ string) (string, error) {
	return "test-upload-id", nil
}
func (r *recordingMultipartService) UploadPart(_ context.Context, _, _, _ string, partNum int, _ int64, body io.Reader) (string, error) {
	io.Copy(io.Discard, body)
	return `"fakeetag"`, nil
}
func (r *recordingMultipartService) Complete(_ context.Context, _, _, _ string, parts []object.CompletePart) (meta.Object, error) {
	r.mu.Lock()
	r.completeCalled = true
	r.mu.Unlock()
	return meta.Object{Key: "file.txt", ETag: `"final"`, SizeBytes: 12, LastModified: time.Now(), UploadComplete: true}, nil
}
func (r *recordingMultipartService) Abort(_ context.Context, _, _ string) error { return nil }
func (r *recordingMultipartService) ListUploads(_ context.Context, _ string) ([]meta.MultipartUpload, error) {
	return nil, nil
}
func (r *recordingMultipartService) ListParts(_ context.Context, _, _ string) ([]meta.MultipartPart, error) {
	return nil, nil
}

func TestObjectService_PutAutoChunks(t *testing.T) {
	wdc := newMockWDV()
	structDB, _ := meta.OpenStructureDB(":memory:")
	defer structDB.Close()
	statsDB, _ := meta.OpenStatsDB(":memory:", "d-autochunk")
	defer statsDB.Close()

	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "http://dav", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 1 << 30, CreatedAt: time.Now(),
	})
	structDB.AddUser(meta.User{
		ID: "u-1", AccessKey: "AK", SecretKeyHash: "h",
		DisplayName: "Alice", Enabled: true, CreatedAt: time.Now(),
	})
	structDB.AddBucket(meta.Bucket{
		ID: "bkt-autochunk", Name: "testbucket", OwnerUserID: "u-1",
		WebDAVLocationID: "loc-1", CreatedAt: time.Now(),
	})

	cache := meta.NewLRUCache(4, func(id string) (meta.BucketDB, error) {
		return meta.OpenBucketDB(":memory:")
	})

	recorded := &recordingMultipartService{}
	// Threshold: 10 bytes; chunk size: 5 bytes
	svc := object.New(wdc, cache, statsDB, structDB, t.TempDir(), recorded, 10, 5)

	body := strings.NewReader("hello world!") // 12 bytes > 10 threshold
	_, err := svc.Put(context.Background(), "testbucket", "file.txt", "text/plain", 12, body)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !recorded.completeCalled {
		t.Error("expected multipart Complete to be called for auto-chunk")
	}
}

func TestObjectService_DeleteChunked(t *testing.T) {
	wdc := newMockWDV()
	wdc.files["/_parts/up1/1"] = []byte("hello ")
	wdc.files["/_parts/up1/2"] = []byte("world")

	structDB, _ := meta.OpenStructureDB(":memory:")
	defer structDB.Close()
	statsDB, _ := meta.OpenStatsDB(":memory:", "d-del-chunked")
	defer statsDB.Close()
	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "http://dav", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 1 << 30, CreatedAt: time.Now(),
	})
	structDB.AddUser(meta.User{
		ID: "u-1", AccessKey: "AK", SecretKeyHash: "h",
		DisplayName: "Alice", Enabled: true, CreatedAt: time.Now(),
	})
	structDB.AddBucket(meta.Bucket{
		ID: "bkt-del-chunked", Name: "del-chunk-bucket", OwnerUserID: "u-1",
		WebDAVLocationID: "loc-1", CreatedAt: time.Now(),
	})

	cache := meta.NewLRUCache(4, func(id string) (meta.BucketDB, error) {
		return meta.OpenBucketDB(":memory:")
	})

	bdb, release, err := cache.Get("bkt-del-chunked")
	if err != nil {
		t.Fatalf("open bdb: %v", err)
	}
	chunks := []meta.ChunkRef{
		{PartNumber: 1, Path: "/_parts/up1/1", Size: 6},
		{PartNumber: 2, Path: "/_parts/up1/2", Size: 5},
	}
	obj := meta.Object{
		ID: "obj1", Key: "hello.txt", HashPath: "", SizeBytes: 11,
		ETag: `"etag"`, ContentType: "text/plain",
		LastModified: time.Now(), UploadComplete: true, Chunks: chunks,
	}
	if err := bdb.PutObject(obj); err != nil {
		release()
		t.Fatalf("put: %v", err)
	}
	release()

	svc := object.New(wdc, cache, statsDB, structDB, t.TempDir(), nil, 0, 0)
	if err := svc.Delete(context.Background(), "del-chunk-bucket", "hello.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Both chunk files should be deleted from WebDAV.
	if _, ok := wdc.files["/_parts/up1/1"]; ok {
		t.Error("chunk 1 still exists after delete")
	}
	if _, ok := wdc.files["/_parts/up1/2"]; ok {
		t.Error("chunk 2 still exists after delete")
	}
}

func TestObjectService_DebugLogsObjectActions(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	svc, _, _, _ := setupObjectService(t)
	ctx := context.Background()

	if _, err := svc.Put(ctx, "test-bucket", "log.txt", "text/plain", 5, strings.NewReader("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rcObj, rc, err := svc.Get(ctx, "test-bucket", "log.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = rcObj
	rc.Close()
	if _, err := svc.Head(ctx, "test-bucket", "log.txt"); err != nil {
		t.Fatalf("Head: %v", err)
	}
	if _, err := svc.List(ctx, "test-bucket", "", "", "", 100); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := svc.Delete(ctx, "test-bucket", "log.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got := logs.String()
	for _, want := range []string{
		`msg="object put started"`,
		`msg="object put completed"`,
		`msg="object get started"`,
		`msg="object get completed"`,
		`msg="object head completed"`,
		`msg="object list completed"`,
		`msg="object delete started"`,
		`msg="object delete completed"`,
		`bucket=test-bucket`,
		`bucket_id=bkt-1`,
		`key=log.txt`,
		`size=5`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("debug logs missing %q\nlogs:\n%s", want, got)
		}
	}
}
