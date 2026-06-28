package bucket_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hpolthof/webdavs3/internal/bucket"
	"github.com/hpolthof/webdavs3/internal/meta"
	wdv "github.com/hpolthof/webdavs3/internal/webdav"
)

// minimal in-memory WebDAV mock (same pattern as sync tests)
type mockWDV struct{ files map[string][]byte }

func newMock() *mockWDV { return &mockWDV{files: map[string][]byte{}} }
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
func (m *mockWDV) Delete(_ context.Context, p string) error   { delete(m.files, p); return nil }
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
func (m *mockWDV) ReadDir(_ context.Context, _ string) ([]string, error)          { return nil, nil }
func (m *mockWDV) ReadDirInfo(_ context.Context, _ string) ([]os.FileInfo, error) { return nil, nil }
func (m *mockWDV) Ping(_ context.Context) error                                   { return nil }
func (m *mockWDV) Stat(_ context.Context, _ string) (os.FileInfo, error)          { return nil, nil }

var _ wdv.Client = (*mockWDV)(nil)

func setupService(t *testing.T) (bucket.Service, meta.StructureDB, *mockWDV) {
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
	wdc := newMock()
	t.Cleanup(func() { structDB.Close(); statsDB.Close() })
	svc := bucket.New(structDB, statsDB, wdc)
	return svc, structDB, wdc
}

func TestBucketService_CreateAndList(t *testing.T) {
	svc, structDB, wdc := setupService(t)
	ctx := context.Background()

	if err := svc.CreateBucket(ctx, "my-bucket", "u-1", "loc-1"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// Should have created /_meta/ dir and uploaded an empty bucket db
	found := false
	for k := range wdc.files {
		if len(k) > 7 && k[:7] == "/_meta/" {
			found = true
		}
	}
	if !found {
		t.Error("expected bucket db file in /_meta/ on mock WebDAV")
	}

	// Bucket should exist in StructureDB
	b, err := structDB.GetBucket("my-bucket")
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	if b.OwnerUserID != "u-1" {
		t.Errorf("owner: got %q want u-1", b.OwnerUserID)
	}

	buckets, err := svc.ListBuckets(ctx, "u-1")
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Name != "my-bucket" {
		t.Errorf("ListBuckets: got %+v", buckets)
	}
}

func TestBucketService_CreateInvalidName(t *testing.T) {
	svc, _, _ := setupService(t)
	ctx := context.Background()

	cases := []string{"AB", "my_bucket", "my..bucket", "-bucket", "bucket-", "a"}
	for _, name := range cases {
		if err := svc.CreateBucket(ctx, name, "u-1", "loc-1"); err == nil {
			t.Errorf("expected error for invalid bucket name %q, got nil", name)
		}
	}
}

func TestBucketService_CreateRequiresLocation(t *testing.T) {
	svc, structDB, wdc := setupService(t)
	ctx := context.Background()

	err := svc.CreateBucket(ctx, "no-location", "u-1", "")
	if err == nil {
		t.Fatal("expected error creating bucket without location, got nil")
	}
	if _, getErr := structDB.GetBucket("no-location"); getErr == nil {
		t.Fatal("bucket was recorded despite missing location")
	}
	if len(wdc.files) != 0 {
		t.Fatalf("bucket db uploaded despite missing location: %v", wdc.files)
	}
}

func TestBucketService_Delete(t *testing.T) {
	svc, structDB, _ := setupService(t)
	ctx := context.Background()

	svc.CreateBucket(ctx, "to-delete", "u-1", "loc-1")
	if err := svc.DeleteBucket(ctx, "to-delete"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	_, err := structDB.GetBucket("to-delete")
	if err == nil {
		t.Fatal("expected error after DeleteBucket, got nil")
	}
}

func TestBucketService_DuplicateName(t *testing.T) {
	svc, _, _ := setupService(t)
	ctx := context.Background()

	svc.CreateBucket(ctx, "unique", "u-1", "loc-1")
	if err := svc.CreateBucket(ctx, "unique", "u-1", "loc-1"); err == nil {
		t.Fatal("expected error creating duplicate bucket, got nil")
	}
}

func TestBucketService_DebugLogsLifecycleActions(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	svc, _, _ := setupService(t)
	ctx := context.Background()

	if err := svc.CreateBucket(ctx, "log-bucket", "u-1", "loc-1"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if _, err := svc.ListBuckets(ctx, "u-1"); err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if err := svc.DeleteBucket(ctx, "log-bucket"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}

	got := logs.String()
	for _, want := range []string{
		`msg="bucket create started"`,
		`msg="bucket create completed"`,
		`msg="bucket list completed"`,
		`msg="bucket delete started"`,
		`msg="bucket delete completed"`,
		`bucket=log-bucket`,
		`bucket_id=`,
		`owner_user_id=u-1`,
		`location=loc-1`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("debug logs missing %q\nlogs:\n%s", want, got)
		}
	}
}
