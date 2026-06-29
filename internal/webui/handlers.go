package webui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/hpolthof/webdavs3/internal/meta"
	"github.com/hpolthof/webdavs3/internal/object"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) handleGetLogin(w http.ResponseWriter, r *http.Request) {
	if s.sessions.valid(r) {
		http.Redirect(w, r, "/buckets", http.StatusSeeOther)
		return
	}
	if err := s.tmpls.ExecuteTemplate(w, "login.html", map[string]any{
		"CSRFToken": s.csrf.newToken(w, r),
	}); err != nil {
		slog.Error("render login", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) handlePostLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.csrf.valid(r, r.FormValue("csrf_token")) {
		s.renderLogin(w, r, "Invalid or expired session. Please try again.")
		return
	}
	accessKey := r.FormValue("access_key")
	password := r.FormValue("password")

	user, err := s.deps.Structure.GetUserByAccessKey(accessKey)
	if err != nil || !user.Enabled || user.WebPasswordHash == "" {
		s.renderLogin(w, r, "Invalid credentials")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.WebPasswordHash), []byte(password)); err != nil {
		s.renderLogin(w, r, "Invalid credentials")
		return
	}
	s.sessions.create(w, r, accessKey)
	http.Redirect(w, r, "/buckets", http.StatusSeeOther)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, errorMsg string) {
	if err := s.tmpls.ExecuteTemplate(w, "login.html", map[string]any{
		"CSRFToken": s.csrf.newToken(w, r),
		"Error":     errorMsg,
	}); err != nil {
		slog.Error("render login", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) handlePostLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.delete(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleRootRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/buckets", http.StatusSeeOther)
}

func (s *Server) handleListBuckets(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	buckets, err := s.deps.BucketService.ListBuckets(r.Context(), user.ID)
	if err != nil {
		slog.Error("list buckets", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.tmpls.ExecuteTemplate(w, "buckets.html", map[string]any{
		"Buckets":          buckets,
		"SidebarBuckets":   buckets,
		"StorageUsedBytes": s.storageUsedBytes(r.Context(), buckets),
		"CSRFToken":        s.csrf.newToken(w, r),
		"HasLocations":     s.hasLocations(),
	}); err != nil {
		slog.Error("render buckets", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) handleCreateBucket(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.csrfOrForbidden(w, r) {
		return
	}
	user := userFromCtx(r.Context())
	name := r.FormValue("name")

	locID := ""
	if locs, err := s.deps.Structure.ListLocations(); err == nil && len(locs) > 0 {
		locID = locs[0].ID
	}
	if locID == "" {
		s.renderListBuckets(w, r, "No WebDAV location configured. Ask an administrator to add one first.")
		return
	}

	if err := s.deps.BucketService.CreateBucket(r.Context(), name, user.ID, locID); err != nil {
		s.renderListBuckets(w, r, err.Error())
		return
	}
	http.Redirect(w, r, "/buckets", http.StatusSeeOther)
}

func (s *Server) renderListBuckets(w http.ResponseWriter, r *http.Request, errorMsg string) {
	user := userFromCtx(r.Context())
	buckets, _ := s.deps.BucketService.ListBuckets(r.Context(), user.ID)
	if err := s.tmpls.ExecuteTemplate(w, "buckets.html", map[string]any{
		"Buckets":          buckets,
		"SidebarBuckets":   buckets,
		"StorageUsedBytes": s.storageUsedBytes(r.Context(), buckets),
		"CSRFToken":        s.csrf.newToken(w, r),
		"Error":            errorMsg,
		"HasLocations":     s.hasLocations(),
	}); err != nil {
		slog.Error("render buckets", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) storageUsedBytes(ctx context.Context, buckets []meta.Bucket) int64 {
	var total int64
	for _, bkt := range buckets {
		token := ""
		for {
			result, err := s.deps.ObjectService.List(ctx, bkt.Name, "", "", token, 1000)
			if err != nil {
				slog.Warn("storage total unavailable", "bucket", bkt.Name, "err", err)
				break
			}
			for _, obj := range result.Objects {
				total += obj.SizeBytes
			}
			if !result.IsTruncated || result.NextContinuationToken == "" {
				break
			}
			token = result.NextContinuationToken
		}
	}
	return total
}

func (s *Server) hasLocations() bool {
	locs, err := s.deps.Structure.ListLocations()
	return err == nil && len(locs) > 0
}

func (s *Server) handleDeleteBucket(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.csrfOrForbidden(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := s.deps.BucketService.DeleteBucket(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/buckets", http.StatusSeeOther)
}

func (s *Server) requireBucketAccess(r *http.Request) (meta.Bucket, error) {
	user := userFromCtx(r.Context())
	name := r.PathValue("name")
	bkt, err := s.deps.Structure.GetBucket(name)
	if err != nil {
		return meta.Bucket{}, fmt.Errorf("bucket not found")
	}
	if bkt.OwnerUserID != user.ID {
		return meta.Bucket{}, fmt.Errorf("forbidden")
	}
	return bkt, nil
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	bkt, err := s.requireBucketAccess(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	result, err := s.deps.ObjectService.List(r.Context(), bkt.Name, prefix, "/", "", 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	objects := hideCurrentFolderMarker(result.Objects, prefix)
	user := userFromCtx(r.Context())
	sidebarBuckets, _ := s.deps.BucketService.ListBuckets(r.Context(), user.ID)
	data := map[string]any{
		"Bucket":            bkt,
		"CurrentBucketName": bkt.Name,
		"SidebarBuckets":    sidebarBuckets,
		"StorageUsedBytes":  s.storageUsedBytes(r.Context(), sidebarBuckets),
		"Prefix":            prefix,
		"Objects":           objects,
		"CommonPrefixes":    result.CommonPrefixes,
		"CSRFToken":         s.csrf.newToken(w, r),
	}
	tmpl := "browse.html"
	if isHTMX(r) {
		w.Header().Set("X-WebUI-CSRF-Token", data["CSRFToken"].(string))
		tmpl = "browse_fragment.html"
	}
	if err := s.tmpls.ExecuteTemplate(w, tmpl, data); err != nil {
		slog.Error("render browse", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func hideCurrentFolderMarker(objects []meta.Object, prefix string) []meta.Object {
	if prefix == "" {
		return objects
	}
	filtered := objects[:0]
	for _, obj := range objects {
		if obj.Key != prefix {
			filtered = append(filtered, obj)
		}
	}
	return filtered
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	bkt, err := s.requireBucketAccess(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	obj, rc, err := s.deps.ObjectService.Get(r.Context(), bkt.Name, key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(obj.SizeBytes, 10))
	w.Header().Set("Content-Disposition", "attachment; filename=\""+path.Base(key)+"\"")
	io.Copy(w, rc)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 30); err != nil {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	if !s.csrfOrForbidden(w, r) {
		return
	}
	bkt, err := s.requireBucketAccess(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	prefix := r.FormValue("prefix")
	key := prefix + header.Filename
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if _, err := s.deps.ObjectService.Put(r.Context(), bkt.Name, key, contentType, header.Size, file); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectToBrowse(w, r, bkt.Name, prefix)
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.csrfOrForbidden(w, r) {
		return
	}
	bkt, err := s.requireBucketAccess(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	prefix := r.FormValue("prefix")
	name := r.FormValue("name")
	key := prefix + name + "/"
	if _, err := s.deps.ObjectService.Put(r.Context(), bkt.Name, key, "application/x-directory", 0, strings.NewReader("")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectToBrowse(w, r, bkt.Name, prefix)
}

func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.csrfOrForbidden(w, r) {
		return
	}
	bkt, err := s.requireBucketAccess(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	key := r.FormValue("key")
	if strings.HasSuffix(key, "/") {
		if err := s.deletePrefix(r.Context(), bkt.Name, key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		redirectToBrowse(w, r, bkt.Name, objectPrefixFromKey(key))
		return
	}
	if err := s.deps.ObjectService.Delete(r.Context(), bkt.Name, key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	prefix := objectPrefixFromKey(key)
	redirectToBrowse(w, r, bkt.Name, prefix)
}

func (s *Server) deletePrefix(ctx context.Context, bucketName, prefix string) error {
	deleted := false
	token := ""
	for {
		result, err := s.deps.ObjectService.List(ctx, bucketName, prefix, "", token, 1000)
		if err != nil {
			return err
		}
		for _, listed := range result.Objects {
			if err := s.deps.ObjectService.Delete(ctx, bucketName, listed.Key); err != nil {
				return err
			}
			deleted = true
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			break
		}
		token = result.NextContinuationToken
	}
	if !deleted {
		return object.ErrObjectNotFound
	}
	return nil
}

func (s *Server) handleRenameObject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.csrfOrForbidden(w, r) {
		return
	}
	bkt, err := s.requireBucketAccess(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	oldKey := r.FormValue("key")
	newKey := normalizeRenameKey(oldKey, r.FormValue("newKey"))

	if strings.HasSuffix(oldKey, "/") {
		newKey = strings.TrimSuffix(newKey, "/") + "/"
		if err := s.renamePrefix(r.Context(), bkt.Name, oldKey, newKey); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		redirectToBrowse(w, r, bkt.Name, objectPrefixFromKey(newKey))
		return
	}

	obj, rc, err := s.deps.ObjectService.Get(r.Context(), bkt.Name, oldKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer rc.Close()

	if _, err := s.deps.ObjectService.Put(r.Context(), bkt.Name, newKey, obj.ContentType, obj.SizeBytes, rc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.deps.ObjectService.Delete(r.Context(), bkt.Name, oldKey); err != nil {
		slog.Error("rename cleanup failed", "bucket", bkt.Name, "key", oldKey, "err", err)
	}
	prefix := objectPrefixFromKey(newKey)
	redirectToBrowse(w, r, bkt.Name, prefix)
}

func normalizeRenameKey(oldKey, newKey string) string {
	newKey = strings.TrimSpace(strings.TrimPrefix(newKey, "/"))
	if newKey == "" {
		return oldKey
	}
	if strings.Contains(newKey, "/") {
		return newKey
	}
	return objectPrefixFromKey(oldKey) + newKey
}

func (s *Server) renamePrefix(ctx context.Context, bucketName, oldPrefix, newPrefix string) error {
	if oldPrefix == newPrefix {
		return nil
	}
	moved := false
	token := ""
	for {
		result, err := s.deps.ObjectService.List(ctx, bucketName, oldPrefix, "", token, 1000)
		if err != nil {
			return err
		}
		for _, listed := range result.Objects {
			newKey := newPrefix + strings.TrimPrefix(listed.Key, oldPrefix)
			if err := s.copyObject(ctx, bucketName, listed.Key, newKey); err != nil {
				return err
			}
			if err := s.deps.ObjectService.Delete(ctx, bucketName, listed.Key); err != nil {
				slog.Error("rename cleanup failed", "bucket", bucketName, "key", listed.Key, "err", err)
			}
			moved = true
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			break
		}
		token = result.NextContinuationToken
	}
	if !moved {
		return object.ErrObjectNotFound
	}
	return nil
}

func (s *Server) copyObject(ctx context.Context, bucketName, oldKey, newKey string) error {
	obj, rc, err := s.deps.ObjectService.Get(ctx, bucketName, oldKey)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = s.deps.ObjectService.Put(ctx, bucketName, newKey, obj.ContentType, obj.SizeBytes, rc)
	return err
}

func objectPrefixFromKey(key string) string {
	if strings.HasSuffix(key, "/") {
		key = strings.TrimSuffix(key, "/")
	}
	dir := path.Dir(key)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir + "/"
}

func redirectToBrowse(w http.ResponseWriter, r *http.Request, bucketName, prefix string) {
	u := "/buckets/" + bucketName + "/browse"
	if prefix != "" {
		u += "?prefix=" + url.QueryEscape(prefix)
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}
