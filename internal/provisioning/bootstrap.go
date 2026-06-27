package provisioning

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hpolthof/webdavs3/internal/adminui"
	"github.com/hpolthof/webdavs3/internal/bucket"
	"github.com/hpolthof/webdavs3/internal/meta"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// ErrProvisioningSkipped is returned by ProvisioningAllowed when the marker
// file already exists and provisioning should not run again.
var ErrProvisioningSkipped = errors.New("provisioning skipped")

// SyncEngine is the minimal interface provisioning needs for per-location sync.
type SyncEngine interface {
	SyncFromWebDAV(ctx context.Context, locationID string) error
	FindRemoteLocationID(ctx context.Context, locationID string) (remoteID string, found bool, err error)
}

type ApplyConfig struct {
	FilePath      string
	LocalCacheDir string
	EncryptionKey string
	Structure     meta.StructureDB
	Buckets       bucket.Service
	RefreshWebDAV func()
	SyncEngine    SyncEngine // optional; if nil, sync step is skipped
}

type DumpConfig struct {
	EncryptionKey string
	Structure     meta.StructureDB
}

func OpenDumpStructure(localCacheDir string) (meta.StructureDB, error) {
	if localCacheDir == "" {
		return nil, fmt.Errorf("local_cache_dir is required for provision dump")
	}

	structPath := filepath.Join(localCacheDir, "structure.db")
	info, err := os.Stat(structPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("structure.db not found at %s", structPath)
		}
		return nil, fmt.Errorf("stat structure.db: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("structure.db path is a directory: %s", structPath)
	}

	structDB, err := meta.OpenStructureDB(structPath)
	if err != nil {
		return nil, fmt.Errorf("open structure.db: %w", err)
	}
	return structDB, nil
}

// ProvisioningAllowed returns ErrProvisioningSkipped if the marker file already
// exists, indicating provisioning has already run. Any other error is an I/O failure.
func ProvisioningAllowed(markerPath string) error {
	if _, err := os.Stat(markerPath); err == nil {
		return fmt.Errorf("%w: marker file already exists at %s", ErrProvisioningSkipped, markerPath)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat marker: %w", err)
	}
	return nil
}

