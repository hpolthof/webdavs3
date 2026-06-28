package object

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hpolthof/webdavs3/internal/meta"
	"github.com/hpolthof/webdavs3/internal/repair"
	wdv "github.com/hpolthof/webdavs3/internal/webdav"
)

// ErrUploadNotFound is returned when the multipart upload ID is not found.
var ErrUploadNotFound = errors.New("multipart upload not found")

// ErrPartNotFound is returned when a part has not been uploaded.
var ErrPartNotFound = errors.New("part not found")

// ErrPartETagMismatch is returned when a part's ETag does not match.
var ErrPartETagMismatch = errors.New("part ETag mismatch")

// CompletePart identifies a part in a CompleteMultipartUpload request.
type CompletePart struct {
	PartNumber int
	ETag       string
}

// MultipartService manages S3 multipart upload lifecycle.
type MultipartService interface {
	Create(ctx context.Context, bucketName, key, contentType string) (uploadID string, err error)
	UploadPart(ctx context.Context, bucketName, key, uploadID string, partNum int, size int64, body io.Reader) (etag string, err error)
	Complete(ctx context.Context, bucketName, key, uploadID string, parts []CompletePart) (meta.Object, error)
	Abort(ctx context.Context, bucketName, uploadID string) error
	ListUploads(ctx context.Context, bucketName string) ([]meta.MultipartUpload, error)
	ListParts(ctx context.Context, bucketName, uploadID string) ([]meta.MultipartPart, error)
}

type multipartService struct {
	wdc       wdv.Client
	cache     *meta.LRUCache
	stats     meta.StatsDB
	structure meta.StructureDB
	cacheDir  string
	repair    *repair.Manager
}

// NewMultipartService creates a MultipartService.
// Parameter order: wdc, cache, stats, structure, cacheDir.
func NewMultipartService(wdc wdv.Client, cache *meta.LRUCache, stats meta.StatsDB, structure meta.StructureDB, cacheDir string) MultipartService {
	return NewMultipartServiceWithRepair(wdc, cache, stats, structure, cacheDir, nil)
}

func NewMultipartServiceWithRepair(wdc wdv.Client, cache *meta.LRUCache, stats meta.StatsDB, structure meta.StructureDB, cacheDir string, repairMgr *repair.Manager) MultipartService {
	return &multipartService{wdc: wdc, cache: cache, stats: stats, structure: structure, cacheDir: cacheDir, repair: repairMgr}
}

// Create initiates a new multipart upload and records it in BucketDB.
func (m *multipartService) Create(ctx context.Context, bucketName, key, contentType string) (string, error) {
	bkt, err := m.structure.GetBucket(bucketName)
	if err != nil {
		return "", fmt.Errorf("bucket %q not found: %w", bucketName, err)
	}
	if err := m.checkWriteAllowed(bkt.ID); err != nil {
		return "", err
	}
	bdb, release, err := m.cache.Get(bkt.ID)
	if err != nil {
		return "", fmt.Errorf("open bucket db: %w", err)
	}
	defer release()

	uploadID := newUUID()
	up := meta.MultipartUpload{
		ID:          uploadID,
		Key:         key,
		ContentType: contentType,
		Initiated:   time.Now().UTC(),
	}
	if err := bdb.CreateMultipartUpload(up); err != nil {
		return "", fmt.Errorf("create multipart upload record: %w", err)
	}

	loc, err := m.structure.GetLocation(bkt.WebDAVLocationID)
	if err != nil {
		return "", fmt.Errorf("get location: %w", err)
	}

	if err := m.syncBucketDB(ctx, bkt.ID, loc.BaseDir, bdb); err != nil {
		if m.repair != nil {
			m.repair.MarkRepairing(bkt.ID, err.Error())
		}
		return "", fmt.Errorf("sync bucket db: %w", err)
	}

	return uploadID, nil
}

// uploadPartDirect streams body to a fixed path on WebDAV and returns MD5 hex and bytes written.
// Unlike uploadStream, this does not content-address the file.
func uploadPartDirect(ctx context.Context, wdc wdv.Client, body io.Reader, size int64, destPath string) (md5Hex string, written int64, err error) {
	md5h := md5.New()
	pr, pw := io.Pipe()

	uploadErr := make(chan error, 1)
	go func() {
		uploadErr <- wdc.Upload(ctx, destPath, pr, size)
	}()

	mw := io.MultiWriter(pw, md5h)
	written, err = io.Copy(mw, body)
	if err == nil {
		err = pw.Close()
	} else {
		_ = pw.CloseWithError(err)
	}

	if err != nil {
		<-uploadErr
		_ = wdc.Delete(ctx, destPath)
		return "", 0, fmt.Errorf("stream part body: %w", err)
	}
	if ue := <-uploadErr; ue != nil {
		_ = wdc.Delete(ctx, destPath)
		return "", 0, fmt.Errorf("upload part to webdav: %w", ue)
	}

	return hex.EncodeToString(md5h.Sum(nil)), written, nil
}

