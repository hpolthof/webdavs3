package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hpolthof/webdavs3/internal/adminui"
	"github.com/hpolthof/webdavs3/internal/meta"
	"github.com/hpolthof/webdavs3/internal/webdav"
	_ "modernc.org/sqlite"
)

type inspectBucketResult struct {
	Name             string `json:"name"`
	ID               string `json:"id"`
	LocalMetaStatus  string `json:"local_meta_status"`
	RemoteMetaStatus string `json:"remote_meta_status"`
	ReferencedFiles  int    `json:"referenced_files"`
	ReferencedBytes  int64  `json:"referenced_bytes"`
	RemoteDataFiles  int    `json:"remote_data_files"`
	RemoteDataBytes  int64  `json:"remote_data_bytes"`
	OrphanFiles      int    `json:"orphan_files"`
	OrphanBytes      int64  `json:"orphan_bytes"`
	MissingFiles     int    `json:"missing_files"`
}

type inspectLocationResult struct {
	DisplayName          string                `json:"display_name"`
	ID                   string                `json:"id"`
	LocalStructureStatus string                `json:"local_structure_status,omitempty"`
	LocalStatsStatus     string                `json:"local_stats_status,omitempty"`
	RemoteMetaFiles      int                   `json:"remote_meta_files,omitempty"`
	RemoteMetaBytes      int64                 `json:"remote_meta_bytes,omitempty"`
	Error                string                `json:"error,omitempty"`
	Buckets              []inspectBucketResult `json:"buckets,omitempty"`
}

type inspectResult struct {
	Locations []inspectLocationResult `json:"locations"`
}

// remoteDataMap maps remote data/parts file paths to their sizes.
type remoteDataMap map[string]int64

type bucketReference struct {
	path string
	size int64
}

