package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hpolthof/webdavs3/internal/adminui"
	"github.com/hpolthof/webdavs3/internal/auth"
	"github.com/hpolthof/webdavs3/internal/bucket"
	"github.com/hpolthof/webdavs3/internal/meta"
	"github.com/hpolthof/webdavs3/internal/object"
	"github.com/hpolthof/webdavs3/internal/provisioning"
	"github.com/hpolthof/webdavs3/internal/quota"
	"github.com/hpolthof/webdavs3/internal/s3api"
	msync "github.com/hpolthof/webdavs3/internal/sync"
	"github.com/hpolthof/webdavs3/internal/webdav"
	"github.com/hpolthof/webdavs3/internal/webui"
)

func main() {
	cfgPath := "config.yaml"

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			if len(os.Args) > 2 {
				cfgPath = os.Args[2]
			}
			if err := runSetup(cfgPath); err != nil {
				fmt.Fprintf(os.Stderr, "setup: %v\n", err)
				os.Exit(1)
			}
			return
		case "gc":
			force := false
			var remaining []string
			for _, a := range os.Args[2:] {
				if a == "--force" {
					force = true
				} else {
					remaining = append(remaining, a)
				}
			}
			if len(remaining) > 0 {
				cfgPath = remaining[0]
			}
			if err := runGC(cfgPath, force); err != nil {
				fmt.Fprintf(os.Stderr, "gc: %v\n", err)
				os.Exit(1)
			}
			return
		case "provision":
			if len(os.Args) > 2 && os.Args[2] == "dump" {
				if len(os.Args) > 3 {
					cfgPath = os.Args[3]
				}
				if err := runProvisionDump(cfgPath); err != nil {
					fmt.Fprintf(os.Stderr, "provision dump: %v\n", err)
					os.Exit(1)
				}
				return
			}
		case "-h", "--help", "help":
			fmt.Println("Usage:")
			fmt.Println("  webdavs3 [config.yaml]          run the daemon")
			fmt.Println("  webdavs3 setup [config.yaml]    interactive first-run setup")
			fmt.Println("  webdavs3 gc [--force] [config.yaml]   remove orphaned files from all WebDAV locations")
			fmt.Println("  webdavs3 provision dump [config.yaml] export current provisioning state as YAML")
			return
		default:
			cfgPath = os.Args[1]
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := runDaemon(ctx, cfgPath); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		slog.Error("daemon error", "err", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}

// runDaemon starts all services and blocks until ctx is cancelled.
func runDaemon(ctx context.Context, cfgPath string) error {
	// 1. Load config.
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// 2. Init logger.
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(handler))

	// 2b. Validate required config.
	if cfg.EncryptionKey == "" {
		return fmt.Errorf("encryption_key is required in config (must be a base64-encoded 32-byte AES-256 key)")
	}

	// 3. Ensure local cache dir exists.
	if cfg.LocalCacheDir != "" {
		if err := os.MkdirAll(cfg.LocalCacheDir, 0750); err != nil {
			return fmt.Errorf("create cache dir: %w", err)
		}
	}

	// 4. Load or generate daemon ID.
	daemonID, err := loadOrCreateDaemonID(cfg.LocalCacheDir)
	if err != nil {
		return fmt.Errorf("daemon id: %w", err)
	}
	slog.Info("daemon started", "id", daemonID)

	// 5. Open local stats.db.
	statsPath := ":memory:"
	if cfg.LocalCacheDir != "" {
		statsPath = filepath.Join(cfg.LocalCacheDir, "stats.db")
	}
	statsDB, err := meta.OpenStatsDB(statsPath, daemonID)
	if err != nil {
		return fmt.Errorf("open stats.db: %w", err)
	}
	defer statsDB.Close()

	// 6. Open local structure.db.
	structPath := ":memory:"
	if cfg.LocalCacheDir != "" {
		structPath = filepath.Join(cfg.LocalCacheDir, "structure.db")
	}
	structDB, err := meta.OpenStructureDB(structPath)
	if err != nil {
		return fmt.Errorf("open structure.db: %w", err)
	}
	defer structDB.Close()

	// 7. Init LRU cache for bucket DBs.
	cacheSize := cfg.BucketCacheSize
	if cacheSize <= 0 {
		cacheSize = 50
	}
	bucketCache := meta.NewLRUCache(cacheSize, func(id string) (meta.BucketDB, error) {
		dbPath := ":memory:"
		if cfg.LocalCacheDir != "" {
			dbPath = filepath.Join(cfg.LocalCacheDir, "bucket-"+id+".db")
		}
		return meta.OpenBucketDB(dbPath)
	})
	defer bucketCache.CloseAll()

	// 8. Resolve a WebDAV client (use noop when no locations are configured yet).
	// Wrap it in RefreshableClient so the admin UI can swap it when locations
	// are added or edited without restarting the daemon.
	wdc := webdav.NewRefreshable(&noopWebDAV{})
	refreshWebDAV := func() {
		locs, _ := structDB.ListLocations()
		if len(locs) == 0 {
			wdc.SetClient(&noopWebDAV{})
			return
		}
		loc := locs[0]
		var password string
		if cfg.EncryptionKey != "" {
			password, _ = adminui.DecryptPassword(loc.PasswordEnc, cfg.EncryptionKey)
		}
		wdc.SetClient(webdav.NewRetryClient(webdav.New(loc.URL, loc.Username, password), 3, time.Second))
	}
	refreshWebDAV()

	// 9. Init services.
	syncEngine := msync.NewWithEncryptionAndCacheDir(wdc, structDB, statsDB, daemonID, cfg.LocalCacheDir, cfg.EncryptionKey, adminui.DecryptPassword)
	verifier := auth.NewVerifier(cfg.Region, "s3")
	quotaSvc := quota.New(structDB, statsDB)

	flushStructure := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := syncEngine.FlushStructure(ctx); err != nil {
			slog.Warn("flush structure.db failed", "err", err)
		}
	}

	bucketSvc := bucket.NewWithFlush(structDB, statsDB, wdc, flushStructure)
	multipartSvc := object.NewMultipartService(wdc, bucketCache, statsDB, structDB, cfg.LocalCacheDir)
	objectSvc := object.New(
		wdc, bucketCache, statsDB, structDB, cfg.LocalCacheDir,
		multipartSvc,
		int64(cfg.ChunkThresholdMB)*1024*1024,
		int64(cfg.ChunkSizeMB)*1024*1024,
	)

	if err := provisioning.ApplyStartupProvisioning(ctx, provisioning.ApplyConfig{
		FilePath:      cfg.ProvisionFile,
		LocalCacheDir: cfg.LocalCacheDir,
		EncryptionKey: cfg.EncryptionKey,
		Structure:     structDB,
		Buckets:       bucketSvc,
		RefreshWebDAV: refreshWebDAV,
		SyncEngine:    syncEngine,
	}); err != nil {
		return fmt.Errorf("startup provisioning: %w", err)
	}

	// 9b. Ensure required WebDAV directories exist.
	go func() {
		initCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		baseDir := "/"
		if locs, err := structDB.ListLocations(); err == nil && len(locs) > 0 {
			baseDir = locs[0].BaseDir
		}
		for _, dir := range []string{baseDir + "_data/.tmp", baseDir + "_parts", baseDir + "_meta"} {
			if err := wdc.MkdirAll(initCtx, dir); err != nil {
				slog.Warn("could not init webdav dir", "dir", dir, "err", err)
			}
		}
		slog.Debug("webdav dirs initialized")
	}()

	// 10. Build S3 HTTP server.
	encKey := cfg.EncryptionKey
	s3Handler := s3api.NewRouter(s3api.S3Deps{
		Auth:      verifier,
		Structure: structDB,
		Buckets:   bucketSvc,
		Objects:   objectSvc,
		Multipart: multipartSvc,
		Quota:     quotaSvc,
		Region:    cfg.Region,
		DecryptFn: func(ct string) (string, error) {
			return adminui.DecryptPassword(ct, encKey)
		},
	})
	s3Server := &http.Server{
		Addr:         cfg.S3Listen,
		Handler:      s3Handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 11. Build Admin + User Web UI HTTP server.
	adminSrv := adminui.NewAdminServer(adminui.AdminDeps{
		AdminPasswordHash: cfg.AdminPasswordHash,
		AdminUsername:     cfg.AdminUsername,
		EncryptionKey:     cfg.EncryptionKey,
		Structure:         structDB,
		Stats:             statsDB,
		SyncEngine:        syncEngine,
		BucketService:     bucketSvc,
		RefreshWebDAV:     refreshWebDAV,
		FlushStructure:    flushStructure,
	})
	webuiSrv := webui.NewServer(webui.Deps{
		Structure:     structDB,
		BucketService: bucketSvc,
		ObjectService: objectSvc,
	})
	combinedMux := http.NewServeMux()
	combinedMux.Handle("/admin/", adminSrv)
	combinedMux.Handle("/", webuiSrv)
	adminServer := &http.Server{
		Addr:         cfg.AdminListen,
		Handler:      combinedMux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 12. Parse background-worker intervals.
	syncInterval, err := time.ParseDuration(cfg.SyncInterval)
	if err != nil {
		syncInterval = 5 * time.Minute
	}
	statsFlushInterval, err := time.ParseDuration(cfg.StatsFlusInterval)
	if err != nil {
		statsFlushInterval = time.Minute
	}

	// 13. Start HTTP servers in goroutines.
	go func() {
		slog.Info("S3 API listening", "addr", cfg.S3Listen)
		if err := s3Server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("S3 server error", "err", err)
		}
	}()
	go func() {
		slog.Info("Admin + Web UI listening", "addr", cfg.AdminListen)
		if err := adminServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Admin server error", "err", err)
		}
	}()

	// 14. Background sync ticker.
	go func() {
		t := time.NewTicker(syncInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				locations, err := structDB.ListLocations()
				if err != nil || len(locations) == 0 {
					continue
				}
				for _, loc := range locations {
					if err := syncEngine.SyncFromWebDAV(ctx, loc.ID); err != nil {
						slog.Warn("sync failed", "location", loc.ID, "err", err)
					}
				}
			}
		}
	}()

	// 15. Background stats flush ticker.
	go func() {
		t := time.NewTicker(statsFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := syncEngine.FlushStats(ctx); err != nil {
					slog.Warn("stats flush failed", "err", err)
				}
			}
		}
	}()

	// 16. Block until shutdown signal.
	<-ctx.Done()
	slog.Info("shutting down")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	_ = s3Server.Shutdown(shutCtx)
	_ = adminServer.Shutdown(shutCtx)

	// Final stats flush on shutdown.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer flushCancel()
	if err := syncEngine.FlushStats(flushCtx); err != nil {
		slog.Warn("final stats flush failed", "err", err)
	}

	return ctx.Err()
}

