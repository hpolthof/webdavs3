package webdav_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/net/webdav"
	ourwebdav "github.com/hpolthof/webdav3s/internal/webdav"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	h := &webdav.Handler{
		FileSystem: webdav.Dir(dir),
		LockSystem: webdav.NewMemLS(),
	}
	return httptest.NewServer(h)
}

func TestClient_UploadDownload(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := ourwebdav.New(srv.URL, "", "")
	ctx := context.Background()

	content := "hello webdav"
	err := c.Upload(ctx, "/test.txt", strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	rc, err := c.Download(ctx, "/test.txt")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != content {
		t.Errorf("Download content: got %q want %q", got, content)
	}
}

func TestClient_Delete(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := ourwebdav.New(srv.URL, "", "")
	ctx := context.Background()

	content := "delete me"
	c.Upload(ctx, "/del.txt", strings.NewReader(content), int64(len(content)))

	exists, err := c.Exists(ctx, "/del.txt")
	if err != nil {
		t.Fatalf("Exists before delete: %v", err)
	}
	if !exists {
		t.Fatal("expected file to exist before delete")
	}

	if err := c.Delete(ctx, "/del.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	exists, err = c.Exists(ctx, "/del.txt")
	if err != nil {
		t.Fatalf("Exists after delete: %v", err)
	}
	if exists {
		t.Fatal("expected file to not exist after delete")
	}
}

func TestClient_MkdirAll(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := ourwebdav.New(srv.URL, "", "")
	ctx := context.Background()

	if err := c.MkdirAll(ctx, "/_data/ab"); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	content := "nested"
	err := c.Upload(ctx, "/_data/ab/file.bin", strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("Upload after MkdirAll: %v", err)
	}
}

func TestClient_DownloadToFile(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := ourwebdav.New(srv.URL, "", "")
	ctx := context.Background()

	content := "file content"
	c.Upload(ctx, "/src.txt", strings.NewReader(content), int64(len(content)))

	dest := t.TempDir() + "/dst.txt"
	if err := c.DownloadToFile(ctx, "/src.txt", dest); err != nil {
		t.Fatalf("DownloadToFile: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != content {
		t.Errorf("DownloadToFile: got %q want %q", got, content)
	}
}

func TestClient_UploadFromFile(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := ourwebdav.New(srv.URL, "", "")
	ctx := context.Background()

	src := t.TempDir() + "/src.txt"
	os.WriteFile(src, []byte("from file"), 0600)

	if err := c.UploadFromFile(ctx, "/uploaded.txt", src); err != nil {
		t.Fatalf("UploadFromFile: %v", err)
	}

	rc, err := c.Download(ctx, "/uploaded.txt")
	if err != nil {
		t.Fatalf("Download after UploadFromFile: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "from file" {
		t.Errorf("UploadFromFile: got %q", got)
	}
}

func TestClient_Exists_Missing(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	c := ourwebdav.New(srv.URL, "", "")
	ctx := context.Background()

	exists, err := c.Exists(ctx, "/nonexistent.txt")
	if err != nil {
		t.Fatalf("Exists on missing: %v", err)
	}
	if exists {
		t.Fatal("expected false for missing file")
	}
}

// Ensure *ClientImpl satisfies the interface at compile time.
var _ ourwebdav.Client = (*ourwebdav.ClientImpl)(nil)

func init() {
	// suppress unused import for http
	_ = http.StatusOK
}
