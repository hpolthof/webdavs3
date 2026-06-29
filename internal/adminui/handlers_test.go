package adminui_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hpolthof/webdavs3/internal/adminui"
	"github.com/hpolthof/webdavs3/internal/meta"
	wdv "github.com/hpolthof/webdavs3/internal/webdav"
	"golang.org/x/crypto/bcrypt"
)

// stubStructureDB for admin tests.
type stubAdminStructureDB struct {
	locations []meta.Location
	users     []meta.User
	buckets   []meta.Bucket
}

func (s *stubAdminStructureDB) AddLocation(l meta.Location) error {
	s.locations = append(s.locations, l)
	return nil
}
func (s *stubAdminStructureDB) GetLocation(id string) (meta.Location, error) {
	for _, l := range s.locations {
		if l.ID == id {
			return l, nil
		}
	}
	return meta.Location{}, fmt.Errorf("not found")
}
func (s *stubAdminStructureDB) ListLocations() ([]meta.Location, error) { return s.locations, nil }
func (s *stubAdminStructureDB) UpdateLocation(meta.Location) error      { return nil }
func (s *stubAdminStructureDB) DeleteLocation(string) error             { return nil }
func (s *stubAdminStructureDB) AddUser(u meta.User) error {
	s.users = append(s.users, u)
	return nil
}
func (s *stubAdminStructureDB) GetUserByAccessKey(key string) (meta.User, error) {
	for _, u := range s.users {
		if u.AccessKey == key {
			return u, nil
		}
	}
	return meta.User{}, fmt.Errorf("not found")
}
func (s *stubAdminStructureDB) GetUser(id string) (meta.User, error) {
	for _, u := range s.users {
		if u.ID == id {
			return u, nil
		}
	}
	return meta.User{}, fmt.Errorf("not found")
}
func (s *stubAdminStructureDB) ListUsers() ([]meta.User, error) { return s.users, nil }
func (s *stubAdminStructureDB) UpdateUser(meta.User) error      { return nil }
func (s *stubAdminStructureDB) DeleteUser(string) error         { return nil }
func (s *stubAdminStructureDB) SetUserEnabled(id string, enabled bool) error {
	for i, u := range s.users {
		if u.ID == id {
			s.users[i].Enabled = enabled
			return nil
		}
	}
	return fmt.Errorf("not found")
}
func (s *stubAdminStructureDB) SetUserWebPassword(id string, hash string) error {
	for i, u := range s.users {
		if u.ID == id {
			s.users[i].WebPasswordHash = hash
			return nil
		}
	}
	return fmt.Errorf("not found")
}
func (s *stubAdminStructureDB) AddBucket(b meta.Bucket) error {
	s.buckets = append(s.buckets, b)
	return nil
}
func (s *stubAdminStructureDB) UpdateBucket(b meta.Bucket) error {
	for i, existing := range s.buckets {
		if existing.Name == b.Name {
			s.buckets[i] = b
			return nil
		}
	}
	return fmt.Errorf("not found")
}
func (s *stubAdminStructureDB) GetBucket(name string) (meta.Bucket, error) {
	for _, b := range s.buckets {
		if b.Name == name {
			return b, nil
		}
	}
	return meta.Bucket{}, fmt.Errorf("not found")
}
func (s *stubAdminStructureDB) ListBuckets() ([]meta.Bucket, error) { return s.buckets, nil }
func (s *stubAdminStructureDB) ListBucketsByUser(uid string) ([]meta.Bucket, error) {
	var result []meta.Bucket
	for _, b := range s.buckets {
		if b.OwnerUserID == uid {
			result = append(result, b)
		}
	}
	return result, nil
}
func (s *stubAdminStructureDB) DeleteBucket(name string) error { return nil }
func (s *stubAdminStructureDB) SaveToFile(string) error        { return nil }
func (s *stubAdminStructureDB) LoadFromFile(string) error      { return nil }
func (s *stubAdminStructureDB) Close() error                   { return nil }

// stubAdminStatsDB
type stubAdminStatsDB struct {
	totalUsage int64
}