// loadOrCreateDaemonID reads the daemon.id file from dir or creates a new UUID v4.
func loadOrCreateDaemonID(dir string) (string, error) {
	if dir == "" {
		return generateUUID(), nil
	}
	path := filepath.Join(dir, "daemon.id")
	data, err := os.ReadFile(path)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	id := generateUUID()
	return id, os.WriteFile(path, []byte(id), 0600)
}

// generateUUID creates a random UUID version 4.
func generateUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("generateUUID: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// runGC removes orphaned /_data/ files from every configured WebDAV location.
// Intended to clean up data left behind before the delete-cleanup fix was deployed.
// Stop the daemon before running, or accept the small race window during active uploads.
func runGC(cfgPath string, force bool) error {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	structPath := ":memory:"
	if cfg.LocalCacheDir != "" {
		structPath = filepath.Join(cfg.LocalCacheDir, "structure.db")
	}
	structDB, err := meta.OpenStructureDB(structPath)
	if err != nil {
		return fmt.Errorf("open structure.db: %w", err)
	}
	defer structDB.Close()

	locs, err := structDB.ListLocations()
	if err != nil {
		return fmt.Errorf("list locations: %w", err)
	}
	if len(locs) == 0 {
		return fmt.Errorf("no locations found in structure.db — run the daemon first to populate it")
	}

	statsDB, err := meta.OpenStatsDB(":memory:", "gc")
	if err != nil {
		return fmt.Errorf("open stats: %w", err)
	}
	defer statsDB.Close()

	bucketCache := meta.NewLRUCache(50, func(id string) (meta.BucketDB, error) {
		dbPath := ":memory:"
		if cfg.LocalCacheDir != "" {
			dbPath = filepath.Join(cfg.LocalCacheDir, "bucket-"+id+".db")
		}
		return meta.OpenBucketDB(dbPath)
	})
	defer bucketCache.CloseAll()

	if force {
		fmt.Printf("Running GC without confirmation (--force). Deleting unreferenced files from %d location(s).\n", len(locs))
	} else {
		fmt.Printf("This will permanently delete unreferenced files from %d location(s).\n", len(locs))
		fmt.Print("Proceed? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	ctx := context.Background()
	totalDeleted := 0

	for _, loc := range locs {
		fmt.Printf("\nLocation: %s (%s)\n", loc.DisplayName, loc.ID)

		var password string
		if cfg.EncryptionKey != "" {
			password, _ = adminui.DecryptPassword(loc.PasswordEnc, cfg.EncryptionKey)
		}
		wdc := webdav.New(loc.URL, loc.Username, password)

		if err := wdc.Ping(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "  unreachable: %v — skipping\n", err)
			continue
		}

		svc := object.New(wdc, bucketCache, statsDB, structDB, cfg.LocalCacheDir, nil, 0, 0)
		gc, _ := svc.(object.GarbageCollector)
		deleted, err := gc.GarbageCollect(ctx, loc.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  gc error: %v\n", err)
			continue
		}
		fmt.Printf("  deleted %d orphaned file(s)\n", deleted)
		totalDeleted += deleted
	}

	fmt.Printf("\nDone. Total deleted: %d\n", totalDeleted)
	return nil
}

func runProvisionDump(cfgPath string) error {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.EncryptionKey == "" {
		return fmt.Errorf("encryption_key is required for provision dump")
	}

	structDB, err := provisioning.OpenDumpStructure(cfg.LocalCacheDir)
	if err != nil {
		return err
	}
	defer structDB.Close()

	if err := provisioning.Dump(context.Background(), provisioning.DumpConfig{
		EncryptionKey: cfg.EncryptionKey,
		Structure:     structDB,
	}, os.Stdout); err != nil {
		return fmt.Errorf("dump provisioning: %w", err)
	}
	return nil
}

// noopWebDAV is a no-op WebDAV client used when no locations are configured yet.
type noopWebDAV struct{}

func (n *noopWebDAV) Upload(_ context.Context, _ string, _ io.Reader, _ int64) error {
	return nil
}
func (n *noopWebDAV) Download(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("no webdav location configured")
}
func (n *noopWebDAV) Delete(_ context.Context, _ string) error   { return nil }
func (n *noopWebDAV) MkdirAll(_ context.Context, _ string) error { return nil }
func (n *noopWebDAV) Exists(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (n *noopWebDAV) Rename(_ context.Context, _, _ string, _ bool) error { return nil }
func (n *noopWebDAV) DownloadToFile(_ context.Context, _, _ string) error {
	return fmt.Errorf("no webdav location configured")
}
func (n *noopWebDAV) UploadFromFile(_ context.Context, _, _ string) error { return nil }
func (n *noopWebDAV) ReadDir(_ context.Context, _ string) ([]string, error) {
	return nil, fmt.Errorf("no webdav location configured")
}
func (n *noopWebDAV) Ping(_ context.Context) error {
	return fmt.Errorf("no webdav location configured")
}
func (n *noopWebDAV) Stat(_ context.Context, _ string) (os.FileInfo, error) {
	return nil, fmt.Errorf("no webdav location configured")
}
