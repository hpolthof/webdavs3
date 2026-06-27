package s3api

import (
	"net/http"
	"strings"

	"github.com/hpolthof/webdavs3/internal/auth"
	"github.com/hpolthof/webdavs3/internal/bucket"
	"github.com/hpolthof/webdavs3/internal/meta"
	"github.com/hpolthof/webdavs3/internal/object"
	"github.com/hpolthof/webdavs3/internal/quota"
)

// S3Deps groups all dependencies required by the S3 API layer.
type S3Deps struct {
	Auth      auth.Verifier
	Structure meta.StructureDB
	Buckets   bucket.Service
	Objects   object.ObjectService
	Multipart object.MultipartService
	Quota     quota.Service
	Region    string
	// DecryptFn decrypts an AES-256-GCM encrypted ciphertext (as produced by
	// adminui.EncryptPassword) and returns the plaintext secret key.
	// Required for SigV4 authentication to work correctly.
	DecryptFn func(ciphertext string) (string, error)
}

// NewRouter returns an http.Handler implementing the S3 API (path-style).
func NewRouter(deps S3Deps) http.Handler {
	mux := http.NewServeMux()
	h := &handlers{deps: deps}

	// Route all requests through a single catch-all and dispatch internally.
	// We use a single handler to inspect method + query params together.
	mux.Handle("/", chain(
		http.HandlerFunc(h.dispatch),
		requestIDMiddleware,
		authMiddleware(deps),
	))

	return mux
}

// dispatch routes requests based on method, path segments, and query parameters.
func (h *handlers) dispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// Strip leading slash and split into at most bucket + key
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	bucketName := parts[0]
	key := ""
	if len(parts) == 2 {
		key = parts[1]
	}

	q := r.URL.Query()

	switch {
	// Service-level: GET / → ListBuckets
	case r.Method == http.MethodGet && bucketName == "":
		h.handleListBuckets(w, r)

	// Bucket-level
	case r.Method == http.MethodPut && key == "" && !q.Has("partNumber"):
		h.handleCreateBucket(w, r, bucketName)
	case r.Method == http.MethodDelete && key == "" && !q.Has("uploadId"):
		h.handleDeleteBucket(w, r, bucketName)
	case r.Method == http.MethodGet && key == "" && q.Has("uploads"):
		h.handleListMultipartUploads(w, r, bucketName)
	case r.Method == http.MethodGet && key == "":
		h.handleListObjects(w, r, bucketName)

	// Multipart upload operations (must precede generic PUT/DELETE on key)
	case r.Method == http.MethodPost && key != "" && q.Has("uploads"):
		h.handleCreateMultipartUpload(w, r, bucketName, key)
	case r.Method == http.MethodPut && key != "" && q.Has("partNumber"):
		h.handleUploadPart(w, r, bucketName, key)
	case r.Method == http.MethodPost && key != "" && q.Has("uploadId"):
		h.handleCompleteMultipartUpload(w, r, bucketName, key)
	case r.Method == http.MethodDelete && key != "" && q.Has("uploadId"):
		h.handleAbortMultipartUpload(w, r, bucketName, key)
	case r.Method == http.MethodGet && key != "" && q.Has("uploadId"):
		h.handleListParts(w, r, bucketName, key)

	// Object operations
	case r.Method == http.MethodPut && key != "":
		h.handlePutObject(w, r, bucketName, key)
	case r.Method == http.MethodGet && key != "":
		h.handleGetObject(w, r, bucketName, key)
	case r.Method == http.MethodDelete && key != "":
		h.handleDeleteObject(w, r, bucketName, key)
	case r.Method == http.MethodHead && key != "":
		h.handleHeadObject(w, r, bucketName, key)

	default:
		reqID := requestIDFromCtx(r.Context())
		WriteS3Error(w, "MethodNotAllowed", "The specified method is not allowed against this resource.", reqID, http.StatusMethodNotAllowed)
	}
}