// UploadPart streams the part body directly to a fixed WebDAV path, hashes it, and records
// the part in BucketDB.
func (m *multipartService) UploadPart(ctx context.Context, bucketName, key, uploadID string, partNum int, size int64, body io.Reader) (string, error) {
	bkt, err := m.structure.GetBucket(bucketName)
	if err != nil {
		return "", fmt.Errorf("bucket %q not found: %w", bucketName, err)
	}
	if err := m.checkWriteAllowed(bkt.ID); err != nil {
		return "", err
	}

	bdb, release, err := m.cache.Get(bkt.ID)
	if err != nil {
		return "", fmt.Errorf("open bucket db: %w", err)
	}
	defer release()

	if _, err := bdb.GetMultipartUpload(uploadID); err != nil {
		return "", fmt.Errorf("%w: %s", ErrUploadNotFound, uploadID)
	}

	loc, err := m.structure.GetLocation(bkt.WebDAVLocationID)
	if err != nil {
		return "", fmt.Errorf("get location: %w", err)
	}

	dirPath := fmt.Sprintf("%s_parts/%s", loc.BaseDir, uploadID)
	if err := m.wdc.MkdirAll(ctx, dirPath); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dirPath, err)
	}

	partPath := fmt.Sprintf("%s_parts/%s/%d", loc.BaseDir, uploadID, partNum)
	md5Hex, written, err := uploadPartDirect(ctx, m.wdc, body, size, partPath)
	if err != nil {
		return "", fmt.Errorf("upload part %d: %w", partNum, err)
	}

	etag := `"` + md5Hex + `"`
	part := meta.MultipartPart{
		UploadID:   uploadID,
		PartNumber: partNum,
		HashPath:   partPath,
		SizeBytes:  written,
		ETag:       etag,
	}
	if err := bdb.AddPart(part); err != nil {
		return "", fmt.Errorf("add part record: %w", err)
	}

	if err := m.syncBucketDB(ctx, bkt.ID, loc.BaseDir, bdb); err != nil {
		if m.repair != nil {
			m.repair.MarkRepairing(bkt.ID, err.Error())
		}
		return "", fmt.Errorf("sync bucket db: %w", err)
	}

	return etag, nil
}

// multipartETag computes the S3 multipart ETag: MD5 of the concatenation of
// the raw MD5 bytes of each part, formatted as "<hex>-<N>".
func multipartETag(parts []CompletePart, partETagMap map[int]string) (string, error) {
	h := md5.New()
	for _, cp := range parts {
		raw := strings.Trim(partETagMap[cp.PartNumber], `"`)
		b, err := hex.DecodeString(raw)
		if err != nil {
			return "", fmt.Errorf("decode part %d etag: %w", cp.PartNumber, err)
		}
		h.Write(b)
	}
	return fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(h.Sum(nil)), len(parts)), nil
}

// Complete builds a chunk manifest from the uploaded parts, computes the multipart ETag,
// writes the Object to BucketDB, and leaves part files on WebDAV (referenced via obj.Chunks).
func (m *multipartService) Complete(ctx context.Context, bucketName, key, uploadID string, parts []CompletePart) (meta.Object, error) {
	bkt, err := m.structure.GetBucket(bucketName)
	if err != nil {
		return meta.Object{}, fmt.Errorf("bucket %q not found: %w", bucketName, err)
	}
	if err := m.checkWriteAllowed(bkt.ID); err != nil {
		return meta.Object{}, err
	}
	bdb, release, err := m.cache.Get(bkt.ID)
	if err != nil {
		return meta.Object{}, fmt.Errorf("open bucket db: %w", err)
	}
	defer release()

	upload, err := bdb.GetMultipartUpload(uploadID)
	if err != nil {
		return meta.Object{}, fmt.Errorf("%w: %s", ErrUploadNotFound, uploadID)
	}

	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	storedParts, err := bdb.ListParts(uploadID)
	if err != nil {
		return meta.Object{}, fmt.Errorf("list parts: %w", err)
	}

	storedMap := make(map[int]meta.MultipartPart, len(storedParts))
	for _, p := range storedParts {
		storedMap[p.PartNumber] = p
	}

	// Validate and build chunk refs.
	etagMap := make(map[int]string, len(storedParts))
	var chunks []meta.ChunkRef
	var totalSize int64
	for _, cp := range parts {
		sp, ok := storedMap[cp.PartNumber]
		if !ok {
			return meta.Object{}, fmt.Errorf("part %d: %w", cp.PartNumber, ErrPartNotFound)
		}
		if cp.ETag != sp.ETag {
			return meta.Object{}, fmt.Errorf("part %d: %w", cp.PartNumber, ErrPartETagMismatch)
		}
		etagMap[cp.PartNumber] = sp.ETag
		chunks = append(chunks, meta.ChunkRef{
			PartNumber: cp.PartNumber,
			Path:       sp.HashPath,
			Size:       sp.SizeBytes,
			MD5Hex:     strings.Trim(sp.ETag, `"`),
		})
		totalSize += sp.SizeBytes
	}

	etag, err := multipartETag(parts, etagMap)
	if err != nil {
		return meta.Object{}, fmt.Errorf("compute etag: %w", err)
	}

	contentType := upload.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	obj := meta.Object{
		ID:             newUUID(),
		Key:            key,
		HashPath:       "",
		Chunks:         chunks,
		SizeBytes:      totalSize,
		ETag:           etag,
		ContentType:    contentType,
		LastModified:   time.Now().UTC(),
		UploadComplete: true,
	}

	if err := bdb.CompleteMultipartUpload(uploadID, obj); err != nil {
		return meta.Object{}, fmt.Errorf("complete multipart record: %w", err)
	}

	// Part files stay on WebDAV — referenced via obj.Chunks.

	loc, err := m.structure.GetLocation(bkt.WebDAVLocationID)
	if err != nil {
		return meta.Object{}, fmt.Errorf("get location: %w", err)
	}

	if err := m.syncBucketDB(ctx, bkt.ID, loc.BaseDir, bdb); err != nil {
		if m.repair != nil {
			m.repair.MarkRepairing(bkt.ID, err.Error())
		}
		return meta.Object{}, fmt.Errorf("sync bucket db: %w", err)
	}

	_ = m.stats.AddDelta(bkt.WebDAVLocationID, bkt.OwnerUserID, bkt.ID, totalSize, 1)

	return obj, nil
}

