package sync

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hpolthof/webdavs3/internal/meta"
	wdv "github.com/hpolthof/webdavs3/internal/webdav"
)

// isNotFound returns true when err represents an HTTP 404 from the WebDAV server.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	type statusCoder interface{ StatusCode() int }
	if sc, ok := err.(statusCoder); ok {
		return sc.StatusCode() == http.StatusNotFound
	}
	msg := err.Error()
	return strings.Contains(msg, "404") || strings.Contains(msg, "Not Found")
}

// primaryBaseDir returns the BaseDir of the first configured location, or "/" if none.
func (e *engine) primaryBaseDir() string {
	locs, err := e.structure.ListLocations()
	if err != nil || len(locs) == 0 {
		return "/"
	}
	return locs[0].BaseDir
}

// Engine handles sync between local SQLite state and WebDAV-stored databases.
type Engine interface {
	SyncFromWebDAV(ctx context.Context, locationID string) error
	// FindRemoteLocationID probes the remote structure.db for the given location
	// and returns the ID under which the same endpoint is already stored there.
	// Returns ("", false, nil) if no remote structure.db exists or the endpoint
	// is not found. A non-nil error means an I/O or parse failure.
	FindRemoteLocationID(ctx context.Context, locationID string) (remoteID string, found bool, err error)
	FlushStructure(ctx context.Context) error
	FlushStats(ctx context.Context) error
}

type engine struct {
	wdc               wdv.Client
	structure         meta.StructureDB
	stats             meta.StatsDB
	daemonID          string
	localCacheDir     string
	encryptionKey     string
	decryptPassword   func(encrypted, key string) (string, error)
	newLocationClient func(url, username, password string) wdv.Client
}

// New creates a sync Engine using the provided client for all operations.
func New(wdc wdv.Client, structure meta.StructureDB, stats meta.StatsDB, daemonID string) Engine {
	return &engine{
		wdc:               wdc,
		structure:         structure,
		stats:             stats,
		daemonID:          daemonID,
		newLocationClient: func(url, username, password string) wdv.Client { return wdv.New(url, username, password) },
	}
}

// NewWithEncryption creates a sync Engine that can decrypt location passwords.
func NewWithEncryption(wdc wdv.Client, structure meta.StructureDB, stats meta.StatsDB, daemonID, encryptionKey string, decryptPassword func(encrypted, key string) (string, error)) Engine {
	return NewWithEncryptionAndCacheDir(wdc, structure, stats, daemonID, "", encryptionKey, decryptPassword)
}

// NewWithEncryptionAndCacheDir creates a sync Engine that also restores bucket
// metadata files into localCacheDir during SyncFromWebDAV.
func NewWithEncryptionAndCacheDir(wdc wdv.Client, structure meta.StructureDB, stats meta.StatsDB, daemonID, localCacheDir, encryptionKey string, decryptPassword func(encrypted, key string) (string, error)) Engine {
	return &engine{
		wdc:               wdc,
		structure:         structure,
		stats:             stats,
		daemonID:          daemonID,
		localCacheDir:     localCacheDir,
		encryptionKey:     encryptionKey,
		decryptPassword:   decryptPassword,
		newLocationClient: func(url, username, password string) wdv.Client { return wdv.New(url, username, password) },
	}
}

// NewWithClientFactory creates a sync Engine with a custom client factory (for testing).
func NewWithClientFactory(wdc wdv.Client, structure meta.StructureDB, stats meta.StatsDB, daemonID string, newClient func(url, username, password string) wdv.Client) Engine {
	return NewWithClientFactoryAndCacheDir(wdc, structure, stats, daemonID, "", newClient)
}

