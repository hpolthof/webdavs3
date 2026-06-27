package meta_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/hpolthof/webdavs3/internal/meta"
	wdv "github.com/hpolthof/webdavs3/internal/webdav"
)

// mockWebDAV implements wdv.Client in-memory for testing Flush.
type mockWebDAV struct {
	files map[string][]byte
}

func newMockWebDAV() *mockWebDAV {
	return &mockWebDAV{files: map[string][]byte{}}
}

func (m *mockWebDAV) Upload(_ context.Context, path string, r io.Reader, _ int64) error {
	data, _ := io.ReadAll(r)
	m.files[path] = data
	return nil
}
func (m *mockWebDAV) Download(_ context.Context, path string) (io.ReadCloser, error) {
	data, ok := m.files[path]
	if !ok {
		return nil, fmt.Errorf("not found: %s", path)
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}
func (m *mockWebDAV) Delete(_ context.Context, path string) error {
	delete(m.files, path)
	return nil
}
func (m *mockWebDAV) MkdirAll(_ context.Context, path string) error { return nil }
func (m *mockWebDAV) Exists(_ context.Context, path string) (bool, error) {
	_, ok := m.files[path]
	return ok, nil
}
func (m *mockWebDAV) Rename(_ context.Context, oldpath, newpath string, _ bool) error {
	data, ok := m.files[oldpath]
	if !ok {
		return fmt.Errorf("not found: %s", oldpath)
	}
	delete(m.files, oldpath)
	m.files[newpath] = data
	return nil
}
func (m *mockWebDAV) DownloadToFile(_ context.Context, path, dest string) error {
	data, ok := m.files[path]
	if !ok {
		return fmt.Errorf("not found: %s", path)
	}
	return os.WriteFile(dest, data, 0600)
}
func (m *mockWebDAV) UploadFromFile(_ context.Context, path, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	m.files[path] = data
	return nil
}
func (m *mockWebDAV) ReadDir(_ context.Context, _ string) ([]string, error) { return nil, nil }
func (m *mockWebDAV) Ping(_ context.Context) error                          { return nil }
func (m *mockWebDAV) Stat(_ context.Context, _ string) (os.FileInfo, error) { return nil, nil }

var _ wdv.Client = (*mockWebDAV)(nil)

func TestStatsDB_AddDeltaAndUsage(t *testing.T) {
	db, err := meta.OpenStatsDB(":memory:", "daemon-1")
	if err != nil {
		t.Fatalf("OpenStatsDB: %v", err)
	}
	defer db.Close()

	if err := db.AddDelta("loc-1", "u-1", "bkt-1", 1000, 1); err != nil {
		t.Fatalf("AddDelta: %v", err)
	}
	if err := db.AddDelta("loc-1", "u-1", "bkt-1", 500, 1); err != nil {
		t.Fatalf("AddDelta 2: %v", err)
	}
	if err := db.AddDelta("loc-1", "u-2", "bkt-2", 200, 1); err != nil {
		t.Fatalf("AddDelta loc2: %v", err)
	}

	usage, err := db.GetTotalUsage("loc-1")
	if err != nil {
		t.Fatalf("GetTotalUsage: %v", err)
	}
	// 1000 + 500 + 200 = 1700
	if usage != 1700 {
		t.Errorf("GetTotalUsage: got %d want 1700", usage)
	}
}

func TestStatsDB_Flush(t *testing.T) {
	db, err := meta.OpenStatsDB(":memory:", "daemon-1")
	if err != nil {
		t.Fatalf("OpenStatsDB: %v", err)
	}
	defer db.Close()

	db.AddDelta("loc-1", "u-1", "bkt-1", 999, 2)

	wdClient := newMockWebDAV()
	ctx := context.Background()
	remotePath := "/_meta/stats-daemon-1.db"
	if err := db.Flush(ctx, wdClient, remotePath); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// File should have been uploaded
	if _, ok := wdClient.files[remotePath]; !ok {
		t.Error("expected stats file to be uploaded to mock WebDAV")
	}
}