func (s *stubAdminStatsDB) AddDelta(_, _, _ string, _, _ int64) error { return nil }
func (s *stubAdminStatsDB) GetTotalUsage(string) (int64, error) {
	if s.totalUsage == 0 {
		return 1024, nil
	}
	return s.totalUsage, nil
}
func (s *stubAdminStatsDB) MergeFromFile(string, string, map[string]string) error {
	return nil
}
func (s *stubAdminStatsDB) Flush(_ context.Context, _ wdv.Client, _ string) error {
	return nil
}
func (s *stubAdminStatsDB) Close() error { return nil }

type stubAdminSyncEngine struct {
	locationIDs []string
	err         error
}

func (s *stubAdminSyncEngine) SyncFromWebDAV(_ context.Context, locationID string) error {
	s.locationIDs = append(s.locationIDs, locationID)
	return s.err
}

func buildAuthedAdminServer(t *testing.T) (*adminui.AdminServer, *http.Cookie) {
	t.Helper()
	srv, cookie, _ := buildAuthedAdminServerWithDB(t)
	return srv, cookie
}

func buildAuthedAdminServerWithDB(t *testing.T) (*adminui.AdminServer, *http.Cookie, *stubAdminStructureDB) {
	t.Helper()
	srv, cookie, structDB, _ := buildAuthedAdminServerWithDBAndSync(t)
	return srv, cookie, structDB
}

func buildAuthedAdminServerWithDBAndSync(t *testing.T) (*adminui.AdminServer, *http.Cookie, *stubAdminStructureDB, *stubAdminSyncEngine) {
	t.Helper()
	structDB := &stubAdminStructureDB{
		locations: []meta.Location{{ID: "loc-1", URL: "http://dav", DisplayName: "Local", QuotaBytes: 1 << 30, BaseDir: "/", CreatedAt: time.Now()}},
		users:     []meta.User{{ID: "u-1", AccessKey: "AK123", DisplayName: "Alice", Enabled: true, CreatedAt: time.Now()}},
	}
	syncEngine := &stubAdminSyncEngine{}

	// testEncKey is a base64-encoded 32-byte AES-256 key used only in tests.
	const testEncKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	srv := adminui.NewAdminServer(adminui.AdminDeps{
		AdminPasswordHash: "$2a$10$UVdKbqzndmqLzRIJu2wrXunPESvTqk6KhPsWb9yCjgdAmKz5MtLBC",
		AdminUsername:     "admin",
		EncryptionKey:     testEncKey,
		Structure:         structDB,
		SyncEngine:        syncEngine,
	})

	// Login to get session cookie.
	form := url.Values{"username": {"admin"}, "password": {"secret"}}
	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	var sessionCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie from login")
	}
	return srv, sessionCookie, structDB, syncEngine
}

func TestAdminHandlers_ListLocations(t *testing.T) {
	srv, cookie := buildAuthedAdminServer(t)

	req := httptest.NewRequest("GET", "/admin/locations", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /admin/locations: got %d want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Local") {
		t.Errorf("expected location display name in body: %s", w.Body.String())
	}
}

func TestAdminHandlers_ListUsers(t *testing.T) {
	srv, cookie := buildAuthedAdminServer(t)

	req := httptest.NewRequest("GET", "/admin/users", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /admin/users: got %d want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Alice") {
		t.Errorf("expected user display name in body: %s", w.Body.String())
	}
}

