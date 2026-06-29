package adminui

import (
	"context"
	"crypto/rand"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hpolthof/webdavs3/internal/meta"
	wdv "github.com/hpolthof/webdavs3/internal/webdav"
	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// AdminDeps holds dependencies for the admin UI.
type AdminDeps struct {
	AdminPasswordHash string
	AdminUsername     string
	EncryptionKey     string // base64-encoded 32-byte AES-256 key; empty disables encryption
	Structure         meta.StructureDB
	Stats             meta.StatsDB
	LocalCacheDir     string
	SyncEngine        syncEngine
	BucketService     bucketService
	RefreshWebDAV     func()
	FlushStructure    func()
}

type syncEngine interface {
	SyncFromWebDAV(ctx context.Context, locationID string) error
}

type bucketService interface {
	CreateBucket(ctx context.Context, name, ownerUserID, locationID string) error
	DeleteBucket(ctx context.Context, name string) error
}

type webPasswordAtomicUpdater interface {
	SetUserWebPasswordAndEnc(id, hash, enc string) error
}

// AdminServer is the HTTP handler for the admin UI.
type AdminServer struct {
	deps     AdminDeps
	sessions *sessionStore
	mux      *http.ServeMux
	tmpls    *template.Template
}

var templateFuncs = template.FuncMap{
	"humanBytes": func(n int64) string {
		if n == 0 {
			return "0 B"
		}
		units := []string{"B", "KB", "MB", "GB", "TB"}
		f := float64(n)
		i := 0
		for f >= 1024 && i < len(units)-1 {
			f /= 1024
			i++
		}
		return fmt.Sprintf("%.2f %s", f, units[i])
	},
	"quotaPercent": func(used, quota int64) float64 {
		if quota <= 0 {
			return 0
		}
		return float64(used) / float64(quota) * 100
	},
	"div": func(a, b int64) int64 {
		if b == 0 {
			return 0
		}
		return a / b
	},
}

// NewAdminServer creates and wires up an AdminServer.
func NewAdminServer(deps AdminDeps) *AdminServer {
	s := &AdminServer{
		deps:     deps,
		sessions: newSessionStore(),
		mux:      http.NewServeMux(),
	}

	s.tmpls = template.Must(template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.html"))

	// Static assets (no session required).
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(fmt.Sprintf("load static assets: %v", err))
	}
	s.mux.Handle("/admin/static/", http.StripPrefix("/admin/static/", http.FileServerFS(staticSub)))

	s.mux.HandleFunc("GET /admin/login", s.handleGetLogin)
	s.mux.HandleFunc("POST /admin/login", s.handlePostLogin)
	s.mux.HandleFunc("POST /admin/logout", s.handlePostLogout)

	// Location routes
	s.mux.Handle("GET /admin/locations", s.requireSession(http.HandlerFunc(s.handleListLocations)))
	s.mux.Handle("GET /admin/locations/new", s.requireSession(http.HandlerFunc(s.handleGetNewLocation)))
	s.mux.Handle("POST /admin/locations/new", s.requireSession(http.HandlerFunc(s.handlePostNewLocation)))
	s.mux.Handle("POST /admin/locations/test", s.requireSession(http.HandlerFunc(s.handleTestLocation)))
	s.mux.Handle("GET /admin/locations/{id}", s.requireSession(http.HandlerFunc(s.handleLocationDetail)))
	s.mux.Handle("GET /admin/locations/{id}/edit", s.requireSession(http.HandlerFunc(s.handleEditLocation)))
	s.mux.Handle("POST /admin/locations/{id}/edit", s.requireSession(http.HandlerFunc(s.handlePostEditLocation)))
	s.mux.Handle("POST /admin/locations/{id}/delete", s.requireSession(http.HandlerFunc(s.handleDeleteLocation)))
	s.mux.Handle("POST /admin/locations/{id}/sync", s.requireSession(http.HandlerFunc(s.handleLocationSync)))

	// User routes
	s.mux.Handle("GET /admin/users", s.requireSession(http.HandlerFunc(s.handleListUsers)))
	s.mux.Handle("GET /admin/users/new", s.requireSession(http.HandlerFunc(s.handleGetNewUser)))
	s.mux.Handle("POST /admin/users/new", s.requireSession(http.HandlerFunc(s.handlePostNewUser)))
	s.mux.Handle("GET /admin/users/{id}", s.requireSession(http.HandlerFunc(s.handleUserDetail)))
	s.mux.Handle("GET /admin/users/{id}/edit", s.requireSession(http.HandlerFunc(s.handleEditUser)))
	s.mux.Handle("POST /admin/users/{id}/edit", s.requireSession(http.HandlerFunc(s.handlePostEditUser)))
	s.mux.Handle("POST /admin/users/{id}/delete", s.requireSession(http.HandlerFunc(s.handleDeleteUser)))
	s.mux.Handle("POST /admin/users/{id}/toggle", s.requireSession(http.HandlerFunc(s.handleUserToggle)))
	s.mux.Handle("POST /admin/users/{id}/regenerate", s.requireSession(http.HandlerFunc(s.handleRegenerateSecret)))
	s.mux.Handle("POST /admin/users/{id}/reset-password", s.requireSession(http.HandlerFunc(s.handleResetWebPassword)))

	// Bucket routes
	s.mux.Handle("GET /admin/buckets", s.requireSession(http.HandlerFunc(s.handleListBuckets)))
	s.mux.Handle("GET /admin/buckets/new", s.requireSession(http.HandlerFunc(s.handleGetNewBucket)))
	s.mux.Handle("POST /admin/buckets/new", s.requireSession(http.HandlerFunc(s.handlePostNewBucket)))
	s.mux.Handle("GET /admin/buckets/{name}", s.requireSession(http.HandlerFunc(s.handleBucketDetail)))
	s.mux.Handle("POST /admin/buckets/{name}/delete", s.requireSession(http.HandlerFunc(s.handleDeleteBucket)))

	// Dashboard catch-all (must be last)
	s.mux.Handle("/admin/", s.requireSession(http.HandlerFunc(s.handleDashboard)))

	return s
}

func (s *AdminServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// requireSession is a middleware that redirects unauthenticated requests to /admin/login.
func (s *AdminServer) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.sessions.valid(r) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *AdminServer) handleGetLogin(w http.ResponseWriter, r *http.Request) {
	if err := s.tmpls.ExecuteTemplate(w, "login.html", nil); err != nil {
		slog.Error("render login", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *AdminServer) handlePostLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")

	if username != s.deps.AdminUsername {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(s.deps.AdminPasswordHash), []byte(password)); err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	s.sessions.create(w)
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (s *AdminServer) handlePostLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.delete(w, r)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (s *AdminServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	locs, _ := s.deps.Structure.ListLocations()
	users, _ := s.deps.Structure.ListUsers()
	buckets, _ := s.deps.Structure.ListBuckets()

	type locStat struct {
		Location    meta.Location
		UsageBytes  int64
		ActualBytes int64
		SavedBytes  int64
	}
	var stats []locStat
	for _, loc := range locs {
		var usage int64
		if s.deps.Stats != nil {
			usage, _ = s.deps.Stats.GetTotalUsage(loc.ID)
		}
		actual := s.actualUsageBytes(loc.ID, buckets)
		saved := usage - actual
		if saved < 0 {
			saved = 0
		}
		stats = append(stats, locStat{
			Location:    loc,
			UsageBytes:  usage,
			ActualBytes: actual,
			SavedBytes:  saved,
		})
	}

	if err := s.tmpls.ExecuteTemplate(w, "dashboard.html", map[string]any{
		"LocationStats": stats,
		"UserCount":     len(users),
		"BucketCount":   len(buckets),
		"LocationCount": len(locs),
	}); err != nil {
		slog.Error("render dashboard", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *AdminServer) actualUsageBytes(locationID string, buckets []meta.Bucket) int64 {
	if s.deps.LocalCacheDir == "" {
		return 0
	}

	uniqueData := map[string]int64{}
	for _, b := range buckets {
		if b.WebDAVLocationID != locationID {
			continue
		}
		dbPath := filepath.Join(s.deps.LocalCacheDir, "bucket-"+b.ID+".db")
		if _, err := os.Stat(dbPath); err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("stat bucket metadata for physical usage failed", "bucket", b.ID, "err", err)
			}
			continue
		}
		bdb, err := meta.OpenBucketDB(dbPath)
		if err != nil {
			slog.Warn("open bucket metadata for physical usage failed", "bucket", b.ID, "err", err)
			continue
		}
		objects, _, err := bdb.ListObjects("", "", "", 0)
		if err != nil {
			if closeErr := bdb.Close(); closeErr != nil {
				slog.Warn("close bucket metadata after physical usage failed", "bucket", b.ID, "err", closeErr)
			}
			slog.Warn("list bucket objects for physical usage failed", "bucket", b.ID, "err", err)
			continue
		}
		for _, obj := range objects {
			fullObj, err := bdb.GetObject(obj.Key)
			if err == nil {
				obj = fullObj
			} else {
				slog.Warn("load object chunks for physical usage failed", "bucket", b.ID, "key", obj.Key, "err", err)
			}
			if len(obj.Chunks) == 0 {
				uniqueData[obj.HashPath] = obj.SizeBytes
				continue
			}
			for _, chunk := range obj.Chunks {
				uniqueData[chunk.Path] = chunk.Size
			}
		}
		if closeErr := bdb.Close(); closeErr != nil {
			slog.Warn("close bucket metadata after physical usage failed", "bucket", b.ID, "err", closeErr)
		}
	}

	var total int64
	for _, size := range uniqueData {
		total += size
	}
	return total
}

// --- Location handlers ---

func (s *AdminServer) handleListLocations(w http.ResponseWriter, r *http.Request) {
	locs, _ := s.deps.Structure.ListLocations()
	if err := s.tmpls.ExecuteTemplate(w, "locations.html", map[string]any{"Locations": locs}); err != nil {
		slog.Error("render locations", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleGetNewLocation(w http.ResponseWriter, r *http.Request) {
	if err := s.tmpls.ExecuteTemplate(w, "location_new.html", nil); err != nil {
		slog.Error("render new location", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleTestLocation(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	url := r.FormValue("url")
	username := r.FormValue("username")
	password := r.FormValue("password")
	locationID := r.FormValue("location_id")

	// When editing an existing location, an empty password means "keep current".
	// In that case decrypt the stored password if a location ID is provided.
	if password == "" && locationID != "" && s.deps.EncryptionKey != "" {
		loc, err := s.deps.Structure.GetLocation(locationID)
		if err == nil && loc.PasswordEnc != "" {
			decrypted, decErr := DecryptPassword(loc.PasswordEnc, s.deps.EncryptionKey)
			if decErr == nil {
				password = decrypted
			}
		}
	}

	if url == "" {
		http.Error(w, "WebDAV URL is required", http.StatusBadRequest)
		return
	}

	wdc := wdv.New(url, username, password)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := wdc.Ping(ctx); err != nil {
		slog.Warn("location connection test failed", "url", url, "username", username, "err", err)
		fmt.Fprintf(w, `<div class="flash flash-error">Connection failed: %v</div>`, template.HTMLEscapeString(err.Error()))
		return
	}

	slog.Info("location connection test succeeded", "url", url, "username", username)
	fmt.Fprint(w, `<div class="flash flash-success">Connection OK</div>`)
}

func (s *AdminServer) handlePostNewLocation(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	quotaGBStr := r.FormValue("quota_gb")
	quotaGB, _ := strconv.ParseInt(quotaGBStr, 10, 64)

	passEnc, encryptErr := encodePassword(r.FormValue("password"), s.deps.EncryptionKey)
	if encryptErr != nil {
		http.Error(w, "password encryption failed", http.StatusInternalServerError)
		return
	}

	loc := meta.Location{
		ID:          newAdminUUID(),
		URL:         r.FormValue("url"),
		Username:    r.FormValue("username"),
		PasswordEnc: passEnc,
		DisplayName: r.FormValue("display_name"),
		QuotaBytes:  quotaGB * (1 << 30),
		BaseDir:     meta.NormalizeBaseDir(r.FormValue("base_dir")),
		CreatedAt:   time.Now(),
	}
	if err := s.deps.Structure.AddLocation(loc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.deps.RefreshWebDAV != nil {
		s.deps.RefreshWebDAV()
	}
	if s.deps.SyncEngine != nil {
		if err := s.deps.SyncEngine.SyncFromWebDAV(r.Context(), loc.ID); err != nil {
			slog.Error("sync new location failed", "location", loc.ID, "err", err)
			http.Error(w, "sync failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/admin/locations", http.StatusSeeOther)
}

func (s *AdminServer) handleLocationDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	loc, err := s.deps.Structure.GetLocation(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var usageBytes int64
	if s.deps.Stats != nil {
		usageBytes, _ = s.deps.Stats.GetTotalUsage(id)
	}
	if err := s.tmpls.ExecuteTemplate(w, "location_detail.html", map[string]any{
		"Location":   loc,
		"UsageBytes": usageBytes,
	}); err != nil {
		slog.Error("render location detail", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleEditLocation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	loc, err := s.deps.Structure.GetLocation(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.tmpls.ExecuteTemplate(w, "location_edit.html", loc); err != nil {
		slog.Error("render edit location", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handlePostEditLocation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	loc, err := s.deps.Structure.GetLocation(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	quotaGB, _ := strconv.ParseInt(r.FormValue("quota_gb"), 10, 64)
	password := r.FormValue("password")
	passwordEnc := loc.PasswordEnc
	if password != "" {
		var encErr error
		passwordEnc, encErr = encodePassword(password, s.deps.EncryptionKey)
		if encErr != nil {
			http.Error(w, "password encryption failed", http.StatusInternalServerError)
			return
		}
	}

	loc.URL = r.FormValue("url")
	loc.Username = r.FormValue("username")
	loc.PasswordEnc = passwordEnc
	loc.DisplayName = r.FormValue("display_name")
	loc.QuotaBytes = quotaGB * (1 << 30)
	loc.BaseDir = meta.NormalizeBaseDir(r.FormValue("base_dir"))

	if err := s.deps.Structure.UpdateLocation(loc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.deps.RefreshWebDAV != nil {
		s.deps.RefreshWebDAV()
	}
	http.Redirect(w, r, "/admin/locations/"+id, http.StatusSeeOther)
}

func (s *AdminServer) handleDeleteLocation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.deps.Structure.DeleteLocation(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.deps.RefreshWebDAV != nil {
		s.deps.RefreshWebDAV()
	}
	http.Redirect(w, r, "/admin/locations", http.StatusSeeOther)
}

func (s *AdminServer) handleLocationSync(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.deps.SyncEngine == nil {
		http.Error(w, "sync engine not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.deps.SyncEngine.SyncFromWebDAV(r.Context(), id); err != nil {
		slog.Error("manual sync failed", "location", id, "err", err)
		http.Error(w, "sync failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Sync complete")
}

// --- User handlers ---

func (s *AdminServer) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, _ := s.deps.Structure.ListUsers()
	locs, _ := s.deps.Structure.ListLocations()
	if err := s.tmpls.ExecuteTemplate(w, "users.html", map[string]any{
		"Users":        users,
		"HasLocations": len(locs) > 0,
	}); err != nil {
		slog.Error("render users", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleGetNewUser(w http.ResponseWriter, r *http.Request) {
	locs, _ := s.deps.Structure.ListLocations()
	if err := s.tmpls.ExecuteTemplate(w, "user_new.html", map[string]any{
		"HasLocations": len(locs) > 0,
	}); err != nil {
		slog.Error("render new user", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handlePostNewUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	locs, _ := s.deps.Structure.ListLocations()
	if len(locs) == 0 {
		if err := s.tmpls.ExecuteTemplate(w, "user_new.html", map[string]any{
			"HasLocations": false,
			"Error":        "Add a WebDAV location before creating users.",
		}); err != nil {
			slog.Error("render new user", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	displayName := r.FormValue("display_name")

	accessKey := randomAlphanumeric(20)
	secretKey := randomAlphanumeric(40)
	webPassword := randomAlphanumeric(20)

	hashBytes, err := bcrypt.GenerateFromPassword([]byte(secretKey), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	webHashBytes, err := bcrypt.GenerateFromPassword([]byte(webPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "password generation failed", http.StatusInternalServerError)
		return
	}

	secretKeyEnc, err := encodePassword(secretKey, s.deps.EncryptionKey)
	if err != nil {
		http.Error(w, "secret key encryption failed", http.StatusInternalServerError)
		return
	}
	webPasswordEnc, err := encodePassword(webPassword, s.deps.EncryptionKey)
	if err != nil {
		http.Error(w, "password encryption failed", http.StatusInternalServerError)
		return
	}

	user := meta.User{
		ID:              newAdminUUID(),
		AccessKey:       accessKey,
		SecretKeyHash:   string(hashBytes),
		SecretKeyEnc:    secretKeyEnc,
		WebPasswordHash: string(webHashBytes),
		WebPasswordEnc:  webPasswordEnc,
		DisplayName:     displayName,
		Enabled:         true,
		CreatedAt:       time.Now(),
	}
	if err := s.deps.Structure.AddUser(user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.deps.FlushStructure != nil {
		s.deps.FlushStructure()
	}

	if err := s.tmpls.ExecuteTemplate(w, "user_created.html", map[string]any{
		"User":        user,
		"SecretKey":   secretKey,
		"AccessKey":   accessKey,
		"WebPassword": webPassword,
		"Title":       "User Created",
		"Flash":       "Save these credentials - the secret key and web password are shown only once.",
	}); err != nil {
		slog.Error("render user created", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleUserDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user, err := s.deps.Structure.GetUser(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	buckets, _ := s.deps.Structure.ListBucketsByUser(id)
	if err := s.tmpls.ExecuteTemplate(w, "user_detail.html", map[string]any{
		"User":    user,
		"Buckets": buckets,
	}); err != nil {
		slog.Error("render user detail", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleEditUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user, err := s.deps.Structure.GetUser(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.tmpls.ExecuteTemplate(w, "user_edit.html", user); err != nil {
		slog.Error("render edit user", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handlePostEditUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user, err := s.deps.Structure.GetUser(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	user.DisplayName = r.FormValue("display_name")
	if err := s.deps.Structure.UpdateUser(user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.deps.FlushStructure != nil {
		s.deps.FlushStructure()
	}
	http.Redirect(w, r, "/admin/users/"+id, http.StatusSeeOther)
}

func (s *AdminServer) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.deps.Structure.DeleteUser(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.deps.FlushStructure != nil {
		s.deps.FlushStructure()
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (s *AdminServer) handleUserToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user, err := s.deps.Structure.GetUser(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.deps.Structure.SetUserEnabled(id, !user.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/users/"+id, http.StatusSeeOther)
}

func (s *AdminServer) handleRegenerateSecret(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user, err := s.deps.Structure.GetUser(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	secretKey := randomAlphanumeric(40)
	hashBytes, err := bcrypt.GenerateFromPassword([]byte(secretKey), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	secretKeyEnc, err := encodePassword(secretKey, s.deps.EncryptionKey)
	if err != nil {
		http.Error(w, "secret key encryption failed", http.StatusInternalServerError)
		return
	}

	user.SecretKeyHash = string(hashBytes)
	user.SecretKeyEnc = secretKeyEnc
	if err := s.deps.Structure.UpdateUser(user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.tmpls.ExecuteTemplate(w, "user_created.html", map[string]any{
		"User":      user,
		"AccessKey": user.AccessKey,
		"SecretKey": secretKey,
		"Title":     "Secret Key Regenerated",
	}); err != nil {
		slog.Error("render regenerated secret", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleResetWebPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user, err := s.deps.Structure.GetUser(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	webPassword := randomAlphanumeric(20)
	hashBytes, err := bcrypt.GenerateFromPassword([]byte(webPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "password generation failed", http.StatusInternalServerError)
		return
	}
	webPasswordEnc, err := encodePassword(webPassword, s.deps.EncryptionKey)
	if err != nil {
		http.Error(w, "password encryption failed", http.StatusInternalServerError)
		return
	}
	user.WebPasswordHash = string(hashBytes)
	user.WebPasswordEnc = webPasswordEnc
	if updater, ok := s.deps.Structure.(webPasswordAtomicUpdater); ok {
		if err := updater.SetUserWebPasswordAndEnc(id, user.WebPasswordHash, user.WebPasswordEnc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		if err := s.deps.Structure.SetUserWebPassword(id, user.WebPasswordHash); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if s.deps.FlushStructure != nil {
		s.deps.FlushStructure()
	}

	if err := s.tmpls.ExecuteTemplate(w, "user_created.html", map[string]any{
		"User":        user,
		"AccessKey":   user.AccessKey,
		"WebPassword": webPassword,
		"Title":       "Web Password Reset",
		"Flash":       "Save this web password - it is shown only once.",
	}); err != nil {
		slog.Error("render reset web password", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// --- Bucket handlers ---

func (s *AdminServer) handleListBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, _ := s.deps.Structure.ListBuckets()
	users, _ := s.deps.Structure.ListUsers()
	locs, _ := s.deps.Structure.ListLocations()
	userNames := map[string]string{}
	for _, u := range users {
		userNames[u.ID] = u.DisplayName
	}
	if err := s.tmpls.ExecuteTemplate(w, "buckets.html", map[string]any{
		"Buckets":      buckets,
		"UserNames":    userNames,
		"HasLocations": len(locs) > 0,
	}); err != nil {
		slog.Error("render buckets", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleGetNewBucket(w http.ResponseWriter, r *http.Request) {
	users, _ := s.deps.Structure.ListUsers()
	locs, _ := s.deps.Structure.ListLocations()
	if err := s.tmpls.ExecuteTemplate(w, "bucket_new.html", map[string]any{
		"Users":     users,
		"Locations": locs,
	}); err != nil {
		slog.Error("render new bucket", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handlePostNewBucket(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	ownerID := r.FormValue("owner_user_id")
	locID := r.FormValue("location_id")
	if locID == "" {
		if locs, err := s.deps.Structure.ListLocations(); err == nil && len(locs) > 0 {
			locID = locs[0].ID
		}
	}
	if locID == "" {
		http.Error(w, "no webdav location configured", http.StatusBadRequest)
		return
	}
	if err := s.deps.BucketService.CreateBucket(r.Context(), name, ownerID, locID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/buckets", http.StatusSeeOther)
}

func (s *AdminServer) handleBucketDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	b, err := s.deps.Structure.GetBucket(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.tmpls.ExecuteTemplate(w, "bucket_detail.html", b); err != nil {
		slog.Error("render bucket detail", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleDeleteBucket(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.deps.BucketService.DeleteBucket(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/buckets", http.StatusSeeOther)
}

// --- Helpers ---

// randomAlphanumeric returns a cryptographically random alphanumeric string of length n.
func randomAlphanumeric(n int) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, n)
	for i := range result {
		idx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		result[i] = chars[idx.Int64()]
	}
	return string(result)
}

// newAdminUUID generates a version-4 UUID.
func newAdminUUID() string {
	var buf [16]byte
	rand.Read(buf[:])
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}
