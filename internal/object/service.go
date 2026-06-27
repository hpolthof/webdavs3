package object

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hpolthof/webdav3s/internal/meta"
	wdv "github.com/hpolthof/webdav3s/internal/webdav"
)

// ErrObjectNotFound is returned when a requested object does not exist.
var ErrObjectNotFound = errors.New("object not found")

// ErrBucketNotFound is returned when a requested bucket does not exist.
var ErrBucketNotFound = errors.New("bucket not found")

// ListResult holds the output of a ListObjectsV2 operation.
type ListResult struct {
	Objects               []meta.Object
	CommonPrefixes        []string
	IsTruncated           bool
	NextContinuationToken string
}

// ObjectService manages object CRUD backed by WebDAV content-addressed storage.
type ObjectService interface {
	Put(ctx context.Context, bucketName, key, contentType string, size int64, body io.Reader) (meta.Object, error)
	Get(ctx context.Context, bucketName, key string) (meta.Object, io.ReadCloser, error)
	Delete(ctx context.Context, bucketName, key string) error
	Head(ctx context.Context, bucketName, key string) (meta.Object, error)
	List(ctx context.Context, bucketName, prefix, delimiter, continuationToken string, maxKeys int) (ListResult, error)
}

// GarbageCollector can be implemented by the object service to clean up orphaned data files.
type GarbageCollector interface {
	GarbageCollect(ctx context.Context, locationID string) (int, error)
}

type objectService struct {
	wdc                 wdv.Client
	cache               *meta.LRUCache
	stats               meta.StatsDB
	structure           meta.StructureDB
	cacheDir            string
	mp                  MultipartService // nil = auto-chunk disabled
	chunkThresholdBytes int64
	chunkSizeBytes      int64
}

// New creates a new ObjectService.
// Parameter order: wdc, cache, stats, structure, cacheDir, mp, chunkThresholdBytes, chunkSizeBytes.
// Pass mp=nil and thresholds=0 to disable auto-chunking.
func New(wdc wdv.Client, cache *meta.LRUCache, stats meta.StatsDB, structure meta.StructureDB, cacheDir string, mp MultipartService, chunkThresholdBytes, chunkSizeBytes int64) ObjectService {
	cleanupStaleTempFiles(cacheDir)

	return &objectService{
		wdc:                 wdc,
		cache:               cache,
		stats:               stats,
		structure:           structure,
		cacheDir:            cacheDir,
		mp:                  mp,
		chunkThresholdBytes: chunkThresholdBytes,
		chunkSizeBytes:      chunkSizeBytes,
	}
}

// cleanupStaleTempFiles removes leftover put-*.bin temp files from previous
// (crashed) runs. It runs once during service initialization, before any
// uploads can start. Files younger than tempFileCleanupAge are kept to avoid
// racing with a concurrent process using the same cacheDir.
const tempFileCleanupAge = 5 * time.Minute

func cleanupStaleTempFiles(cacheDir string) {
	if cacheDir == "" {
		return
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		slog.Debug("cleanup stale temp files: readdir failed", "dir", cacheDir, "err", err)
		return
	}
	cutoff := time.Now().Add(-tempFileCleanupAge)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "put-") || !strings.HasSuffix(name, ".bin") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(cacheDir, name)
			if err := os.Remove(path); err != nil {
				slog.Warn("cleanup stale temp file failed", "path", path, "err", err)
			} else {
				slog.Info("cleaned up stale temp file", "path", path)
			}
		}
	}
}