func runInspect(cfgPath string, jsonOutput bool, filterBucket string) error {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if cfg.LocalCacheDir == "" {
		return fmt.Errorf("inspect requires local_cache_dir")
	}
	structPath := filepath.Join(cfg.LocalCacheDir, "structure.db")
	locs, allBuckets, err := loadInspectStructure(structPath)
	if err != nil {
		return fmt.Errorf("read structure.db: %w", err)
	}
	if len(locs) == 0 {
		return fmt.Errorf("no locations in structure.db - run daemon first to populate it")
	}
	localStructureStatus := localSQLiteStatus(structPath)
	localStatsStatus := localSQLiteStatus(filepath.Join(cfg.LocalCacheDir, "stats.db"))

	ctx := context.Background()
	result := inspectResult{}

	for _, loc := range locs {
		locResult := inspectLocationResult{
			DisplayName:          loc.DisplayName,
			ID:                   loc.ID,
			LocalStructureStatus: localStructureStatus,
			LocalStatsStatus:     localStatsStatus,
		}

		var password string
		if cfg.EncryptionKey != "" {
			password, _ = adminui.DecryptPassword(loc.PasswordEnc, cfg.EncryptionKey)
		}
		wdc := webdav.New(loc.URL, loc.Username, password)

		if err := wdc.Ping(ctx); err != nil {
			locResult.Error = fmt.Sprintf("unreachable: %v", err)
			result.Locations = append(result.Locations, locResult)
			if !jsonOutput {
				fmt.Printf("Location: %s (%s)\n  error: %v\n\n", loc.DisplayName, loc.ID, err)
			}
			continue
		}

		// Collect all remote data files for this location once.
		remoteData := collectRemoteDataFiles(ctx, wdc, loc.BaseDir)
		remoteMeta := collectRemoteMetaFiles(ctx, wdc, loc.BaseDir)
		locResult.RemoteMetaFiles = len(remoteMeta)
		for _, size := range remoteMeta {
			locResult.RemoteMetaBytes += size
		}

		// Collect all referenced paths from every bucket on this location.
		globalReferenced := make(map[string]struct{})

		for _, bkt := range allBuckets {
			if bkt.WebDAVLocationID != loc.ID {
				continue
			}
			if filterBucket != "" && bkt.Name != filterBucket {
				continue
			}

			br, referenced := inspectBucket(ctx, cfg, wdc, bkt, loc, remoteData)
			for p := range referenced {
				globalReferenced[p] = struct{}{}
			}

			// Compute orphans at location level (after all buckets processed).
			// Store temporarily; we'll fill orphan counts at the end.
			locResult.Buckets = append(locResult.Buckets, br)
		}

		// Compute location-wide orphans and distribute to per-bucket display.
		// Since orphans are location-wide, we annotate the first bucket or
		// produce a summary. To match the spec's per-bucket output, we compute
		// orphans as: remote files not in globalReferenced.
		var orphanFiles int
		var orphanBytes int64
		for path, size := range remoteData {
			if _, ok := globalReferenced[path]; !ok {
				orphanFiles++
				orphanBytes += size
			}
		}
		// Attach orphan totals to each bucket for display (matches sample output).
		// For multi-bucket, orphan figures reflect the full location.
		for i := range locResult.Buckets {
			locResult.Buckets[i].OrphanFiles = orphanFiles
			locResult.Buckets[i].OrphanBytes = orphanBytes
			locResult.Buckets[i].RemoteDataFiles = len(remoteData)
			var totalRemoteBytes int64
			for _, s := range remoteData {
				totalRemoteBytes += s
			}
			locResult.Buckets[i].RemoteDataBytes = totalRemoteBytes
		}

		result.Locations = append(result.Locations, locResult)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	printInspectText(result)
	return nil
}

func openSQLiteReadOnly(dbPath string) (*sql.DB, error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(abs)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func validateSQLiteReadOnly(dbPath string) error {
	db, err := openSQLiteReadOnly(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var result string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("%s", result)
	}
	return nil
}

func localSQLiteStatus(dbPath string) string {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		return fmt.Sprintf("stat failed: %v", err)
	}
	if err := validateSQLiteReadOnly(dbPath); err != nil {
		return fmt.Sprintf("corrupt: %v", err)
	}
	return "ok"
}

func loadInspectStructure(dbPath string) ([]meta.Location, []meta.Bucket, error) {
	db, err := openSQLiteReadOnly(dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()

	locRows, err := db.Query(`SELECT id, url, username, password_enc, display_name, quota_bytes, base_dir, created_at FROM webdav_locations ORDER BY created_at`)
	if err != nil {
		return nil, nil, fmt.Errorf("list locations: %w", err)
	}
	defer locRows.Close()

	var locs []meta.Location
	for locRows.Next() {
		var loc meta.Location
		var ts int64
		if err := locRows.Scan(&loc.ID, &loc.URL, &loc.Username, &loc.PasswordEnc, &loc.DisplayName, &loc.QuotaBytes, &loc.BaseDir, &ts); err != nil {
			return nil, nil, fmt.Errorf("scan location: %w", err)
		}
		loc.BaseDir = meta.NormalizeBaseDir(loc.BaseDir)
		loc.CreatedAt = time.Unix(ts, 0)
		locs = append(locs, loc)
	}
	if err := locRows.Err(); err != nil {
		return nil, nil, err
	}

	bucketRows, err := db.Query(`SELECT id, name, owner_user_id, webdav_location_id, created_at FROM buckets ORDER BY created_at`)
	if err != nil {
		return nil, nil, fmt.Errorf("list buckets: %w", err)
	}
	defer bucketRows.Close()

	var buckets []meta.Bucket
	for bucketRows.Next() {
		var bucket meta.Bucket
		var ts int64
		if err := bucketRows.Scan(&bucket.ID, &bucket.Name, &bucket.OwnerUserID, &bucket.WebDAVLocationID, &ts); err != nil {
			return nil, nil, fmt.Errorf("scan bucket: %w", err)
		}
		bucket.CreatedAt = time.Unix(ts, 0)
		buckets = append(buckets, bucket)
	}
	if err := bucketRows.Err(); err != nil {
		return nil, nil, err
	}

	return locs, buckets, nil
}

func loadBucketReferencesReadOnly(dbPath string) ([]bucketReference, error) {
	db, err := openSQLiteReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT key, hash_path, size_bytes FROM objects WHERE upload_complete = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []bucketReference
	for rows.Next() {
		var key, hashPath string
		var size int64
		if err := rows.Scan(&key, &hashPath, &size); err != nil {
			return nil, err
		}
		if hashPath != "" {
			refs = append(refs, bucketReference{path: hashPath, size: size})
			continue
		}
		chunkRows, err := db.Query(`SELECT path, size_bytes FROM object_chunks WHERE object_key = ? ORDER BY part_number`, key)
		if err != nil {
			return nil, err
		}
		for chunkRows.Next() {
			var path string
			var chunkSize int64
			if err := chunkRows.Scan(&path, &chunkSize); err != nil {
				chunkRows.Close()
				return nil, err
			}
			refs = append(refs, bucketReference{path: path, size: chunkSize})
		}
		if err := chunkRows.Err(); err != nil {
			chunkRows.Close()
			return nil, err
		}
		chunkRows.Close()
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	partRows, err := db.Query(`SELECT hash_path, size_bytes FROM multipart_parts`)
	if err != nil {
		return nil, err
	}
	defer partRows.Close()
	for partRows.Next() {
		var hashPath string
		var size int64
		if err := partRows.Scan(&hashPath, &size); err != nil {
			return nil, err
		}
		refs = append(refs, bucketReference{path: hashPath, size: size})
	}
	return refs, partRows.Err()
}

// collectRemoteDataFiles lists all files under _data/ and _parts/ with sizes.
func collectRemoteDataFiles(ctx context.Context, wdc webdav.Client, baseDir string) remoteDataMap {
	m := make(remoteDataMap)

	// _data/{prefix}/{file}
	dataDir := baseDir + "_data/"
	prefixInfos, err := wdc.ReadDirInfo(ctx, dataDir)
	if err == nil {
		for _, prefixFI := range prefixInfos {
			if prefixFI.Name() == ".tmp" {
				continue
			}
			subDir := dataDir + prefixFI.Name()
			fileInfos, err := wdc.ReadDirInfo(ctx, subDir)
			if err != nil {
				continue
			}
			for _, fi := range fileInfos {
				m[subDir+"/"+fi.Name()] = fi.Size()
			}
		}
	}

	// _parts/{uploadID}/{file}
	partsDir := baseDir + "_parts/"
	uploadInfos, err := wdc.ReadDirInfo(ctx, partsDir)
	if err == nil {
		for _, uploadFI := range uploadInfos {
			uploadDir := partsDir + uploadFI.Name()
			fileInfos, err := wdc.ReadDirInfo(ctx, uploadDir)
			if err != nil {
				continue
			}
			for _, fi := range fileInfos {
				m[uploadDir+"/"+fi.Name()] = fi.Size()
			}
		}
	}

	return m
}

func collectRemoteMetaFiles(ctx context.Context, wdc webdav.Client, baseDir string) remoteDataMap {
	m := make(remoteDataMap)
	metaDir := baseDir + "_meta/"
	infos, err := wdc.ReadDirInfo(ctx, metaDir)
	if err != nil {
		return m
	}
	for _, fi := range infos {
		m[metaDir+fi.Name()] = fi.Size()
	}
	return m
}

// inspectBucket returns per-bucket stats and the set of paths it references.
func inspectBucket(
	ctx context.Context,
	cfg *Config,
	wdc webdav.Client,
	bkt meta.Bucket,
	loc meta.Location,
	remoteData remoteDataMap,
) (inspectBucketResult, map[string]struct{}) {
	br := inspectBucketResult{Name: bkt.Name, ID: bkt.ID}

	// Local DB integrity check.
	localDBPath := ""
	if cfg.LocalCacheDir != "" {
		localDBPath = filepath.Join(cfg.LocalCacheDir, "bucket-"+bkt.ID+".db")
	}
	if localDBPath == "" {
		br.LocalMetaStatus = "no local cache dir"
	} else if err := validateSQLiteReadOnly(localDBPath); err != nil {
		br.LocalMetaStatus = fmt.Sprintf("corrupt: %v", err)
	} else {
		br.LocalMetaStatus = "ok"
	}

	// Remote DB integrity check.
	remoteDBPath := fmt.Sprintf("%s_meta/%s.db", loc.BaseDir, bkt.ID)
	br.RemoteMetaStatus = checkRemoteDB(ctx, wdc, remoteDBPath, cfg.LocalCacheDir)

	// List locally referenced paths.
	referenced := make(map[string]struct{})
	if localDBPath != "" {
		refs, err := loadBucketReferencesReadOnly(localDBPath)
		if err == nil {
			for _, ref := range refs {
				referenced[ref.path] = struct{}{}
				br.ReferencedFiles++
				br.ReferencedBytes += ref.size
			}
		}
	}

	// Missing: locally referenced but not present in remote data map.
	for path := range referenced {
		if _, ok := remoteData[path]; !ok {
			br.MissingFiles++
		}
	}

	return br, referenced
}

// checkRemoteDB downloads the remote bucket DB to a cache temp file and validates it read-only.
func checkRemoteDB(ctx context.Context, wdc webdav.Client, remotePath, cacheDir string) string {
	tmp, err := os.CreateTemp(cacheDir, "inspect-remote-*.db")
	if err != nil {
		return fmt.Sprintf("temp file error: %v", err)
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	if err := wdc.DownloadToFile(ctx, remotePath, tmp.Name()); err != nil {
		return fmt.Sprintf("download failed: %v", err)
	}
	if err := validateSQLiteReadOnly(tmp.Name()); err != nil {
		return fmt.Sprintf("corrupt: %v", err)
	}
	return "ok"
}

func printInspectText(result inspectResult) {
	for _, loc := range result.Locations {
		fmt.Printf("Location: %s (%s)\n", loc.DisplayName, loc.ID)
		if loc.LocalStructureStatus != "" {
			fmt.Printf("  local structure: %s\n", loc.LocalStructureStatus)
		}
		if loc.LocalStatsStatus != "" {
			fmt.Printf("  local stats:     %s\n", loc.LocalStatsStatus)
		}
		fmt.Printf("  remote metadata: %d files, %s\n", loc.RemoteMetaFiles, formatBytes(loc.RemoteMetaBytes))
		if loc.Error != "" {
			fmt.Printf("  error: %s\n\n", loc.Error)
			continue
		}
		if len(loc.Buckets) == 0 {
			fmt.Printf("  (no buckets)\n\n")
			continue
		}
		for _, b := range loc.Buckets {
			fmt.Printf("  Bucket: %s (%s)\n", b.Name, shortID(b.ID))
			fmt.Printf("    local metadata:  %s\n", b.LocalMetaStatus)
			fmt.Printf("    remote metadata: %s\n", b.RemoteMetaStatus)
			fmt.Printf("    referenced data: %d files, %s\n", b.ReferencedFiles, formatBytes(b.ReferencedBytes))
			fmt.Printf("    remote data:     %d files, %s\n", b.RemoteDataFiles, formatBytes(b.RemoteDataBytes))
			fmt.Printf("    orphan data:     %d files, %s\n", b.OrphanFiles, formatBytes(b.OrphanBytes))
			fmt.Printf("    missing data:    %d files\n", b.MissingFiles)
		}
		fmt.Println()
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "..."
	}
	return id
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	val := float64(b) / float64(div)
	suffix := strings.ToUpper(string("kmgtpe"[exp])) + "iB"
	return fmt.Sprintf("%.1f %s", val, suffix)
}
