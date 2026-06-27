package bucket

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hpolthof/webdavs3/internal/meta"
	wdv "github.com/hpolthof/webdavs3/internal/webdav"
)

// bucketNameRE enforces S3 bucket naming: 3-63 lowercase/digit/hyphen,
// must not start or end with hyphen.
var bucketNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{1,61}[a-z0-9]$`)

// Service manages bucket lifecycle.
type Service interface {
	CreateBucket(ctx context.Context, name, ownerUserID, locationID string) error
	DeleteBucket(ctx context.Context, name string) error
	ListBuckets(ctx context.Context, ownerUserID string) ([]meta.Bucket, error)
}

type service struct {
	structure      meta.StructureDB
	stats          meta.StatsDB
	wdc            wdv.Client
	flushStructure func()
	cacheDir       string
}

// New creates a bucket Service.
func New(structure meta.StructureDB, stats meta.StatsDB, wdc wdv.Client) Service {
	return NewWithFlush(structure, stats, wdc, nil)
}

// NewWithFlush creates a bucket Service with an optional callback that is
// invoked after the bucket list in StructureDB changes.
func NewWithFlush(structure meta.StructureDB, stats meta.StatsDB, wdc wdv.Client, flushStructure func()) Service {
	return NewWithFlushAndCacheDir(structure, stats, wdc, flushStructure, "")
}

// NewWithFlushAndCacheDir creates a bucket Service that writes temp files to cacheDir.
func NewWithFlushAndCacheDir(structure meta.StructureDB, stats meta.StatsDB, wdc wdv.Client, flushStructure func(), cacheDir string) Service {
	return &service{structure: structure, stats: stats, wdc: wdc, flushStructure: flushStructure, cacheDir: cacheDir}
}

// validateBucketName enforces DNS-compatible S3 bucket naming rules.
func validateBucketName(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return fmt.Errorf("bucket name %q must be 3-63 characters", name)
	}
	if !bucketNameRE.MatchString(name) {
		return fmt.Errorf("bucket name %q is invalid: must be lowercase, start/end with letter or digit, no consecutive dots or hyphens at edges", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("bucket name %q must not contain consecutive dots", name)
	}
	return nil
}

// CreateBucket validates the name, records the bucket in StructureDB, creates
// the base directory + _meta/ directory on WebDAV, and uploads an empty bucket database.
func (s *service) CreateBucket(ctx context.Context, name, ownerUserID, locationID string) error {
	if err := validateBucketName(name); err != nil {
		return err
	}
	if strings.TrimSpace(locationID) == "" {
		return fmt.Errorf("no webdav location configured")
	}

	loc, err := s.structure.GetLocation(locationID)
	if err != nil {
		return fmt.Errorf("get location: %w", err)
	}

	// Check for duplicate
	if _, err := s.structure.GetBucket(name); err == nil {
		return fmt.Errorf("bucket %q already exists", name)
	}

	bucketID := newUUID()

	// Ensure base directory and _meta/ exist on WebDAV
	metaDir := fmt.Sprintf("%s_meta", loc.BaseDir)
	if err := s.wdc.MkdirAll(ctx, metaDir); err != nil {
		return fmt.Errorf("mkdir %s: %w", metaDir, err)
	}

	// Create an empty bucket DB in a temp file, then upload it.
	tmp, err := os.CreateTemp(s.cacheDir, "bucket-*.db")
	if err != nil {
		return fmt.Errorf("create temp bucket db: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	// Remove up front so OpenBucketDB can create it fresh.
	os.Remove(tmpPath)
	defer os.Remove(tmpPath)

	bdb, err := meta.OpenBucketDB(tmpPath)
	if err != nil {
		return fmt.Errorf("open bucket db: %w", err)
	}
	bdb.Close()

	remotePath := fmt.Sprintf("%s_meta/%s.db", loc.BaseDir, bucketID)
	if err := s.wdc.UploadFromFile(ctx, remotePath, tmpPath); err != nil {
		return fmt.Errorf("upload bucket db to WebDAV: %w", err)
	}

	b := meta.Bucket{
		ID:               bucketID,
		Name:             name,
		OwnerUserID:      ownerUserID,
		WebDAVLocationID: locationID,
		CreatedAt:        time.Now(),
	}
	if err := s.structure.AddBucket(b); err != nil {
		return fmt.Errorf("record bucket in structure db: %w", err)
	}
	if s.flushStructure != nil {
		s.flushStructure()
	}
	return nil
}

// DeleteBucket removes the bucket record and its WebDAV metadata file.
func (s *service) DeleteBucket(ctx context.Context, name string) error {
	b, err := s.structure.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket %q: %w", name, err)
	}

	loc, err := s.structure.GetLocation(b.WebDAVLocationID)
	if err != nil {
		return fmt.Errorf("get location: %w", err)
	}

	// Remove the bucket db from WebDAV
	remotePath := fmt.Sprintf("%s_meta/%s.db", loc.BaseDir, b.ID)
	// Ignore WebDAV delete error — StructureDB removal is authoritative.
	_ = s.wdc.Delete(ctx, remotePath)

	if err := s.structure.DeleteBucket(name); err != nil {
		return err
	}
	if s.flushStructure != nil {
		s.flushStructure()
	}
	return nil
}

// ListBuckets returns all buckets owned by ownerUserID.
// If ownerUserID is empty, returns all buckets.
func (s *service) ListBuckets(ctx context.Context, ownerUserID string) ([]meta.Bucket, error) {
	if ownerUserID == "" {
		return s.structure.ListBuckets()
	}
	return s.structure.ListBucketsByUser(ownerUserID)
}

// newUUID generates a random UUID v4 using crypto/rand.
func newUUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("newUUID: %v", err))
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}
