package s3api

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/hpolthof/webdavs3/internal/meta"
	"github.com/hpolthof/webdavs3/internal/object"
	"github.com/hpolthof/webdavs3/internal/quota"
)

type handlers struct {
	deps S3Deps
}

func writeXML(w http.ResponseWriter, status int, v any) {
	data, err := xml.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write([]byte(xml.Header))
	w.Write(data)
}

// requireBucketOwner verifies the authenticated user owns the requested bucket.
// It writes an S3 error response and returns false if the bucket does not exist
// or is owned by another user.
func (h *handlers) requireBucketOwner(w http.ResponseWriter, r *http.Request, bucketName string) (meta.Bucket, bool) {
	reqID := requestIDFromCtx(r.Context())
	user := userFromCtx(r.Context())

	bkt, err := h.deps.Structure.GetBucket(bucketName)
	if err != nil {
		WriteS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", reqID, http.StatusNotFound)
		return meta.Bucket{}, false
	}
	if bkt.OwnerUserID != user.ID {
		WriteS3Error(w, "AccessDenied", "Access Denied.", reqID, http.StatusForbidden)
		return meta.Bucket{}, false
	}
	return bkt, true
}

// handleListBuckets handles GET /
func (h *handlers) handleListBuckets(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	reqID := requestIDFromCtx(r.Context())

	buckets, err := h.deps.Buckets.ListBuckets(r.Context(), user.ID)
	if err != nil {
		WriteS3Error(w, "InternalError", err.Error(), reqID, http.StatusInternalServerError)
		return
	}

	entries := make([]BucketEntry, 0, len(buckets))
	for _, b := range buckets {
		entries = append(entries, BucketEntry{
			Name:         b.Name,
			CreationDate: b.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	writeXML(w, http.StatusOK, ListAllMyBucketsResult{
		Owner:   Owner{ID: user.ID, DisplayName: user.DisplayName},
		Buckets: entries,
	})
}

// handleCreateBucket handles PUT /{bucket}
func (h *handlers) handleCreateBucket(w http.ResponseWriter, r *http.Request, bucketName string) {
	user := userFromCtx(r.Context())
	reqID := requestIDFromCtx(r.Context())

	// Resolve locationID: prefer x-amz-bucket-location header, then first configured location.
	locationID := r.Header.Get("x-amz-bucket-location")
	if locationID == "" {
		locs, err := h.deps.Structure.ListLocations()
		if err == nil && len(locs) > 0 {
			locationID = locs[0].ID
		}
		// If no locations are configured, proceed with empty locationID.
		// The bucket service or downstream will surface the error if it matters.
	}

	if err := h.deps.Buckets.CreateBucket(r.Context(), bucketName, user.ID, locationID); err != nil {
		code := mapBucketError(err)
		WriteS3Error(w, code, err.Error(), reqID, StatusForCode(code))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleDeleteBucket handles DELETE /{bucket}
func (h *handlers) handleDeleteBucket(w http.ResponseWriter, r *http.Request, bucketName string) {
	if _, ok := h.requireBucketOwner(w, r, bucketName); !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	if err := h.deps.Buckets.DeleteBucket(r.Context(), bucketName); err != nil {
		WriteS3Error(w, "NoSuchBucket", err.Error(), reqID, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListObjects handles GET /{bucket}
func (h *handlers) handleListObjects(w http.ResponseWriter, r *http.Request, bucketName string) {
	if _, ok := h.requireBucketOwner(w, r, bucketName); !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	token := q.Get("continuation-token")
	maxKeysStr := q.Get("max-keys")
	maxKeys := 1000
	if maxKeysStr != "" {
		if n, err := strconv.Atoi(maxKeysStr); err == nil && n > 0 {
			maxKeys = n
		}
	}

	result, err := h.deps.Objects.List(r.Context(), bucketName, prefix, delimiter, token, maxKeys)
	if err != nil {
		WriteS3Error(w, "NoSuchBucket", err.Error(), reqID, http.StatusNotFound)
		return
	}

	contents := make([]ObjectEntry, 0, len(result.Objects))
	for _, obj := range result.Objects {
		contents = append(contents, ObjectEntry{
			Key:          obj.Key,
			LastModified: obj.LastModified.UTC().Format(time.RFC3339Nano),
			ETag:         obj.ETag,
			Size:         obj.SizeBytes,
			StorageClass: "STANDARD",
		})
	}
	prefixes := make([]CommonPrefix, 0, len(result.CommonPrefixes))
	for _, p := range result.CommonPrefixes {
		prefixes = append(prefixes, CommonPrefix{Prefix: p})
	}

	writeXML(w, http.StatusOK, ListBucketResult{
		Name:                  bucketName,
		Prefix:                prefix,
		Delimiter:             delimiter,
		MaxKeys:               maxKeys,
		KeyCount:              len(contents) + len(prefixes),
		IsTruncated:           result.IsTruncated,
		NextContinuationToken: result.NextContinuationToken,
		Contents:              contents,
		CommonPrefixes:        prefixes,
	})
}

// handlePutObject handles PUT /{bucket}/{key}
func (h *handlers) handlePutObject(w http.ResponseWriter, r *http.Request, bucketName, key string) {
	bkt, ok := h.requireBucketOwner(w, r, bucketName)
	if !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}

	// Quota pre-check if Content-Length is known.
	if r.ContentLength > 0 {
		if checkErr := h.deps.Quota.Check(r.Context(), bkt.WebDAVLocationID, r.ContentLength); checkErr != nil {
			if quota.IsExceeded(checkErr) {
				WriteS3Error(w, "QuotaExceeded", "Storage quota exceeded.", reqID, StatusForCode("QuotaExceeded"))
				return
			}
		}
	}

	body := h.maybeAWSChunkedBody(r)
	size := r.ContentLength
	if r.Header.Get("Content-Encoding") == "aws-chunked" || r.Header.Get("x-amz-content-sha256") == "STREAMING-AWS4-HMAC-SHA256-PAYLOAD" {
		size = -1
	}

	obj, err := h.deps.Objects.Put(r.Context(), bucketName, key, ct, size, body)
	if err != nil {
		WriteS3Error(w, "InternalError", err.Error(), reqID, http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", obj.ETag)
	w.WriteHeader(http.StatusOK)
}

// handleGetObject handles GET /{bucket}/{key}
func (h *handlers) handleGetObject(w http.ResponseWriter, r *http.Request, bucketName, key string) {
	if _, ok := h.requireBucketOwner(w, r, bucketName); !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	obj, rc, err := h.deps.Objects.Get(r.Context(), bucketName, key)
	if err != nil {
		WriteS3Error(w, "NoSuchKey", "The specified key does not exist.", reqID, http.StatusNotFound)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", obj.SizeBytes))
	w.Header().Set("ETag", obj.ETag)
	w.Header().Set("Last-Modified", obj.LastModified.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, rc)
}

// handleHeadObject handles HEAD /{bucket}/{key}
func (h *handlers) handleHeadObject(w http.ResponseWriter, r *http.Request, bucketName, key string) {
	if _, ok := h.requireBucketOwner(w, r, bucketName); !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	obj, err := h.deps.Objects.Head(r.Context(), bucketName, key)
	if err != nil {
		WriteS3Error(w, "NoSuchKey", "The specified key does not exist.", reqID, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", obj.SizeBytes))
	w.Header().Set("ETag", obj.ETag)
	w.Header().Set("Last-Modified", obj.LastModified.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

// handleDeleteObject handles DELETE /{bucket}/{key}
func (h *handlers) handleDeleteObject(w http.ResponseWriter, r *http.Request, bucketName, key string) {
	if _, ok := h.requireBucketOwner(w, r, bucketName); !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	if err := h.deps.Objects.Delete(r.Context(), bucketName, key); err != nil {
		WriteS3Error(w, "NoSuchKey", "The specified key does not exist.", reqID, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCreateMultipartUpload handles POST /{bucket}/{key}?uploads
func (h *handlers) handleCreateMultipartUpload(w http.ResponseWriter, r *http.Request, bucketName, key string) {
	if _, ok := h.requireBucketOwner(w, r, bucketName); !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	uploadID, err := h.deps.Multipart.Create(r.Context(), bucketName, key, ct)
	if err != nil {
		WriteS3Error(w, "InternalError", err.Error(), reqID, http.StatusInternalServerError)
		return
	}
	writeXML(w, http.StatusOK, InitiateMultipartUploadResult{
		Bucket:   bucketName,
		Key:      key,
		UploadID: uploadID,
	})
}

// handleUploadPart handles PUT /{bucket}/{key}?partNumber=N&uploadId=X
func (h *handlers) handleUploadPart(w http.ResponseWriter, r *http.Request, bucketName, key string) {
	if _, ok := h.requireBucketOwner(w, r, bucketName); !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	q := r.URL.Query()
	partNumStr := q.Get("partNumber")
	uploadID := q.Get("uploadId")

	partNum, err := strconv.Atoi(partNumStr)
	if err != nil || partNum < 1 || partNum > 10000 {
		WriteS3Error(w, "InvalidArgument", "Invalid part number.", reqID, http.StatusBadRequest)
		return
	}

	body := h.maybeAWSChunkedBody(r)
	size := r.ContentLength
	if r.Header.Get("Content-Encoding") == "aws-chunked" || r.Header.Get("x-amz-content-sha256") == "STREAMING-AWS4-HMAC-SHA256-PAYLOAD" {
		size = -1
	}

	etag, err := h.deps.Multipart.UploadPart(r.Context(), bucketName, key, uploadID, partNum, size, body)
	if err != nil {
		if errors.Is(err, object.ErrUploadNotFound) {
			WriteS3Error(w, "NoSuchUpload", "The specified upload does not exist.", reqID, StatusForCode("NoSuchUpload"))
			return
		}
		WriteS3Error(w, "InternalError", err.Error(), reqID, http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// handleCompleteMultipartUpload handles POST /{bucket}/{key}?uploadId=X
func (h *handlers) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucketName, key string) {
	if _, ok := h.requireBucketOwner(w, r, bucketName); !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	uploadID := r.URL.Query().Get("uploadId")

	// Parse the CompleteMultipartUpload XML body.
	var xmlBody struct {
		Parts []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&xmlBody); err != nil {
		WriteS3Error(w, "MalformedXML", "The XML you provided was not well-formed.", reqID, http.StatusBadRequest)
		return
	}

	parts := make([]object.CompletePart, 0, len(xmlBody.Parts))
	for _, p := range xmlBody.Parts {
		parts = append(parts, object.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag})
	}

	obj, err := h.deps.Multipart.Complete(r.Context(), bucketName, key, uploadID, parts)
	if err != nil {
		if errors.Is(err, object.ErrUploadNotFound) {
			WriteS3Error(w, "NoSuchUpload", "The specified upload does not exist.", reqID, StatusForCode("NoSuchUpload"))
			return
		}
		if errors.Is(err, object.ErrPartNotFound) || errors.Is(err, object.ErrPartETagMismatch) {
			WriteS3Error(w, "InvalidPart", "One or more of the specified parts could not be found or had an invalid digest.", reqID, StatusForCode("InvalidPart"))
			return
		}
		WriteS3Error(w, "InternalError", err.Error(), reqID, http.StatusInternalServerError)
		return
	}
	writeXML(w, http.StatusOK, CompleteMultipartUploadResult{
		Location: fmt.Sprintf("/%s/%s", bucketName, key),
		Bucket:   bucketName,
		Key:      key,
		ETag:     obj.ETag,
	})
}

// handleAbortMultipartUpload handles DELETE /{bucket}/{key}?uploadId=X
func (h *handlers) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucketName, key string) {
	if _, ok := h.requireBucketOwner(w, r, bucketName); !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	uploadID := r.URL.Query().Get("uploadId")
	if err := h.deps.Multipart.Abort(r.Context(), bucketName, uploadID); err != nil {
		WriteS3Error(w, "NoSuchUpload", err.Error(), reqID, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListMultipartUploads handles GET /{bucket}?uploads
func (h *handlers) handleListMultipartUploads(w http.ResponseWriter, r *http.Request, bucketName string) {
	if _, ok := h.requireBucketOwner(w, r, bucketName); !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	uploads, err := h.deps.Multipart.ListUploads(r.Context(), bucketName)
	if err != nil {
		WriteS3Error(w, "NoSuchBucket", err.Error(), reqID, http.StatusNotFound)
		return
	}
	entries := make([]UploadEntry, 0, len(uploads))
	for _, u := range uploads {
		entries = append(entries, UploadEntry{
			UploadID:  u.ID,
			Key:       u.Key,
			Initiated: u.Initiated.UTC().Format(time.RFC3339Nano),
		})
	}
	writeXML(w, http.StatusOK, ListMultipartUploadsResult{
		Bucket:  bucketName,
		Uploads: entries,
	})
}

// handleListParts handles GET /{bucket}/{key}?uploadId=X
func (h *handlers) handleListParts(w http.ResponseWriter, r *http.Request, bucketName, key string) {
	if _, ok := h.requireBucketOwner(w, r, bucketName); !ok {
		return
	}
	reqID := requestIDFromCtx(r.Context())
	uploadID := r.URL.Query().Get("uploadId")
	parts, err := h.deps.Multipart.ListParts(r.Context(), bucketName, uploadID)
	if err != nil {
		WriteS3Error(w, "NoSuchUpload", err.Error(), reqID, http.StatusNotFound)
		return
	}
	entries := make([]PartEntry, 0, len(parts))
	for _, p := range parts {
		entries = append(entries, PartEntry{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
			Size:       p.SizeBytes,
		})
	}
	writeXML(w, http.StatusOK, ListPartsResult{
		Bucket:   bucketName,
		Key:      key,
		UploadID: uploadID,
		Parts:    entries,
	})
}

// maybeAWSChunkedBody returns a reader that decodes aws-chunked request bodies
// when the appropriate headers are present. For non-chunked requests the
// original body is returned unchanged.
func (h *handlers) maybeAWSChunkedBody(r *http.Request) io.Reader {
	isChunked := r.Header.Get("Content-Encoding") == "aws-chunked" ||
		r.Header.Get("x-amz-content-sha256") == "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	if !isChunked {
		return r.Body
	}
	if sigCtx, ok := sigCtxFromCtx(r.Context()); ok {
		return newAWSChunkedReader(r.Body, sigCtx.signingKey, sigCtx.seedSignature, sigCtx.amzDate, sigCtx.credentialScope)
	}
	return r.Body
}

// mapBucketError maps bucket service errors to S3 error codes.
func mapBucketError(err error) string {
	msg := err.Error()
	switch {
	case containsAny(msg, "already exists"):
		return "BucketAlreadyExists"
	case containsAny(msg, "invalid", "must be", "must not"):
		return "InvalidBucketName"
	case containsAny(msg, "not found"):
		return "NoSuchBucket"
	default:
		return "InternalError"
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