// NewWithClientFactoryAndCacheDir creates a sync Engine with a custom client
// factory and local bucket metadata restore path.
func NewWithClientFactoryAndCacheDir(wdc wdv.Client, structure meta.StructureDB, stats meta.StatsDB, daemonID, localCacheDir string, newClient func(url, username, password string) wdv.Client) Engine {
	return &engine{
		wdc:               wdc,
		structure:         structure,
		stats:             stats,
		daemonID:          daemonID,
		localCacheDir:     localCacheDir,
		newLocationClient: newClient,
	}
}

// SyncFromWebDAV downloads structure.db from the specified WebDAV location and merges
// remote records into the local StructureDB. Existing local records win on
// conflict (same ID/name). Remote-only records are imported.
func (e *engine) SyncFromWebDAV(ctx context.Context, locationID string) error {
	slog.Info("sync from webdav started", "location", locationID)
	// Look up location by ID.
	loc, err := e.structure.GetLocation(locationID)
	if err != nil {
		return fmt.Errorf("get location %s: %w", locationID, err)
	}

	// Decrypt password if needed.
	password := loc.PasswordEnc
	if e.decryptPassword != nil && e.encryptionKey != "" {
		password, err = e.decryptPassword(loc.PasswordEnc, e.encryptionKey)
		if err != nil {
			return fmt.Errorf("decrypt password: %w", err)
		}
	}

	// Create a location-specific WebDAV client.
	locClient := e.newLocationClient(loc.URL, loc.Username, password)

	// Ensure the _meta directory exists on WebDAV under the location's base directory.
	metaDir := fmt.Sprintf("%s_meta/", loc.BaseDir)
	if err := locClient.MkdirAll(ctx, metaDir); err != nil {
		return fmt.Errorf("mkdirall %s: %w", metaDir, err)
	}

	// Download structure.db to a local temp file.
	tmp, err := os.CreateTemp(e.localCacheDir, "structure-sync-*.db")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	structureRemotePath := fmt.Sprintf("%s_meta/structure.db", loc.BaseDir)
	if err := locClient.DownloadToFile(ctx, structureRemotePath, tmpPath); err != nil {
		// On a fresh deployment the remote structure.db does not exist yet.
		// Treat a 404 / not-found as a no-op rather than a failure.
		if isNotFound(err) {
			slog.Info("sync from webdav completed", "location", locationID, "result", "remote structure.db not found")
			return nil
		}
		return fmt.Errorf("download structure.db: %w", err)
	}

	remoteDB, err := meta.OpenStructureDB(tmpPath)
	if err != nil {
		return fmt.Errorf("open remote structure.db: %w", err)
	}
	defer remoteDB.Close()

	locationIDMap, err := e.mergeStructure(remoteDB, loc)
	if err != nil {
		return err
	}
	if err := e.syncBucketDBs(ctx, locClient, remoteDB, locationIDMap, locationID, loc.BaseDir); err != nil {
		return err
	}
	if err := e.syncStatsDBs(ctx, locClient, locationIDMap, loc.BaseDir); err != nil {
		return err
	}
	slog.Info("sync from webdav completed", "location", locationID)
	return nil
}