func ApplyStartupProvisioning(ctx context.Context, cfg ApplyConfig) error {
	if cfg.FilePath == "" {
		return nil
	}
	if cfg.LocalCacheDir == "" {
		return fmt.Errorf("provisioning requires local_cache_dir")
	}

	doc, err := ParseFile(cfg.FilePath)
	if err != nil {
		return fmt.Errorf("parse provision file: %w", err)
	}
	doc.Normalize()
	if err := Validate(doc); err != nil {
		return fmt.Errorf("validate provision file: %w", err)
	}

	markerPath := filepath.Join(cfg.LocalCacheDir, "provisioned.flag")
	if err := ProvisioningAllowed(markerPath); err != nil {
		if errors.Is(err, ErrProvisioningSkipped) {
			slog.Warn("startup provisioning skipped", "reason", err.Error())
			return nil
		}
		return err
	}

	// 1. Add locations.
	locationIDs := make(map[string]string, len(doc.Locations))
	for _, spec := range doc.Locations {
		passwordEnc, err := adminui.EncryptPassword(spec.Password, cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("encrypt location password for %s: %w", spec.Slug, err)
		}
		id := newUUID()
		locationIDs[spec.Slug] = id
		if err := cfg.Structure.AddLocation(meta.Location{
			ID:          id,
			URL:         spec.URL,
			Username:    spec.Username,
			PasswordEnc: passwordEnc,
			DisplayName: spec.DisplayName,
			QuotaBytes:  spec.QuotaGB * (1 << 30),
			BaseDir:     spec.BaseDir,
			CreatedAt:   time.Now(),
		}); err != nil {
			return fmt.Errorf("add location %s: %w", spec.Slug, err)
		}
	}

	// 2. Reconcile location IDs: probe each remote for an existing structure.db.
	//    If the remote already knows this endpoint under a different ID, adopt
	//    that ID so bucket and stats references stay consistent with the remote.
	if cfg.SyncEngine != nil {
		for slug, provisionalID := range locationIDs {
			remoteID, found, err := cfg.SyncEngine.FindRemoteLocationID(ctx, provisionalID)
			if err != nil {
				slog.Warn("provisioning: probe remote location id failed", "location", slug, "err", err)
				continue
			}
			if !found {
				continue
			}
			loc, err := cfg.Structure.GetLocation(provisionalID)
			if err != nil {
				return fmt.Errorf("get provisional location %s: %w", slug, err)
			}
			if err := cfg.Structure.DeleteLocation(provisionalID); err != nil {
				return fmt.Errorf("delete provisional location %s: %w", slug, err)
			}
			loc.ID = remoteID
			if err := cfg.Structure.AddLocation(loc); err != nil {
				return fmt.Errorf("re-add location %s with remote id %s: %w", slug, remoteID, err)
			}
			locationIDs[slug] = remoteID
			slog.Info("provisioning: adopted remote location id", "location", slug, "remote_id", remoteID)
		}
	}

	// 3. Refresh WebDAV connections so the (reconciled) locations are reachable.
	if cfg.RefreshWebDAV != nil {
		cfg.RefreshWebDAV()
	}

	// 4. Sync each location so that users/buckets already on the remote are
	//    imported before we apply the provision file. Remote data takes priority.
	if cfg.SyncEngine != nil {
		for _, spec := range doc.Locations {
			if err := cfg.SyncEngine.SyncFromWebDAV(ctx, locationIDs[spec.Slug]); err != nil {
				slog.Warn("provisioning: sync location failed", "location", spec.Slug, "err", err)
			}
		}
	}

	// 5. Add users from the provision file, skipping any whose AccessKey is
	//    already claimed (e.g. imported by the sync step above).
	userIDs := make(map[string]string, len(doc.Users))
	for _, spec := range doc.Users {
		if existing, err := cfg.Structure.GetUserByAccessKey(spec.AccessKey); err == nil {
			slog.Info("provisioning: user already exists, skipping", "slug", spec.Slug, "access_key", spec.AccessKey)
			userIDs[spec.Slug] = existing.ID
			continue
		}

		secretHash, err := bcrypt.GenerateFromPassword([]byte(spec.SecretKey), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash user secret key for %s: %w", spec.Slug, err)
		}
		webHash, err := bcrypt.GenerateFromPassword([]byte(spec.WebPassword), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash web password for %s: %w", spec.Slug, err)
		}
		secretEnc, err := adminui.EncryptPassword(spec.SecretKey, cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("encrypt user secret key for %s: %w", spec.Slug, err)
		}
		webEnc, err := adminui.EncryptPassword(spec.WebPassword, cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("encrypt web password for %s: %w", spec.Slug, err)
		}

		id := newUUID()
		userIDs[spec.Slug] = id
		if err := cfg.Structure.AddUser(meta.User{
			ID:              id,
			AccessKey:       spec.AccessKey,
			SecretKeyHash:   string(secretHash),
			SecretKeyEnc:    secretEnc,
			WebPasswordHash: string(webHash),
			WebPasswordEnc:  webEnc,
			DisplayName:     spec.DisplayName,
			Enabled:         *spec.Enabled,
			CreatedAt:       time.Now(),
		}); err != nil {
			return fmt.Errorf("add user %s: %w", spec.Slug, err)
		}
	}

	// 6. Add buckets, skipping any whose name already exists (e.g. imported by sync).
	for _, loc := range doc.Locations {
		for _, bucketSpec := range loc.Buckets {
			if _, err := cfg.Structure.GetBucket(bucketSpec.Name); err == nil {
				slog.Info("provisioning: bucket already exists, skipping", "bucket", bucketSpec.Name)
				continue
			}
			if err := cfg.Buckets.CreateBucket(ctx, bucketSpec.Name, userIDs[bucketSpec.Owner], locationIDs[loc.Slug]); err != nil {
				return fmt.Errorf("create bucket %s: %w", bucketSpec.Name, err)
			}
		}
	}

	if err := os.WriteFile(markerPath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	return nil
}

func Dump(_ context.Context, cfg DumpConfig, w io.Writer) error {
	users, err := cfg.Structure.ListUsers()
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	locations, err := cfg.Structure.ListLocations()
	if err != nil {
		return fmt.Errorf("list locations: %w", err)
	}
	buckets, err := cfg.Structure.ListBuckets()
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}

	doc := Document{}
	userSlugByID := make(map[string]string, len(users))
	userSlugSeen := make(map[string]int, len(users))
	for _, user := range users {
		secretKey, err := adminui.DecryptPassword(user.SecretKeyEnc, cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("decrypt user secret key for %s: %w", user.ID, err)
		}
		webPassword, err := adminui.DecryptPassword(user.WebPasswordEnc, cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("decrypt user web password for %s: %w", user.ID, err)
		}

		slug := uniqueSlug(slugify(user.DisplayName), userSlugSeen)
		userSlugByID[user.ID] = slug
		enabled := user.Enabled
		doc.Users = append(doc.Users, UserSpec{
			Slug:        slug,
			DisplayName: user.DisplayName,
			AccessKey:   user.AccessKey,
			SecretKey:   secretKey,
			WebPassword: webPassword,
			Enabled:     &enabled,
		})
	}

	locationIndexByID := make(map[string]int, len(locations))
	locationSlugSeen := make(map[string]int, len(locations))
	for _, location := range locations {
		password, err := adminui.DecryptPassword(location.PasswordEnc, cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("decrypt location password for %s: %w", location.ID, err)
		}

		slug := uniqueSlug(slugify(location.DisplayName), locationSlugSeen)
		locationIndexByID[location.ID] = len(doc.Locations)
		doc.Locations = append(doc.Locations, LocationSpec{
			Slug:        slug,
			DisplayName: location.DisplayName,
			URL:         location.URL,
			Username:    location.Username,
			Password:    password,
			QuotaGB:     location.QuotaBytes / (1 << 30),
			BaseDir:     location.BaseDir,
			Buckets:     []BucketSpec{},
		})
	}

	for _, bucket := range buckets {
		locationIndex, ok := locationIndexByID[bucket.WebDAVLocationID]
		if !ok {
			return fmt.Errorf("bucket %s references unknown location %s", bucket.Name, bucket.WebDAVLocationID)
		}
		ownerSlug, ok := userSlugByID[bucket.OwnerUserID]
		if !ok {
			return fmt.Errorf("bucket %s references unknown owner %s", bucket.Name, bucket.OwnerUserID)
		}

		doc.Locations[locationIndex].Buckets = append(doc.Locations[locationIndex].Buckets, BucketSpec{
			Name:  bucket.Name,
			Owner: ownerSlug,
		})
	}

	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	defer enc.Close()
	return enc.Encode(doc)
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("newUUID: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
