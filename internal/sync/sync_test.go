package sync_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hpolthof/webdavs3/internal/meta"
	msync "github.com/hpolthof/webdavs3/internal/sync"
	wdv "github.com/hpolthof/webdavs3/internal/webdav"
)

// mockWDV is a minimal in-memory WebDAV client for testing.
type mockWDV struct {
	files map[string][]byte
}

func newMockWDV() *mockWDV { return &mockWDV{files: map[string][]byte{}} }

func (m *mockWDV) Upload(_ context.Context, path string, r io.Reader, _ int64) error {
	data, _ := io.ReadAll(r)
	m.files[path] = data
	return nil
}
func (m *mockWDV) Download(_ context.Context, path string) (io.ReadCloser, error) {
	data, ok := m.files[path]
	if !ok {
		return nil, fmt.Errorf("not found: %s", path)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
func (m *mockWDV) Delete(_ context.Context, path string) error { delete(m.files, path); return nil }
func (m *mockWDV) MkdirAll(_ context.Context, _ string) error  { return nil }
func (m *mockWDV) Exists(_ context.Context, path string) (bool, error) {
	_, ok := m.files[path]
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
func (m *mockWDV) DownloadToFile(_ context.Context, path, dest string) error {
	data, ok := m.files[path]
	if !ok {
		return fmt.Errorf("not found: %s", path)
	}
	return os.WriteFile(dest, data, 0600)
}
func (m *mockWDV) UploadFromFile(_ context.Context, path, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	m.files[path] = data
	return nil
}
func (m *mockWDV) ReadDir(_ context.Context, dir string) ([]string, error) {
	prefix := strings.TrimRight(dir, "/") + "/"
	seen := map[string]struct{}{}
	for file := range m.files {
		if !strings.HasPrefix(file, prefix) {
			continue
		}
		rest := strings.TrimPrefix(file, prefix)
		if rest == "" || strings.Contains(rest, "/") {
			continue
		}
		seen[rest] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	return names, nil
}
func (m *mockWDV) ReadDirInfo(_ context.Context, _ string) ([]os.FileInfo, error) { return nil, nil }
func (m *mockWDV) Ping(_ context.Context) error                                   { return nil }
func (m *mockWDV) Stat(_ context.Context, _ string) (os.FileInfo, error)          { return nil, nil }

var _ wdv.Client = (*mockWDV)(nil)

func TestEngine_SyncFromWebDAV(t *testing.T) {
	// Build a structure.db on the mock WebDAV.
	srcDB, _ := meta.OpenStructureDB(":memory:")
	srcDB.AddLocation(meta.Location{
		ID: "loc-synced", URL: "http://dav", Username: "u",
		PasswordEnc: "e", DisplayName: "Synced", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})

	wdc := newMockWDV()

	// Persist srcDB to a temp file and store it on the mock WebDAV.
	tmp, _ := os.CreateTemp(t.TempDir(), "struct*.db")
	tmp.Close()
	srcDB.SaveToFile(tmp.Name())
	data, _ := os.ReadFile(tmp.Name())
	wdc.files["/_meta/structure.db"] = data
	srcDB.Close()

	// Create local structure DB and engine.
	localDB, _ := meta.OpenStructureDB(":memory:")
	statsDB, _ := meta.OpenStatsDB(":memory:", "daemon-t1")
	defer localDB.Close()
	defer statsDB.Close()

	// Add location to local DB so sync can look it up.
	localDB.AddLocation(meta.Location{
		ID: "loc-synced", URL: "http://dav", Username: "u",
		PasswordEnc: "e", DisplayName: "Synced", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})

	eng := msync.NewWithClientFactory(wdc, localDB, statsDB, "daemon-t1", func(_, _, _ string) wdv.Client { return wdc })
	if err := eng.SyncFromWebDAV(context.Background(), "loc-synced"); err != nil {
		t.Fatalf("SyncFromWebDAV: %v", err)
	}

	// Local DB should now have the location.
	locs, err := localDB.ListLocations()
	if err != nil {
		t.Fatalf("ListLocations after sync: %v", err)
	}
	if len(locs) != 1 || locs[0].ID != "loc-synced" {
		t.Errorf("expected 1 synced location, got %+v", locs)
	}
}

func TestEngine_FlushStats(t *testing.T) {
	wdc := newMockWDV()
	localDB, _ := meta.OpenStructureDB(":memory:")
	statsDB, _ := meta.OpenStatsDB(":memory:", "daemon-t2")
	defer localDB.Close()
	defer statsDB.Close()

	statsDB.AddDelta("loc-1", "u-1", "bkt-1", 500, 1)

	eng := msync.New(wdc, localDB, statsDB, "daemon-t2")
	if err := eng.FlushStats(context.Background()); err != nil {
		t.Fatalf("FlushStats: %v", err)
	}

	remotePath := "/_meta/stats-daemon-t2.db"
	if _, ok := wdc.files[remotePath]; !ok {
		t.Errorf("expected stats file at %s after FlushStats", remotePath)
	}
}

func TestEngine_FlushStructure(t *testing.T) {
	wdc := newMockWDV()
	localDB, _ := meta.OpenStructureDB(":memory:")
	statsDB, _ := meta.OpenStatsDB(":memory:", "daemon-t3")
	defer localDB.Close()
	defer statsDB.Close()

	localDB.AddLocation(meta.Location{
		ID: "loc-local", URL: "http://local", Username: "u",
		PasswordEnc: "e", DisplayName: "Local", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})

	eng := msync.New(wdc, localDB, statsDB, "daemon-t3")
	if err := eng.FlushStructure(context.Background()); err != nil {
		t.Fatalf("FlushStructure: %v", err)
	}

	if _, ok := wdc.files["/_meta/structure.db"]; !ok {
		t.Errorf("expected structure.db file on WebDAV after FlushStructure")
	}
}

func TestEngine_FlushBucketDBs(t *testing.T) {
	wdc := newMockWDV()
	cacheDir := t.TempDir()
	bucketID := "bucket-local"

	localDB, _ := meta.OpenStructureDB(":memory:")
	localDB.AddLocation(meta.Location{
		ID: "loc-local", URL: "http://dav", Username: "u",
		PasswordEnc: "e", DisplayName: "Local", QuotaBytes: 1e9,
		BaseDir: "/base/", CreatedAt: time.Now(),
	})
	localDB.AddBucket(meta.Bucket{
		ID: bucketID, Name: "local-bucket", OwnerUserID: "u-local",
		WebDAVLocationID: "loc-local", CreatedAt: time.Now(),
	})
	statsDB, _ := meta.OpenStatsDB(":memory:", "daemon-bucket-flush")
	defer localDB.Close()
	defer statsDB.Close()

	localBucketDB, _ := meta.OpenBucketDB(filepath.Join(cacheDir, "bucket-"+bucketID+".db"))
	localBucketDB.PutObject(meta.Object{
		ID: "obj-local", Key: "local.txt", HashPath: "/base/_data/local",
		SizeBytes: 5, ETag: "etag", ContentType: "text/plain",
		LastModified: time.Now(), UploadComplete: true,
	})
	localBucketDB.Close()

	eng := msync.NewWithClientFactoryAndCacheDir(wdc, localDB, statsDB, "daemon-bucket-flush", cacheDir, func(_, _, _ string) wdv.Client { return wdc })
	if err := eng.FlushBucketDBs(context.Background()); err != nil {
		t.Fatalf("FlushBucketDBs: %v", err)
	}

	remoteData, ok := wdc.files["/base/_meta/"+bucketID+".db"]
	if !ok {
		t.Fatal("expected bucket db uploaded to WebDAV")
	}
	remotePath := filepath.Join(t.TempDir(), "remote-bucket.db")
	if err := os.WriteFile(remotePath, remoteData, 0600); err != nil {
		t.Fatalf("write remote bucket db: %v", err)
	}
	remoteBucketDB, err := meta.OpenBucketDB(remotePath)
	if err != nil {
		t.Fatalf("open remote bucket db: %v", err)
	}
	defer remoteBucketDB.Close()
	if _, err := remoteBucketDB.GetObject("local.txt"); err != nil {
		t.Fatalf("expected uploaded object metadata: %v", err)
	}
}

func TestEngine_SyncFromWebDAV_MergeLocalWins(t *testing.T) {
	// Build a remote structure.db with a location and a user.
	srcDB, _ := meta.OpenStructureDB(":memory:")
	srcDB.AddLocation(meta.Location{
		ID: "loc-remote", URL: "http://dav", Username: "u",
		PasswordEnc: "e", DisplayName: "Remote", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	srcDB.AddUser(meta.User{
		ID: "u-remote", AccessKey: "AKREMOTE", SecretKeyHash: "h", SecretKeyEnc: "e",
		DisplayName: "Remote User", Enabled: true, CreatedAt: time.Now(),
	})

	wdc := newMockWDV()
	tmp, _ := os.CreateTemp(t.TempDir(), "struct*.db")
	tmp.Close()
	srcDB.SaveToFile(tmp.Name())
	data, _ := os.ReadFile(tmp.Name())
	wdc.files["/_meta/structure.db"] = data
	srcDB.Close()

	// Local DB already has a user with the same ID but different data.
	localDB, _ := meta.OpenStructureDB(":memory:")
	localDB.AddLocation(meta.Location{
		ID: "loc-remote", URL: "http://dav", Username: "u",
		PasswordEnc: "e", DisplayName: "Remote", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	localDB.AddUser(meta.User{
		ID: "u-remote", AccessKey: "AKLOCAL", SecretKeyHash: "h2", SecretKeyEnc: "e2",
		DisplayName: "Local User", Enabled: true, CreatedAt: time.Now(),
	})
	statsDB, _ := meta.OpenStatsDB(":memory:", "daemon-t4")
	defer localDB.Close()
	defer statsDB.Close()

	eng := msync.NewWithClientFactory(wdc, localDB, statsDB, "daemon-t4", func(_, _, _ string) wdv.Client { return wdc })
	if err := eng.SyncFromWebDAV(context.Background(), "loc-remote"); err != nil {
		t.Fatalf("SyncFromWebDAV: %v", err)
	}

	locs, _ := localDB.ListLocations()
	if len(locs) != 1 || locs[0].ID != "loc-remote" {
		t.Errorf("expected remote location merged, got %+v", locs)
	}

	u, _ := localDB.GetUser("u-remote")
	if u.AccessKey != "AKLOCAL" {
		t.Errorf("expected local user to win on conflict, got access_key=%q", u.AccessKey)
	}
}

func TestEngine_SyncFromWebDAV_PreservesWebPasswordEnc(t *testing.T) {
	srcDB, _ := meta.OpenStructureDB(":memory:")
	srcDB.AddLocation(meta.Location{
		ID: "loc-remote", URL: "http://dav", Username: "u",
		PasswordEnc: "e", DisplayName: "Remote", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	srcDB.AddUser(meta.User{
		ID:              "u-remote",
		AccessKey:       "AKREMOTE",
		SecretKeyHash:   "h",
		SecretKeyEnc:    "e",
		WebPasswordHash: "web-hash",
		WebPasswordEnc:  "web-enc",
		DisplayName:     "Remote User",
		Enabled:         true,
		CreatedAt:       time.Now(),
	})

	wdc := newMockWDV()
	tmp, _ := os.CreateTemp(t.TempDir(), "struct*.db")
	tmp.Close()
	srcDB.SaveToFile(tmp.Name())
	data, _ := os.ReadFile(tmp.Name())
	wdc.files["/_meta/structure.db"] = data
	srcDB.Close()

	localDB, _ := meta.OpenStructureDB(":memory:")
	localDB.AddLocation(meta.Location{
		ID: "loc-remote", URL: "http://dav", Username: "u",
		PasswordEnc: "e", DisplayName: "Remote", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	statsDB, _ := meta.OpenStatsDB(":memory:", "daemon-t7")
	defer localDB.Close()
	defer statsDB.Close()

	eng := msync.NewWithClientFactory(wdc, localDB, statsDB, "daemon-t7", func(_, _, _ string) wdv.Client { return wdc })
	if err := eng.SyncFromWebDAV(context.Background(), "loc-remote"); err != nil {
		t.Fatalf("SyncFromWebDAV: %v", err)
	}

	u, err := localDB.GetUser("u-remote")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.WebPasswordEnc != "web-enc" {
		t.Fatalf("WebPasswordEnc = %q, want %q", u.WebPasswordEnc, "web-enc")
	}
}

func TestEngine_SyncFromWebDAV_RemapSameLocationAndDownloadBucketDB(t *testing.T) {
	bucketID := "bucket-old"

	srcDB, _ := meta.OpenStructureDB(":memory:")
	srcDB.AddLocation(meta.Location{
		ID: "loc-old", URL: "http://dav/root/", Username: "u",
		PasswordEnc: "old", DisplayName: "Old", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	srcDB.AddUser(meta.User{
		ID: "u-old", AccessKey: "AKOLD", SecretKeyHash: "h", SecretKeyEnc: "e",
		DisplayName: "Old User", Enabled: true, CreatedAt: time.Now(),
	})
	srcDB.AddBucket(meta.Bucket{
		ID: bucketID, Name: "restored-bucket", OwnerUserID: "u-old",
		WebDAVLocationID: "loc-old", CreatedAt: time.Now(),
	})

	wdc := newMockWDV()
	tmp, _ := os.CreateTemp(t.TempDir(), "struct*.db")
	tmp.Close()
	srcDB.SaveToFile(tmp.Name())
	data, _ := os.ReadFile(tmp.Name())
	wdc.files["/_meta/structure.db"] = data
	srcDB.Close()

	remoteBucketDB, _ := meta.OpenBucketDB(filepath.Join(t.TempDir(), "remote-bucket.db"))
	remoteBucketDB.PutObject(meta.Object{
		ID: "obj-1", Key: "hello.txt", HashPath: "/_data/hash",
		SizeBytes: 5, ETag: "etag", ContentType: "text/plain",
		LastModified: time.Now(), UploadComplete: true,
	})
	remoteBucketFile, _ := os.CreateTemp(t.TempDir(), "bucket*.db")
	remoteBucketFile.Close()
	os.Remove(remoteBucketFile.Name())
	remoteBucketDB.SaveToFile(remoteBucketFile.Name())
	remoteBucketDB.Close()
	bucketData, _ := os.ReadFile(remoteBucketFile.Name())
	wdc.files["/_meta/"+bucketID+".db"] = bucketData

	cacheDir := t.TempDir()
	localDB, _ := meta.OpenStructureDB(":memory:")
	localDB.AddLocation(meta.Location{
		ID: "loc-new", URL: "http://dav/root", Username: "u",
		PasswordEnc: "new", DisplayName: "New", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	statsDB, _ := meta.OpenStatsDB(":memory:", "daemon-t5")
	defer localDB.Close()
	defer statsDB.Close()

	eng := msync.NewWithClientFactoryAndCacheDir(wdc, localDB, statsDB, "daemon-t5", cacheDir, func(_, _, _ string) wdv.Client { return wdc })
	if err := eng.SyncFromWebDAV(context.Background(), "loc-new"); err != nil {
		t.Fatalf("SyncFromWebDAV: %v", err)
	}

	locs, _ := localDB.ListLocations()
	if len(locs) != 1 || locs[0].ID != "loc-new" {
		t.Fatalf("expected same WebDAV location to stay single, got %+v", locs)
	}

	bkt, err := localDB.GetBucket("restored-bucket")
	if err != nil {
		t.Fatalf("GetBucket restored-bucket: %v", err)
	}
	if bkt.WebDAVLocationID != "loc-new" {
		t.Fatalf("expected bucket location remapped to loc-new, got %q", bkt.WebDAVLocationID)
	}

	localBucketDB, err := meta.OpenBucketDB(filepath.Join(cacheDir, "bucket-"+bucketID+".db"))
	if err != nil {
		t.Fatalf("open synced bucket db: %v", err)
	}
	defer localBucketDB.Close()
	if _, err := localBucketDB.GetObject("hello.txt"); err != nil {
		t.Fatalf("expected synced bucket object metadata: %v", err)
	}
}

func TestEngine_SyncFromWebDAV_InvalidRemoteBucketDBRepairsFromLocal(t *testing.T) {
	bucketID := "bucket-corrupt"

	srcDB, _ := meta.OpenStructureDB(":memory:")
	srcDB.AddLocation(meta.Location{
		ID: "loc-remote", URL: "http://dav/root/", Username: "u",
		PasswordEnc: "old", DisplayName: "Old", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	srcDB.AddBucket(meta.Bucket{
		ID: bucketID, Name: "kept-bucket", OwnerUserID: "u-old",
		WebDAVLocationID: "loc-remote", CreatedAt: time.Now(),
	})

	wdc := newMockWDV()
	tmp, _ := os.CreateTemp(t.TempDir(), "struct*.db")
	tmp.Close()
	srcDB.SaveToFile(tmp.Name())
	data, _ := os.ReadFile(tmp.Name())
	wdc.files["/_meta/structure.db"] = data
	srcDB.Close()
	wdc.files["/_meta/"+bucketID+".db"] = []byte("not a sqlite database")

	cacheDir := t.TempDir()
	localBucketPath := filepath.Join(cacheDir, "bucket-"+bucketID+".db")
	localBucketDB, _ := meta.OpenBucketDB(localBucketPath)
	localBucketDB.PutObject(meta.Object{
		ID: "obj-local", Key: "local.txt", HashPath: "/_data/local",
		SizeBytes: 5, ETag: "etag", ContentType: "text/plain",
		LastModified: time.Now(), UploadComplete: true,
	})
	localBucketDB.Close()

	localDB, _ := meta.OpenStructureDB(":memory:")
	localDB.AddLocation(meta.Location{
		ID: "loc-local", URL: "http://dav/root", Username: "u",
		PasswordEnc: "new", DisplayName: "New", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	statsDB, _ := meta.OpenStatsDB(":memory:", "daemon-corrupt")
	defer localDB.Close()
	defer statsDB.Close()

	eng := msync.NewWithClientFactoryAndCacheDir(wdc, localDB, statsDB, "daemon-corrupt", cacheDir, func(_, _, _ string) wdv.Client { return wdc })
	if err := eng.SyncFromWebDAV(context.Background(), "loc-local"); err != nil {
		t.Fatalf("SyncFromWebDAV should repair invalid remote bucket db from local: %v", err)
	}

	keptDB, err := meta.OpenBucketDB(localBucketPath)
	if err != nil {
		t.Fatalf("local bucket db was corrupted: %v", err)
	}
	defer keptDB.Close()
	if _, err := keptDB.GetObject("local.txt"); err != nil {
		t.Fatalf("local object metadata was not preserved: %v", err)
	}

	remotePath := filepath.Join(t.TempDir(), "remote-repaired.db")
	if err := os.WriteFile(remotePath, wdc.files["/_meta/"+bucketID+".db"], 0600); err != nil {
		t.Fatalf("write repaired remote db: %v", err)
	}
	if err := meta.ValidateBucketDBFile(remotePath); err != nil {
		t.Fatalf("remote bucket db was not repaired: %v", err)
	}
	remoteDB, err := meta.OpenBucketDB(remotePath)
	if err != nil {
		t.Fatalf("open repaired remote bucket db: %v", err)
	}
	defer remoteDB.Close()
	if _, err := remoteDB.GetObject("local.txt"); err != nil {
		t.Fatalf("repaired remote metadata missing local object: %v", err)
	}
}

func TestEngine_SyncFromWebDAV_MergesRemoteStatsWithLocationRemap(t *testing.T) {
	srcDB, _ := meta.OpenStructureDB(":memory:")
	srcDB.AddLocation(meta.Location{
		ID: "loc-old", URL: "http://dav/root/", Username: "u",
		PasswordEnc: "old", DisplayName: "Old", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})

	wdc := newMockWDV()
	tmp, _ := os.CreateTemp(t.TempDir(), "struct*.db")
	tmp.Close()
	srcDB.SaveToFile(tmp.Name())
	data, _ := os.ReadFile(tmp.Name())
	wdc.files["/_meta/structure.db"] = data
	srcDB.Close()

	remoteStats, _ := meta.OpenStatsDB(":memory:", "daemon-old")
	remoteStats.AddDelta("loc-old", "u-old", "bucket-old", 1234, 1)
	if err := remoteStats.Flush(context.Background(), wdc, "/_meta/stats-daemon-old.db"); err != nil {
		t.Fatalf("flush remote stats: %v", err)
	}
	remoteStats.Close()

	localDB, _ := meta.OpenStructureDB(":memory:")
	localDB.AddLocation(meta.Location{
		ID: "loc-new", URL: "http://dav/root", Username: "u",
		PasswordEnc: "new", DisplayName: "New", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	statsDB, _ := meta.OpenStatsDB(":memory:", "daemon-new")
	defer localDB.Close()
	defer statsDB.Close()

	eng := msync.NewWithClientFactory(wdc, localDB, statsDB, "daemon-new", func(_, _, _ string) wdv.Client { return wdc })
	if err := eng.SyncFromWebDAV(context.Background(), "loc-new"); err != nil {
		t.Fatalf("SyncFromWebDAV: %v", err)
	}

	usage, err := statsDB.GetTotalUsage("loc-new")
	if err != nil {
		t.Fatalf("GetTotalUsage loc-new: %v", err)
	}
	if usage != 1234 {
		t.Fatalf("usage after stats sync: got %d want 1234", usage)
	}

	if err := eng.SyncFromWebDAV(context.Background(), "loc-new"); err != nil {
		t.Fatalf("second SyncFromWebDAV: %v", err)
	}
	usage, _ = statsDB.GetTotalUsage("loc-new")
	if usage != 1234 {
		t.Fatalf("usage after second stats sync: got %d want idempotent 1234", usage)
	}

	if _, ok := wdc.files[path.Join("/_meta", "stats-daemon-old.db")]; !ok {
		t.Fatal("test setup lost remote stats file")
	}
}

func TestEngine_SyncFromWebDAV_RepairsExistingBucketLocationID(t *testing.T) {
	srcDB, _ := meta.OpenStructureDB(":memory:")
	srcDB.AddLocation(meta.Location{
		ID: "loc-old", URL: "http://dav/root/", Username: "u",
		PasswordEnc: "old", DisplayName: "Old", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	srcDB.AddBucket(meta.Bucket{
		ID: "bucket-old", Name: "restored-bucket", OwnerUserID: "u-old",
		WebDAVLocationID: "loc-old", CreatedAt: time.Now(),
	})

	wdc := newMockWDV()
	tmp, _ := os.CreateTemp(t.TempDir(), "struct*.db")
	tmp.Close()
	srcDB.SaveToFile(tmp.Name())
	data, _ := os.ReadFile(tmp.Name())
	wdc.files["/_meta/structure.db"] = data
	srcDB.Close()

	localDB, _ := meta.OpenStructureDB(":memory:")
	localDB.AddLocation(meta.Location{
		ID: "loc-new", URL: "http://dav/root", Username: "u",
		PasswordEnc: "new", DisplayName: "New", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	localDB.AddLocation(meta.Location{
		ID: "loc-old", URL: "http://dav/root/", Username: "u",
		PasswordEnc: "old", DisplayName: "Old", QuotaBytes: 1e9, CreatedAt: time.Now(),
	})
	localDB.AddBucket(meta.Bucket{
		ID: "bucket-old", Name: "restored-bucket", OwnerUserID: "u-old",
		WebDAVLocationID: "loc-old", CreatedAt: time.Now(),
	})
	statsDB, _ := meta.OpenStatsDB(":memory:", "daemon-t6")
	defer localDB.Close()
	defer statsDB.Close()

	eng := msync.NewWithClientFactory(wdc, localDB, statsDB, "daemon-t6", func(_, _, _ string) wdv.Client { return wdc })
	if err := eng.SyncFromWebDAV(context.Background(), "loc-new"); err != nil {
		t.Fatalf("SyncFromWebDAV: %v", err)
	}

	bkt, err := localDB.GetBucket("restored-bucket")
	if err != nil {
		t.Fatalf("GetBucket restored-bucket: %v", err)
	}
	if bkt.WebDAVLocationID != "loc-new" {
		t.Fatalf("expected existing bucket location repaired to loc-new, got %q", bkt.WebDAVLocationID)
	}
}