// FindRemoteLocationID probes the remote structure.db for the given local
// location and returns the ID stored there for the same endpoint.
func (e *engine) FindRemoteLocationID(ctx context.Context, locationID string) (string, bool, error) {
	loc, err := e.structure.GetLocation(locationID)
	if err != nil {
		return "", false, fmt.Errorf("get location %s: %w", locationID, err)
	}

	password := loc.PasswordEnc
	if e.decryptPassword != nil && e.encryptionKey != "" {
		password, err = e.decryptPassword(loc.PasswordEnc, e.encryptionKey)
		if err != nil {
			return "", false, fmt.Errorf("decrypt password: %w", err)
		}
	}

	locClient := e.newLocationClient(loc.URL, loc.Username, password)

	tmp, err := os.CreateTemp(e.localCacheDir, "structure-probe-*.db")
	if err != nil {
		return "", false, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	remotePath := fmt.Sprintf("%s_meta/structure.db", loc.BaseDir)
	if err := locClient.DownloadToFile(ctx, remotePath, tmpPath); err != nil {
		if isNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("download structure.db: %w", err)
	}

	remoteDB, err := meta.OpenStructureDB(tmpPath)
	if err != nil {
		return "", false, fmt.Errorf("open remote structure.db: %w", err)
	}
	defer remoteDB.Close()

	remoteLocs, err := remoteDB.ListLocations()
	if err != nil {
		return "", false, fmt.Errorf("list remote locations: %w", err)
	}

	for _, remoteLoc := range remoteLocs {
		if sameLocationEndpoint(loc, remoteLoc) && remoteLoc.ID != locationID {
			return remoteLoc.ID, true, nil
		}
	}
	return "", false, nil
}

// mergeStructure imports remote-only records into the local StructureDB.
// Local records win when the same ID/name exists on both sides.
func (e *engine) mergeStructure(remote meta.StructureDB, targetLoc meta.Location) (map[string]string, error) {
	locationIDMap := map[string]string{}

	localLocs, err := e.structure.ListLocations()
	if err != nil {
		return nil, fmt.Errorf("list local locations: %w", err)
	}

	remoteLocs, err := remote.ListLocations()
	if err != nil {
		return nil, fmt.Errorf("list remote locations: %w", err)
	}
	for _, loc := range remoteLocs {
		if sameLocationEndpoint(targetLoc, loc) {
			locationIDMap[loc.ID] = targetLoc.ID
			continue
		}
		if localID, ok := findMatchingLocationID(localLocs, loc); ok {
			locationIDMap[loc.ID] = localID
			continue
		}
		if _, err := e.structure.GetLocation(loc.ID); err != nil {
			if err := e.structure.AddLocation(loc); err != nil {
				return nil, fmt.Errorf("merge location %s: %w", loc.ID, err)
			}
		}
		locationIDMap[loc.ID] = loc.ID
	}

	remoteUsers, err := remote.ListUsers()
	if err != nil {
		return nil, fmt.Errorf("list remote users: %w", err)
	}
	for _, u := range remoteUsers {
		if _, err := e.structure.GetUser(u.ID); err != nil {
			if err := e.structure.AddUser(meta.User{
				ID:              u.ID,
				AccessKey:       u.AccessKey,
				SecretKeyHash:   u.SecretKeyHash,
				SecretKeyEnc:    u.SecretKeyEnc,
				WebPasswordHash: u.WebPasswordHash,
				WebPasswordEnc:  u.WebPasswordEnc,
				DisplayName:     u.DisplayName,
				Enabled:         u.Enabled,
				CreatedAt:       u.CreatedAt,
			}); err != nil {
				return nil, fmt.Errorf("merge user %s: %w", u.ID, err)
			}
		}
	}

	remoteBuckets, err := remote.ListBuckets()
	if err != nil {
		return nil, fmt.Errorf("list remote buckets: %w", err)
	}
	for _, b := range remoteBuckets {
		localLocationID := b.WebDAVLocationID
		if mapped, ok := locationIDMap[b.WebDAVLocationID]; ok {
			localLocationID = mapped
		}

		existing, err := e.structure.GetBucket(b.Name)
		if err == nil {
			if existing.WebDAVLocationID != localLocationID {
				existing.WebDAVLocationID = localLocationID
				if err := e.structure.UpdateBucket(existing); err != nil {
					return nil, fmt.Errorf("update bucket %s location: %w", b.Name, err)
				}
			}
			continue
		}

		b.WebDAVLocationID = localLocationID
		if err := e.structure.AddBucket(b); err != nil {
			return nil, fmt.Errorf("merge bucket %s: %w", b.Name, err)
		}
	}
	return locationIDMap, nil
}

func findMatchingLocationID(localLocs []meta.Location, remoteLoc meta.Location) (string, bool) {
	for _, loc := range localLocs {
		if sameLocationEndpoint(loc, remoteLoc) {
			return loc.ID, true
		}
	}
	return "", false
}

func sameLocationEndpoint(a, b meta.Location) bool {
	return strings.TrimRight(a.URL, "/") == strings.TrimRight(b.URL, "/") &&
		a.Username == b.Username
}

func (e *engine) syncBucketDBs(ctx context.Context, locClient wdv.Client, remote meta.StructureDB, locationIDMap map[string]string, targetLocationID, baseDir string) error {
	if e.localCacheDir == "" {
		return nil
	}
	remoteBuckets, err := remote.ListBuckets()
	if err != nil {
		return fmt.Errorf("list remote buckets for metadata sync: %w", err)
	}
	for _, b := range remoteBuckets {
		localLocationID := b.WebDAVLocationID
		if mapped, ok := locationIDMap[b.WebDAVLocationID]; ok {
			localLocationID = mapped
		}
		if localLocationID != targetLocationID {
			continue
		}

		localPath := filepath.Join(e.localCacheDir, "bucket-"+b.ID+".db")
		remotePath := fmt.Sprintf("%s_meta/%s.db", baseDir, b.ID)
		if err := locClient.DownloadToFile(ctx, remotePath, localPath); err != nil {
			if isNotFound(err) {
				continue
			}
			return fmt.Errorf("download bucket db %s: %w", b.ID, err)
		}
	}
	return nil
}

func (e *engine) syncStatsDBs(ctx context.Context, locClient wdv.Client, locationIDMap map[string]string, baseDir string) error {
	metaDir := fmt.Sprintf("%s_meta", baseDir)
	names, err := locClient.ReadDir(ctx, metaDir)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("list %s for stats sync: %w", metaDir, err)
	}

	for _, name := range names {
		if !strings.HasPrefix(name, "stats-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		if name == fmt.Sprintf("stats-%s.db", e.daemonID) {
			continue
		}

		tmp, err := os.CreateTemp(e.localCacheDir, "stats-sync-*.db")
		if err != nil {
			return fmt.Errorf("create temp stats sync: %w", err)
		}
		tmpPath := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpPath)

		remotePath := path.Join(metaDir, name)
		if err := locClient.DownloadToFile(ctx, remotePath, tmpPath); err != nil {
			if isNotFound(err) {
				continue
			}
			return fmt.Errorf("download stats db %s: %w", name, err)
		}
		if err := e.stats.MergeFromFile(tmpPath, name, locationIDMap); err != nil {
			return fmt.Errorf("merge stats db %s: %w", name, err)
		}
	}
	return nil
}

// FlushStructure uploads the local structure.db to WebDAV under the primary location's base directory.
func (e *engine) FlushStructure(ctx context.Context) error {
	baseDir := e.primaryBaseDir()
	metaDir := fmt.Sprintf("%s_meta/", baseDir)
	if err := e.wdc.MkdirAll(ctx, metaDir); err != nil {
		return fmt.Errorf("mkdirall %s: %w", metaDir, err)
	}

	tmp, err := os.CreateTemp(e.localCacheDir, "structure-flush-*.db")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if err := e.structure.SaveToFile(tmpPath); err != nil {
		return fmt.Errorf("save structure.db: %w", err)
	}
	remotePath := fmt.Sprintf("%s_meta/structure.db", baseDir)
	if err := e.wdc.UploadFromFile(ctx, remotePath, tmpPath); err != nil {
		return fmt.Errorf("upload structure.db: %w", err)
	}
	return nil
}

// FlushStats uploads the local stats database to WebDAV at the daemon-specific path.
func (e *engine) FlushStats(ctx context.Context) error {
	baseDir := e.primaryBaseDir()
	remotePath := fmt.Sprintf("%s_meta/stats-%s.db", baseDir, e.daemonID)
	return e.stats.Flush(ctx, e.wdc, remotePath)
}