// Abort deletes part files from WebDAV, removes the upload record and all part records from BucketDB.
func (m *multipartService) Abort(ctx context.Context, bucketName, uploadID string) error {
	bkt, err := m.structure.GetBucket(bucketName)
	if err != nil {
		return fmt.Errorf("bucket %q not found: %w", bucketName, err)
	}
	if err := m.checkWriteAllowed(bkt.ID); err != nil {
		return err
	}
	bdb, release, err := m.cache.Get(bkt.ID)
	if err != nil {
		return fmt.Errorf("open bucket db: %w", err)
	}
	defer release()

	if _, err := bdb.GetMultipartUpload(uploadID); err != nil {
		return fmt.Errorf("%w: %s", ErrUploadNotFound, uploadID)
	}

	// Delete part files from WebDAV before removing records.
	parts, _ := bdb.ListParts(uploadID)
	for _, p := range parts {
		_ = m.wdc.Delete(ctx, p.HashPath)
	}

	if err := bdb.AbortMultipartUpload(uploadID); err != nil {
		return fmt.Errorf("abort multipart record: %w", err)
	}

	loc, err := m.structure.GetLocation(bkt.WebDAVLocationID)
	if err != nil {
		return fmt.Errorf("get location: %w", err)
	}

	if err := m.syncBucketDB(ctx, bkt.ID, loc.BaseDir, bdb); err != nil {
		if m.repair != nil {
			m.repair.MarkRepairing(bkt.ID, err.Error())
		}
		return err
	}
	return nil
}

// ListUploads returns all in-progress multipart uploads for the given bucket.
func (m *multipartService) ListUploads(ctx context.Context, bucketName string) ([]meta.MultipartUpload, error) {
	bkt, err := m.structure.GetBucket(bucketName)
	if err != nil {
		return nil, fmt.Errorf("bucket %q not found: %w", bucketName, err)
	}
	bdb, release, err := m.cache.Get(bkt.ID)
	if err != nil {
		return nil, fmt.Errorf("open bucket db: %w", err)
	}
	defer release()
	return bdb.ListMultipartUploads()
}

// ListParts returns all uploaded parts for the given multipart upload.
func (m *multipartService) ListParts(ctx context.Context, bucketName, uploadID string) ([]meta.MultipartPart, error) {
	bkt, err := m.structure.GetBucket(bucketName)
	if err != nil {
		return nil, fmt.Errorf("bucket %q not found: %w", bucketName, err)
	}
	bdb, release, err := m.cache.Get(bkt.ID)
	if err != nil {
		return nil, fmt.Errorf("open bucket db: %w", err)
	}
	defer release()
	return bdb.ListParts(uploadID)
}

// syncBucketDB saves BucketDB to a temp file and uploads to WebDAV as {baseDir}_meta/{bucketID}.db.
func (m *multipartService) syncBucketDB(ctx context.Context, bucketID, baseDir string, bdb meta.BucketDB) error {
	tmp, err := os.CreateTemp(m.cacheDir, "bdb-sync-*.db")
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
	return m.wdc.UploadFromFile(ctx, fmt.Sprintf("%s_meta/%s.db", baseDir, bucketID), tmpPath)
}

func (m *multipartService) checkWriteAllowed(bucketID string) error {
	if m.repair == nil {
		return nil
	}
	return m.repair.CheckWrite(bucketID)
}