func TestAdminHandlers_DashboardShowsDedupStorage(t *testing.T) {
	cacheDir := t.TempDir()
	bucketID := "bucket-1"
	bucketDB, err := meta.OpenBucketDB(filepath.Join(cacheDir, "bucket-"+bucketID+".db"))
	if err != nil {
		t.Fatalf("OpenBucketDB: %v", err)
	}
	now := time.Now()
	for _, key := range []string{"folder-a/file.txt", "folder-b/file.txt"} {
		if err := bucketDB.PutObject(meta.Object{
			ID:             key,
			Key:            key,
			HashPath:       "/_data/shared",
			SizeBytes:      1024,
			ETag:           "etag",
			ContentType:    "text/plain",
			LastModified:   now,
			UploadComplete: true,
		}); err != nil {
			t.Fatalf("PutObject(%s): %v", key, err)
		}
	}
	if err := bucketDB.Close(); err != nil {
		t.Fatalf("Close bucketDB: %v", err)
	}

	structDB := &stubAdminStructureDB{
		locations: []meta.Location{{ID: "loc-1", URL: "http://dav", DisplayName: "Local", QuotaBytes: 1 << 30, BaseDir: "/", CreatedAt: now}},
		users:     []meta.User{{ID: "u-1", AccessKey: "AK123", DisplayName: "Alice", Enabled: true, CreatedAt: now}},
		buckets:   []meta.Bucket{{ID: bucketID, Name: "photos", OwnerUserID: "u-1", WebDAVLocationID: "loc-1", CreatedAt: now}},
	}
	const testEncKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	srv := adminui.NewAdminServer(adminui.AdminDeps{
		AdminPasswordHash: "$2a$10$UVdKbqzndmqLzRIJu2wrXunPESvTqk6KhPsWb9yCjgdAmKz5MtLBC",
		AdminUsername:     "admin",
		EncryptionKey:     testEncKey,
		Structure:         structDB,
		Stats:             &stubAdminStatsDB{totalUsage: 2048},
		LocalCacheDir:     cacheDir,
	})
	form := url.Values{"username": {"admin"}, "password": {"secret"}}
	loginReq := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginW := httptest.NewRecorder()
	srv.ServeHTTP(loginW, loginReq)
	var sessionCookie *http.Cookie
	for _, c := range loginW.Result().Cookies() {
		if c.Name == "session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie from login")
	}

	req := httptest.NewRequest("GET", "/admin/", nil)
	req.AddCookie(sessionCookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "2.00 KB") {
		t.Fatalf("expected logical usage to render as 2.00 KB in body: %s", body)
	}
	if count := strings.Count(body, "1.00 KB"); count < 2 {
		t.Fatalf("expected actual and saved storage to render as 1.00 KB at least twice, got %d in body: %s", count, body)
	}
	if !strings.Contains(body, "Actual") || !strings.Contains(body, "Saved") {
		t.Fatalf("expected Actual and Saved columns in body: %s", body)
	}
}

func TestAdminHandlers_NewLocation(t *testing.T) {
	srv, cookie, structDB, syncEngine := buildAuthedAdminServerWithDBAndSync(t)

	form := url.Values{
		"url": {"http://newdav"}, "username": {"u"}, "password": {"p"},
		"display_name": {"New Location"}, "quota_gb": {"10"},
	}
	req := httptest.NewRequest("POST", "/admin/locations/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Errorf("POST /admin/locations/new: got %d want redirect", w.Code)
	}
	if len(structDB.locations) != 2 {
		t.Fatalf("locations stored: got %d want 2", len(structDB.locations))
	}
	newLocationID := structDB.locations[1].ID
	if len(syncEngine.locationIDs) != 1 || syncEngine.locationIDs[0] != newLocationID {
		t.Fatalf("sync calls: got %+v want [%s]", syncEngine.locationIDs, newLocationID)
	}
}

func TestAdminHandlers_NewUser(t *testing.T) {
	srv, cookie := buildAuthedAdminServer(t)

	form := url.Values{"display_name": {"Bob"}}
	req := httptest.NewRequest("POST", "/admin/users/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("POST /admin/users/new: got %d want 200 (show credentials once)", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "access_key") && !strings.Contains(body, "Access Key") && !strings.Contains(body, "AK") {
		t.Errorf("expected access key in creation response, body: %s", body)
	}
}

func TestAdminHandlers_NewUserShowsAndStoresWebPassword(t *testing.T) {
	srv, cookie, structDB := buildAuthedAdminServerWithDB(t)

	form := url.Values{"display_name": {"Bob"}}
	req := httptest.NewRequest("POST", "/admin/users/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /admin/users/new: got %d want 200", w.Code)
	}
	webPassword := extractCredential(t, w.Body.String(), "Web Password")
	if len(structDB.users) != 2 {
		t.Fatalf("users stored: got %d want 2", len(structDB.users))
	}
	if err := bcrypt.CompareHashAndPassword([]byte(structDB.users[1].WebPasswordHash), []byte(webPassword)); err != nil {
		t.Fatalf("stored web password hash does not match displayed password: %v", err)
	}
}