// Put streams body to a local temp file, computes SHA256 + MD5, then either
// uploads directly to WebDAV or auto-chunks via MultipartService when the
// written size exceeds chunkThresholdBytes.
func (s *objectService) Put(ctx context.Context, bucketName, key, contentType string, size int64, body io.Reader) (meta.Object, error) {
	bkt, err := s.structure.GetBucket(bucketName)
	if err != nil {
		return meta.Object{}, fmt.Errorf("%w: %s", ErrBucketNotFound, bucketName)
	}

	// Stream to temp file, compute SHA256 + MD5 simultaneously.
	tmp, err := os.CreateTemp(s.cacheDir, "put-*.bin")
	if err != nil {
		return meta.Object{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	sha := sha256.New()
	md5h := md5.New()
	mw := io.MultiWriter(tmp, sha, md5h)
	written, err := io.Copy(mw, body)
	tmp.Close()
	if err != nil {
		return meta.Object{}, fmt.Errorf("stream to temp: %w", err)
	}

	// Auto-chunk if size exceeds threshold and multipart service is available.
	if s.chunkThresholdBytes > 0 && written > s.chunkThresholdBytes && s.mp != nil {
		return s.putChunked(ctx, bucketName, key, contentType, tmpPath, written)
	}

	loc, err := s.structure.GetLocation(bkt.WebDAVLocationID)
	if err != nil {
		return meta.Object{}, fmt.Errorf("get location: %w", err)
	}

	sha256Hex := hex.EncodeToString(sha.Sum(nil))
	md5Hex := hex.EncodeToString(md5h.Sum(nil))
	etag := `"` + md5Hex + `"`

	hashPath := fmt.Sprintf("%s_data/%s/%s", loc.BaseDir, sha256Hex[:2], sha256Hex)
	dirPath := fmt.Sprintf("%s_data/%s", loc.BaseDir, sha256Hex[:2])
	if err := s.wdc.MkdirAll(ctx, dirPath); err != nil {
		return meta.Object{}, fmt.Errorf("mkdir %s: %w", dirPath, err)
	}

	slog.Debug("uploading object to webdav", "bucket", bucketName, "key", key, "path", hashPath, "size", written)
	if err := s.wdc.UploadFromFile(ctx, hashPath, tmpPath); err != nil {
		slog.Error("upload to webdav failed", "bucket", bucketName, "key", key, "path", hashPath, "err", err)
		return meta.Object{}, fmt.Errorf("upload to WebDAV: %w", err)
	}

	obj := meta.Object{
		ID:             newUUID(),
		Key:            key,
		HashPath:       hashPath,
		SizeBytes:      written,
		ETag:           etag,
		ContentType:    contentType,
		LastModified:   time.Now().UTC(),
		UploadComplete: true,
	}

	bdb, release, err := s.cache.Get(bkt.ID)
	if err != nil {
		return meta.Object{}, fmt.Errorf("open bucket db: %w", err)
	}
	defer release()

	if err := bdb.PutObject(obj); err != nil {
		return meta.Object{}, fmt.Errorf("put object record: %w", err)
	}

	if err := s.syncBucketDB(ctx, bkt.ID, loc.BaseDir, bdb); err != nil {
		return meta.Object{}, fmt.Errorf("sync bucket db: %w", err)
	}

	_ = s.stats.AddDelta(bkt.WebDAVLocationID, bkt.OwnerUserID, bkt.ID, written, 1)

	return obj, nil
}

// putChunked splits the temp file into chunks and uploads via MultipartService.
func (s *objectService) putChunked(ctx context.Context, bucketName, key, contentType, tmpPath string, totalSize int64) (meta.Object, error) {
	uploadID, err := s.mp.Create(ctx, bucketName, key, contentType)
	if err != nil {
		return meta.Object{}, fmt.Errorf("create multipart upload: %w", err)
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		_ = s.mp.Abort(ctx, bucketName, uploadID)
		return meta.Object{}, fmt.Errorf("open temp for chunked put: %w", err)
	}
	defer f.Close()

	chunkSize := s.chunkSizeBytes
	if chunkSize <= 0 {
		chunkSize = 100 * 1024 * 1024 // 100 MB default
	}

	var completeParts []CompletePart
	partNum := 1
	remaining := totalSize

	for remaining > 0 {
		partSize := chunkSize
		if remaining < chunkSize {
			partSize = remaining
		}
		lr := io.LimitReader(f, partSize)
		etag, err := s.mp.UploadPart(ctx, bucketName, key, uploadID, partNum, partSize, lr)
		if err != nil {
			_ = s.mp.Abort(ctx, bucketName, uploadID)
			return meta.Object{}, fmt.Errorf("upload part %d: %w", partNum, err)
		}
		completeParts = append(completeParts, CompletePart{PartNumber: partNum, ETag: etag})
		remaining -= partSize
		partNum++
	}

	obj, err := s.mp.Complete(ctx, bucketName, key, uploadID, completeParts)
	if err != nil {
		_ = s.mp.Abort(ctx, bucketName, uploadID)
		return meta.Object{}, fmt.Errorf("complete multipart upload: %w", err)
	}
	return obj, nil
}


// Get returns the Object metadata and a streaming reader from WebDAV.
// If the object is chunked (len(obj.Chunks) > 0), the chunks are streamed
// sequentially as a single ReadCloser via io.MultiReader.
func (s *objectService) Get(ctx context.Context, bucketName, key string) (meta.Object, io.ReadCloser, error) {
	obj, _, err := s.lookupObject(ctx, bucketName, key)
	if err != nil {
		return meta.Object{}, nil, err
	}

	if len(obj.Chunks) > 0 {
		return s.getChunked(ctx, obj)
	}

	slog.Debug("downloading object from webdav", "bucket", bucketName, "key", key, "path", obj.HashPath)
	rc, err := s.wdc.Download(ctx, obj.HashPath)
	if err != nil {
		slog.Error("download from webdav failed", "bucket", bucketName, "key", key, "path", obj.HashPath, "err", err)
		return meta.Object{}, nil, fmt.Errorf("download %s: %w", obj.HashPath, err)
	}
	return obj, rc, nil
}

// getChunked opens each chunk as a separate WebDAV download and stitches them
// together into a single ReadCloser via io.MultiReader.
func (s *objectService) getChunked(ctx context.Context, obj meta.Object) (meta.Object, io.ReadCloser, error) {
	closers := make([]io.ReadCloser, 0, len(obj.Chunks))
	readers := make([]io.Reader, 0, len(obj.Chunks))

	closeAll := func() {
		for _, rc := range closers {
			rc.Close()
		}
	}

	for _, c := range obj.Chunks {
		rc, err := s.wdc.Download(ctx, c.Path)
		if err != nil {
			closeAll()
			return meta.Object{}, nil, fmt.Errorf("download chunk %d (%s): %w", c.PartNumber, c.Path, err)
		}
		closers = append(closers, rc)
		readers = append(readers, rc)
	}

	return obj, &multiReadCloser{Reader: io.MultiReader(readers...), closers: closers}, nil
}

// multiReadCloser combines io.MultiReader with multiple Closers.
type multiReadCloser struct {
	io.Reader
	closers []io.ReadCloser
}

func (m *multiReadCloser) Close() error {
	var last error
	for _, rc := range m.closers {
		if err := rc.Close(); err != nil {
			last = err
		}
	}
	return last
}

// Delete removes the object from BucketDB and cleans up WebDAV data files.
// For regular objects, the /_data/ file is only removed when no other object
// in any bucket on the same location still references it (dedup guard).
// For multipart objects, chunk files are deleted directly (paths are unique per upload).
func (s *objectService) Delete(ctx context.Context, bucketName, key string) error {
	obj, bkt, err := s.lookupObject(ctx, bucketName, key)
	if err != nil {
		return err
	}

	loc, err := s.structure.GetLocation(bkt.WebDAVLocationID)
	if err != nil {
		return fmt.Errorf("get location: %w", err)
	}

	bdb, release, err := s.cache.Get(bkt.ID)
	if err != nil {
		return fmt.Errorf("open bucket db: %w", err)
	}
	defer release()

	if err := bdb.DeleteObject(key); err != nil {
		return fmt.Errorf("delete object record: %w", err)
	}

	if err := s.syncBucketDB(ctx, bkt.ID, loc.BaseDir, bdb); err != nil {
		return fmt.Errorf("sync bucket db: %w", err)
	}

	_ = s.stats.AddDelta(bkt.WebDAVLocationID, bkt.OwnerUserID, bkt.ID, -obj.SizeBytes, -1)

	if len(obj.Chunks) > 0 {
		for _, chunk := range obj.Chunks {
			if err := s.wdc.Delete(ctx, chunk.Path); err != nil {
				slog.Warn("delete chunk failed", "path", chunk.Path, "err", err)
			}
		}
	} else if obj.HashPath != "" {
		if err := s.maybeDeleteDataFile(ctx, obj.HashPath, bkt, bdb); err != nil {
			slog.Warn("data file cleanup failed", "hashPath", obj.HashPath, "err", err)
		}
	}

	return nil
}

// maybeDeleteDataFile deletes hashPath from WebDAV only when no remaining object
// in any bucket on the same WebDAV location still references it.
func (s *objectService) maybeDeleteDataFile(ctx context.Context, hashPath string, bkt meta.Bucket, currentBDB meta.BucketDB) error {
	count, err := currentBDB.CountByHashPath(hashPath)
	if err != nil {
		return fmt.Errorf("count refs in current bucket: %w", err)
	}
	if count > 0 {
		return nil
	}

	allBuckets, err := s.structure.ListBuckets()
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	for _, b := range allBuckets {
		if b.ID == bkt.ID || b.WebDAVLocationID != bkt.WebDAVLocationID {
			continue
		}
		bdb, release, err := s.cache.Get(b.ID)
		if err != nil {
			return fmt.Errorf("open bucket db %s: %w", b.ID, err)
		}
		n, countErr := bdb.CountByHashPath(hashPath)
		release()
		if countErr != nil {
			return fmt.Errorf("count refs in bucket %s: %w", b.ID, countErr)
		}
		if n > 0 {
			return nil
		}
	}

	if err := s.wdc.Delete(ctx, hashPath); err != nil {
		return fmt.Errorf("delete data file %s: %w", hashPath, err)
	}
	slog.Debug("deleted data file", "hashPath", hashPath)
	return nil
}

// Head returns the Object metadata without touching WebDAV.
func (s *objectService) Head(ctx context.Context, bucketName, key string) (meta.Object, error) {
	obj, _, err := s.lookupObject(ctx, bucketName, key)
	return obj, err
}

// List implements ListObjectsV2 semantics. ContinuationToken = base64(lastKey).
func (s *objectService) List(ctx context.Context, bucketName, prefix, delimiter, continuationToken string, maxKeys int) (ListResult, error) {
	bkt, err := s.structure.GetBucket(bucketName)
	if err != nil {
		return ListResult{}, fmt.Errorf("bucket %q not found: %w", bucketName, err)
	}
	bdb, release, err := s.cache.Get(bkt.ID)
	if err != nil {
		return ListResult{}, fmt.Errorf("open bucket db: %w", err)
	}
	defer release()

	startAfter := ""
	if continuationToken != "" {
		decoded, err := base64.StdEncoding.DecodeString(continuationToken)
		if err != nil {
			return ListResult{}, fmt.Errorf("invalid continuation token: %w", err)
		}
		startAfter = string(decoded)
	}

	// Default maxKeys to 1000 (S3 default)
	fetchMax := maxKeys
	if maxKeys <= 0 {
		fetchMax = 1000
	}
	// Fetch maxKeys+1 to detect truncation across objects + common prefixes.
	fetchMax++

	objects, commonPrefixes, err := bdb.ListObjects(prefix, delimiter, startAfter, fetchMax)
	if err != nil {
		return ListResult{}, fmt.Errorf("list objects: %w", err)
	}

	var result ListResult
	// Count combined total to correctly detect truncation when common prefixes
	// are present (fixes IsTruncated ignoring common prefixes).
	total := len(objects) + len(commonPrefixes)
	if maxKeys <= 0 {
		maxKeys = 1000
	}
	if total > maxKeys {
		result.IsTruncated = true
		// Trim to maxKeys combined, preferring objects first then prefixes.
		if len(objects) > maxKeys {
			result.Objects = objects[:maxKeys]
			result.CommonPrefixes = nil
		} else {
			result.Objects = objects
			remaining := maxKeys - len(objects)
			result.CommonPrefixes = commonPrefixes[:remaining]
		}
		// Next token is the last object key (or last prefix if no objects remain).
		if len(result.Objects) > 0 {
			lastKey := result.Objects[len(result.Objects)-1].Key
			result.NextContinuationToken = base64.StdEncoding.EncodeToString([]byte(lastKey))
		} else if len(result.CommonPrefixes) > 0 {
			lastKey := result.CommonPrefixes[len(result.CommonPrefixes)-1]
			result.NextContinuationToken = base64.StdEncoding.EncodeToString([]byte(lastKey))
		}
	} else {
		result.Objects = objects
		result.CommonPrefixes = commonPrefixes
	}
	return result, nil
}

// lookupObject resolves bucket + key → (Object, Bucket).
func (s *objectService) lookupObject(ctx context.Context, bucketName, key string) (meta.Object, meta.Bucket, error) {
	bkt, err := s.structure.GetBucket(bucketName)
	if err != nil {
		return meta.Object{}, meta.Bucket{}, fmt.Errorf("%w: %s", ErrBucketNotFound, bucketName)
	}
	bdb, release, err := s.cache.Get(bkt.ID)
	if err != nil {
		return meta.Object{}, meta.Bucket{}, fmt.Errorf("open bucket db: %w", err)
	}
	defer release()

	obj, err := bdb.GetObject(key)
	if err != nil {
		return meta.Object{}, meta.Bucket{}, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
	}
	return obj, bkt, nil
}

// syncBucketDB saves BucketDB to a temp file and uploads to WebDAV as {baseDir}_meta/{bucketID}.db.
func (s *objectService) syncBucketDB(ctx context.Context, bucketID, baseDir string, bdb meta.BucketDB) error {
	tmp, err := os.CreateTemp(s.cacheDir, "bdb-sync-*.db")
	if err != nil {
		return fmt.Errorf("create temp for sync: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	// Remove the file so SaveToFile (VACUUM INTO) can create it fresh.
	os.Remove(tmpPath)
	defer os.Remove(tmpPath)

	if err := bdb.SaveToFile(tmpPath); err != nil {
		return fmt.Errorf("save bucket db: %w", err)
	}
	return s.wdc.UploadFromFile(ctx, fmt.Sprintf("%s_meta/%s.db", baseDir, bucketID), tmpPath)
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
