package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hpolthof/webdavs3/internal/meta"
)

func TestMain_StartupShutdown(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	// Note: bcrypt hash contains $ signs — use single-quoted YAML scalars to avoid interpretation.
	cfgYAML := "admin_username: admin\n" +
		"admin_password_hash: '$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy'\n" +
		"encryption_key: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='\n" +
		"local_cache_dir: '" + dir + "'\n" +
		"s3_listen: '127.0.0.1:19000'\n" +
		"admin_listen: '127.0.0.1:19001'\n" +
		"sync_interval: '1h'\n" +
		"stats_flush_interval: '1h'\n"
	os.WriteFile(cfgPath, []byte(cfgYAML), 0600)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runDaemon(ctx, cfgPath)
	}()

	// Give the servers a moment to start. Also detect early crash.
	select {
	case err := <-errCh:
		t.Fatalf("runDaemon returned early with error: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	// S3 listener should respond (401 expected without auth).
	resp, err := http.Get("http://127.0.0.1:19000/")
	if err != nil {
		t.Fatalf("S3 server not reachable: %v", err)
	}
	resp.Body.Close()

	// Admin listener should respond.
	resp2, err := http.Get("http://127.0.0.1:19001/admin/login")
	if err != nil {
		t.Fatalf("Admin server not reachable: %v", err)
	}
	resp2.Body.Close()

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
			t.Errorf("runDaemon returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("runDaemon did not shut down within 3s")
	}
}

func TestDaemonID_LoadOrCreate(t *testing.T) {
	dir := t.TempDir()

	// First call: no file exists → generates a new ID.
	id1, err := loadOrCreateDaemonID(dir)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if id1 == "" {
		t.Fatal("expected non-empty daemon ID")
	}

	// Second call: file exists → returns the same ID.
	id2, err := loadOrCreateDaemonID(dir)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if id1 != id2 {
		t.Errorf("expected same ID on second call, got %q vs %q", id1, id2)
	}

	// Verify UUID format (8-4-4-4-12).
	if len(id1) != 36 {
		t.Errorf("expected UUID length 36, got %d: %q", len(id1), id1)
	}
}

func TestDaemonID_EmptyDir(t *testing.T) {
	// Empty dir → ephemeral ID, no error.
	id, err := loadOrCreateDaemonID("")
	if err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}
}

func TestRunHealthcheck_AdminLoginOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/login" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfgPath := writeHealthcheckConfig(t, strings.TrimPrefix(srv.URL, "http://"))

	if err := runHealthcheck(cfgPath); err != nil {
		t.Fatalf("runHealthcheck: %v", err)
	}
}

func TestRunHealthcheck_AdminLoginNonOKFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfgPath := writeHealthcheckConfig(t, strings.TrimPrefix(srv.URL, "http://"))

	err := runHealthcheck(cfgPath)
	if err == nil {
		t.Fatal("expected runHealthcheck to fail")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected status in error, got: %v", err)
	}
}

func writeHealthcheckConfig(t *testing.T, adminListen string) string {
	t.Helper()
	t.Setenv("WEBDAV3S_ADMIN_LISTEN", "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgYAML := "admin_listen: '" + adminListen + "'\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

func setupGCTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	structDB, err := meta.OpenStructureDB(filepath.Join(dir, "structure.db"))
	if err != nil {
		t.Fatalf("open structuredb: %v", err)
	}
	structDB.AddLocation(meta.Location{
		ID:          "loc-1",
		URL:         "http://127.0.0.1:19999", // nothing listening here
		Username:    "u",
		PasswordEnc: "",
		DisplayName: "Test",
		QuotaBytes:  1 << 30,
		CreatedAt:   time.Now(),
	})
	structDB.Close()

	cfgYAML := "encryption_key: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='\n" +
		"local_cache_dir: '" + dir + "'\n"
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

func TestRunGC_ForceSkipsPrompt(t *testing.T) {
	cfgPath := setupGCTestConfig(t)

	done := make(chan error, 1)
	go func() { done <- runGC(cfgPath, true) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runGC(force=true) returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runGC(force=true) timed out — likely blocked on prompt")
	}
}

func TestRunGC_NoForceAbortsOnEmptyInput(t *testing.T) {
	cfgPath := setupGCTestConfig(t)

	// force=false, stdin is os.Stdin (EOF in test env) → answer="" → Aborted, nil return.
	done := make(chan error, 1)
	go func() { done <- runGC(cfgPath, false) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runGC(force=false) returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runGC(force=false) timed out")
	}
}