func TestAdminHandlers_NewUserRequiresLocation(t *testing.T) {
	srv, cookie, structDB := buildAuthedAdminServerWithDB(t)
	structDB.locations = nil

	form := url.Values{"display_name": {"Bob"}}
	req := httptest.NewRequest("POST", "/admin/users/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /admin/users/new without locations: got %d want 200", w.Code)
	}
	if len(structDB.users) != 1 {
		t.Fatalf("users stored: got %d want 1", len(structDB.users))
	}
	if !strings.Contains(w.Body.String(), "Add a WebDAV location before creating users") {
		t.Fatalf("expected missing location message, body: %s", w.Body.String())
	}
}

func TestAdminHandlers_ResetWebPasswordShowsAndStoresNewPassword(t *testing.T) {
	srv, cookie, structDB := buildAuthedAdminServerWithDB(t)

	req := httptest.NewRequest("POST", "/admin/users/u-1/reset-password", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /admin/users/u-1/reset-password: got %d want 200", w.Code)
	}
	webPassword := extractCredential(t, w.Body.String(), "Web Password")
	if err := bcrypt.CompareHashAndPassword([]byte(structDB.users[0].WebPasswordHash), []byte(webPassword)); err != nil {
		t.Fatalf("stored web password hash does not match displayed password: %v", err)
	}
}

func extractCredential(t *testing.T, body, label string) string {
	t.Helper()
	re := regexp.MustCompile(`<tr><th>` + regexp.QuoteMeta(label) + `</th><td><code>([^<]+)</code></td></tr>`)
	matches := re.FindStringSubmatch(body)
	if len(matches) != 2 {
		t.Fatalf("expected %s in response body: %s", label, body)
	}
	return matches[1]
}

func TestAdminHandlers_LocationEditPage(t *testing.T) {
	srv, cookie := buildAuthedAdminServer(t)

	req := httptest.NewRequest("GET", "/admin/locations/loc-1/edit", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /admin/locations/loc-1/edit: got %d want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Local") {
		t.Errorf("expected location name in edit form: %s", w.Body.String())
	}
}

func TestAdminHandlers_DeleteLocation(t *testing.T) {
	srv, cookie := buildAuthedAdminServer(t)

	req := httptest.NewRequest("POST", "/admin/locations/loc-1/delete", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Errorf("POST /admin/locations/loc-1/delete: got %d want redirect", w.Code)
	}
}

func TestAdminHandlers_DeleteUser(t *testing.T) {
	srv, cookie := buildAuthedAdminServer(t)

	req := httptest.NewRequest("POST", "/admin/users/u-1/delete", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Errorf("POST /admin/users/u-1/delete: got %d want redirect", w.Code)
	}
}

func TestAdminHandlers_ListBuckets(t *testing.T) {
	srv, cookie := buildAuthedAdminServer(t)

	req := httptest.NewRequest("GET", "/admin/buckets", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /admin/buckets: got %d want 200", w.Code)
	}
	// The page should render the buckets list heading even when empty.
	if !strings.Contains(w.Body.String(), "Buckets") {
		t.Errorf("expected buckets heading in body: %s", w.Body.String())
	}
}

func TestAdminHandlers_NewBucketPage(t *testing.T) {
	srv, cookie := buildAuthedAdminServer(t)

	req := httptest.NewRequest("GET", "/admin/buckets/new", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /admin/buckets/new: got %d want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Add Bucket") {
		t.Errorf("expected bucket form in body: %s", w.Body.String())
	}
}

func TestAdminHandlers_TestLocationMissingURL(t *testing.T) {
	srv, cookie := buildAuthedAdminServer(t)

	form := url.Values{"username": {"u"}, "password": {"p"}}
	req := httptest.NewRequest("POST", "/admin/locations/test", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST /admin/locations/test missing url: got %d want 400", w.Code)
	}
}

func TestAdminHandlers_TestLocationUnreachable(t *testing.T) {
	srv, cookie := buildAuthedAdminServer(t)

	form := url.Values{"url": {"http://127.0.0.1:1/"}, "username": {"u"}, "password": {"p"}}
	req := httptest.NewRequest("POST", "/admin/locations/test", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("POST /admin/locations/test unreachable: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "flash-error") || !strings.Contains(body, "Connection failed") {
		t.Errorf("expected connection failure flash, got: %s", body)
	}
}
