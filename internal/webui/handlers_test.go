package webui_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hpolthof/webdavs3/internal/meta"
	"github.com/hpolthof/webdavs3/internal/object"
	"github.com/hpolthof/webdavs3/internal/webui"
	"golang.org/x/crypto/bcrypt"
)

// --- Stubs ---

type stubStructureDB struct {
	locations []meta.Location
	users     []meta.User
	buckets   []meta.Bucket
}

func (s *stubStructureDB) AddLocation(l meta.Location) error {
	s.locations = append(s.locations, l)
	return nil
}
func (s *stubStructureDB) GetLocation(id string) (meta.Location, error) {
	for _, l := range s.locations {
		if l.ID == id {
			return l, nil
		}
	}
	return meta.Location{}, fmt.Errorf("not found")
}
func (s *stubStructureDB) ListLocations() ([]meta.Location, error) { return s.locations, nil }
func (s *stubStructureDB) UpdateLocation(meta.Location) error      { return nil }
func (s *stubStructureDB) DeleteLocation(string) error             { return nil }
func (s *stubStructureDB) AddUser(u meta.User) error {
	s.users = append(s.users, u)
	return nil
}
func (s *stubStructureDB) GetUserByAccessKey(key string) (meta.User, error) {
	for _, u := range s.users {
		if u.AccessKey == key {
			return u, nil
		}
	}
	return meta.User{}, fmt.Errorf("not found")
}
func (s *stubStructureDB) GetUser(id string) (meta.User, error) {
	for _, u := range s.users {
		if u.ID == id {
			return u, nil
		}
	}
	return meta.User{}, fmt.Errorf("not found")
}
func (s *stubStructureDB) ListUsers() ([]meta.User, error) { return s.users, nil }
func (s *stubStructureDB) UpdateUser(meta.User) error      { return nil }
func (s *stubStructureDB) DeleteUser(string) error         { return nil }
func (s *stubStructureDB) SetUserEnabled(id string, enabled bool) error {
	for i, u := range s.users {
		if u.ID == id {
			s.users[i].Enabled = enabled
			return nil
		}
	}
	return fmt.Errorf("not found")
}
func (s *stubStructureDB) SetUserWebPassword(id string, hash string) error {
	for i, u := range s.users {
		if u.ID == id {
			s.users[i].WebPasswordHash = hash
			return nil
		}
	}
	return fmt.Errorf("not found")
}
func (s *stubStructureDB) AddBucket(b meta.Bucket) error {
	s.buckets = append(s.buckets, b)
	return nil
}
func (s *stubStructureDB) UpdateBucket(b meta.Bucket) error {
	for i, existing := range s.buckets {
		if existing.Name == b.Name {
			s.buckets[i] = b
			return nil
		}
	}
	return fmt.Errorf("not found")
}
func (s *stubStructureDB) GetBucket(name string) (meta.Bucket, error) {
	for _, b := range s.buckets {
		if b.Name == name {
			return b, nil
		}
	}
	return meta.Bucket{}, fmt.Errorf("not found")
}
func (s *stubStructureDB) ListBuckets() ([]meta.Bucket, error) { return s.buckets, nil }
func (s *stubStructureDB) ListBucketsByUser(uid string) ([]meta.Bucket, error) {
	var out []meta.Bucket
	for _, b := range s.buckets {
		if b.OwnerUserID == uid {
			out = append(out, b)
		}
	}
	return out, nil
}
func (s *stubStructureDB) DeleteBucket(name string) error {
	for i, b := range s.buckets {
		if b.Name == name {
			s.buckets = append(s.buckets[:i], s.buckets[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("not found")
}
func (s *stubStructureDB) SaveToFile(string) error   { return nil }
func (s *stubStructureDB) LoadFromFile(string) error { return nil }
func (s *stubStructureDB) Close() error              { return nil }

type stubBucketService struct {
	structure *stubStructureDB
}

func (s *stubBucketService) CreateBucket(ctx context.Context, name, ownerUserID, locationID string) error {
	if _, err := s.structure.GetBucket(name); err == nil {
		return fmt.Errorf("bucket %q already exists", name)
	}
	return s.structure.AddBucket(meta.Bucket{
		ID:               "bkt-" + name,
		Name:             name,
		OwnerUserID:      ownerUserID,
		WebDAVLocationID: locationID,
		CreatedAt:        time.Now(),
	})
}
func (s *stubBucketService) DeleteBucket(ctx context.Context, name string) error {
	return s.structure.DeleteBucket(name)
}
func (s *stubBucketService) ListBuckets(ctx context.Context, ownerUserID string) ([]meta.Bucket, error) {
	return s.structure.ListBucketsByUser(ownerUserID)
}

type stubObjectService struct {
	objects map[string]meta.Object // bucketName/key -> Object
	data    map[string][]byte      // bucketName/key -> content
}

func newStubObjectService() *stubObjectService {
	return &stubObjectService{
		objects: map[string]meta.Object{},
		data:    map[string][]byte{},
	}
}

func (s *stubObjectService) key(bucketName, objectKey string) string {
	return bucketName + "\x00" + objectKey
}

func (s *stubObjectService) Put(ctx context.Context, bucketName, key, contentType string, size int64, body io.Reader) (meta.Object, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return meta.Object{}, err
	}
	k := s.key(bucketName, key)
	s.data[k] = data
	s.objects[k] = meta.Object{
		Key:          key,
		SizeBytes:    int64(len(data)),
		ContentType:  contentType,
		LastModified: time.Now().UTC(),
	}
	return s.objects[k], nil
}
func (s *stubObjectService) Get(ctx context.Context, bucketName, key string) (meta.Object, io.ReadCloser, error) {
	k := s.key(bucketName, key)
	obj, ok := s.objects[k]
	if !ok {
		return meta.Object{}, nil, object.ErrObjectNotFound
	}
	return obj, io.NopCloser(bytes.NewReader(s.data[k])), nil
}
func (s *stubObjectService) Delete(ctx context.Context, bucketName, key string) error {
	k := s.key(bucketName, key)
	if _, ok := s.objects[k]; !ok {
		return object.ErrObjectNotFound
	}
	delete(s.objects, k)
	delete(s.data, k)
	return nil
}
func (s *stubObjectService) Head(ctx context.Context, bucketName, key string) (meta.Object, error) {
	k := s.key(bucketName, key)
	obj, ok := s.objects[k]
	if !ok {
		return meta.Object{}, object.ErrObjectNotFound
	}
	return obj, nil
}
func (s *stubObjectService) List(ctx context.Context, bucketName, prefix, delimiter, continuationToken string, maxKeys int) (object.ListResult, error) {
	var result object.ListResult
	seenPrefixes := map[string]bool{}
	for k, obj := range s.objects {
		parts := strings.SplitN(k, "\x00", 2)
		if len(parts) != 2 || parts[0] != bucketName {
			continue
		}
		key := parts[1]
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		if delimiter != "" && strings.Contains(rest, delimiter) {
			idx := strings.Index(rest, delimiter)
			common := prefix + rest[:idx+len(delimiter)]
			if !seenPrefixes[common] {
				seenPrefixes[common] = true
				result.CommonPrefixes = append(result.CommonPrefixes, common)
			}
			continue
		}
		result.Objects = append(result.Objects, obj)
	}
	return result, nil
}

// --- Test helpers ---

func hashPassword(t *testing.T, pw string) string {
	t.Helper()
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	return string(b)
}

func buildWebUIServer(t *testing.T) (*webui.Server, *stubStructureDB, *stubObjectService) {
	t.Helper()
	structDB := &stubStructureDB{
		locations: []meta.Location{{ID: "loc-1", URL: "http://dav", DisplayName: "Local", QuotaBytes: 1 << 30, CreatedAt: time.Now()}},
		users: []meta.User{{
			ID:              "u-1",
			AccessKey:       "AK123",
			WebPasswordHash: hashPassword(t, "webpass"),
			DisplayName:     "Alice",
			Enabled:         true,
			CreatedAt:       time.Now(),
		}},
		buckets: []meta.Bucket{{
			ID:               "bkt-1",
			Name:             "my-bucket",
			OwnerUserID:      "u-1",
			WebDAVLocationID: "loc-1",
			CreatedAt:        time.Now(),
		}},
	}
	objSvc := newStubObjectService()
	srv := webui.NewServer(webui.Deps{
		Structure:     structDB,
		BucketService: &stubBucketService{structure: structDB},
		ObjectService: objSvc,
	})
	return srv, structDB, objSvc
}

func csrfToken(t *testing.T, srv *webui.Server, cookies []*http.Cookie, path string) (token string, mergedCookies []*http.Cookie) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: got %d want 200", path, w.Code)
	}
	body := w.Body.String()
	const prefix = `name="csrf_token" value="`
	idx := strings.Index(body, prefix)
	if idx == -1 {
		t.Fatalf("csrf token not found on %s", path)
	}
	rest := body[idx+len(prefix):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		t.Fatalf("csrf token value not terminated")
	}
	// Merge response cookies into input cookies so the session cookie is preserved.
	cookieMap := map[string]*http.Cookie{}
	for _, c := range cookies {
		cookieMap[c.Name] = c
	}
	for _, c := range w.Result().Cookies() {
		cookieMap[c.Name] = c
	}
	merged := make([]*http.Cookie, 0, len(cookieMap))
	for _, c := range cookieMap {
		merged = append(merged, c)
	}
	return rest[:end], merged
}

func login(t *testing.T, srv *webui.Server) []*http.Cookie {
	t.Helper()
	token, cookies := csrfToken(t, srv, nil, "/login")
	form := url.Values{"access_key": {"AK123"}, "password": {"webpass"}, "csrf_token": {token}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /login: got %d want redirect", w.Code)
	}
	return w.Result().Cookies()
}

// --- Tests ---

func TestWebUI_LoginSuccess(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)
	cookies := login(t, srv)
	if len(cookies) == 0 {
		t.Fatal("expected session cookie after login")
	}
}

func TestWebUI_LoginSuccessOverHTTPWithBrowserCookieJar(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	client := ts.Client()
	client.Jar = jar

	resp, err := client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read login body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login: got %d want 200", resp.StatusCode)
	}
	body := string(bodyBytes)
	const prefix = `name="csrf_token" value="`
	idx := strings.Index(body, prefix)
	if idx == -1 {
		t.Fatalf("csrf token not found in body: %s", body)
	}
	rest := body[idx+len(prefix):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		t.Fatalf("csrf token value not terminated")
	}

	form := url.Values{"access_key": {"AK123"}, "password": {"webpass"}, "csrf_token": {rest[:end]}}
	resp, err = client.PostForm(ts.URL+"/login", form)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/buckets" {
		t.Fatalf("POST /login final response: got status %d path %s want 200 /buckets", resp.StatusCode, resp.Request.URL.Path)
	}
}

func TestWebUI_LoginAcceptsValidCSRFAfterStaleDuplicateCookie(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)
	token, cookies := csrfToken(t, srv, nil, "/login")
	form := url.Values{"access_key": {"AK123"}, "password": {"webpass"}, "csrf_token": {token}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "webui_csrf", Value: "stale-token"})
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /login with stale duplicate CSRF cookie: got %d want redirect; body: %s", w.Code, w.Body.String())
	}
}

func TestWebUI_LoginCSRFSurvivesServerInstanceChange(t *testing.T) {
	oldSrv, _, _ := buildWebUIServer(t)
	token, cookies := csrfToken(t, oldSrv, nil, "/login")

	newSrv, _, _ := buildWebUIServer(t)
	form := url.Values{"access_key": {"AK123"}, "password": {"webpass"}, "csrf_token": {token}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	newSrv.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /login after server instance change: got %d want redirect; body: %s", w.Code, w.Body.String())
	}
}

func TestWebUI_LoginBadPassword(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)
	token, cookies := csrfToken(t, srv, nil, "/login")
	form := url.Values{"access_key": {"AK123"}, "password": {"wrong"}, "csrf_token": {token}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("POST /login bad password: got %d want 200 (render login with error)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Invalid credentials") {
		t.Errorf("expected error message in body: %s", w.Body.String())
	}
}

func TestWebUI_LoginUsesWorkspaceStyling(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)

	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /login: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="auth-shell"`) {
		t.Fatalf("expected auth shell, body: %s", body)
	}
	if !strings.Contains(body, `Sign in to your workspace`) {
		t.Fatalf("expected upgraded login copy, body: %s", body)
	}
}

func TestWebUI_ListBucketsFilteredByOwner(t *testing.T) {
	srv, structDB, _ := buildWebUIServer(t)
	cookies := login(t, srv)

	// Add a bucket owned by someone else.
	structDB.AddBucket(meta.Bucket{ID: "bkt-2", Name: "other-bucket", OwnerUserID: "u-2", WebDAVLocationID: "loc-1", CreatedAt: time.Now()})

	req := httptest.NewRequest("GET", "/buckets", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /buckets: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "my-bucket") {
		t.Errorf("expected owned bucket in body: %s", body)
	}
	if strings.Contains(body, "other-bucket") {
		t.Errorf("did not expect other user's bucket in body: %s", body)
	}
}

func TestWebUI_BucketsPageIncludesShellAssets(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)
	cookies := login(t, srv)

	req := httptest.NewRequest("GET", "/buckets", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /buckets: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `/static/webui.css`) {
		t.Fatalf("expected webui.css asset, body: %s", body)
	}
	if !strings.Contains(body, `/static/webui.js`) {
		t.Fatalf("expected webui.js asset, body: %s", body)
	}
	if !strings.Contains(body, `class="webui-shell"`) {
		t.Fatalf("expected shell wrapper, body: %s", body)
	}
}

func TestWebUI_BucketsPageShowsWorkspaceHeader(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)
	cookies := login(t, srv)

	req := httptest.NewRequest("GET", "/buckets", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /buckets: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="page-header"`) {
		t.Fatalf("expected page header, body: %s", body)
	}
	if !strings.Contains(body, `Create a bucket to start uploading and organizing files.`) {
		t.Fatalf("expected helper copy, body: %s", body)
	}
}

func TestWebUI_CSSIncludesShellNavClasses(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)

	req := httptest.NewRequest("GET", "/static/webui.css", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /static/webui.css: got %d want 200", w.Code)
	}
	body := w.Body.String()
	for _, selector := range []string{
		".webui-shell",
		".webui-brand",
		".webui-nav-links",
		".webui-nav-actions",
		".btn-quiet",
	} {
		if !strings.Contains(body, selector) {
			t.Fatalf("expected selector %q in webui.css, body: %s", selector, body)
		}
	}
}

func TestWebUI_CSSKeepsBrowseCompatibilityHooks(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)

	req := httptest.NewRequest("GET", "/static/webui.css", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /static/webui.css: got %d want 200", w.Code)
	}
	body := w.Body.String()
	for _, selector := range []string{
		".card",
		".btn-secondary",
		".empty",
	} {
		if !strings.Contains(body, selector) {
			t.Fatalf("expected selector %q in webui.css, body: %s", selector, body)
		}
	}
}

func TestWebUI_CSSKeepsBrowseTableCompatibility(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)

	req := httptest.NewRequest("GET", "/static/webui.css", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /static/webui.css: got %d want 200", w.Code)
	}
	body := w.Body.String()
	for _, selector := range []string{
		"\ntable {\n",
		"\nth,\ntd {\n",
		"\ntr:last-child td {\n",
	} {
		if !strings.Contains(body, selector) {
			t.Fatalf("expected table compatibility selector %q in webui.css, body: %s", selector, body)
		}
	}
}

func TestWebUI_CSSResponsiveTableCompatibilityBlockIsWellFormed(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)

	req := httptest.NewRequest("GET", "/static/webui.css", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /static/webui.css: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, ".resource-table td {\n  td {") {
		t.Fatalf("expected responsive table compatibility block to avoid malformed nested td selector, body: %s", body)
	}
	if !strings.Contains(body, ".resource-table td,\n  td {") {
		t.Fatalf("expected responsive table compatibility td selector list, body: %s", body)
	}
}

func TestWebUI_CreateAndDeleteBucket(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)
	cookies := login(t, srv)

	// Create
	token, cookies := csrfToken(t, srv, cookies, "/buckets")
	form := url.Values{"name": {"new-bucket"}, "csrf_token": {token}}
	req := httptest.NewRequest("POST", "/buckets", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Errorf("POST /buckets: got %d want redirect", w.Code)
	}

	// Delete
	token, cookies = csrfToken(t, srv, cookies, "/buckets")
	form2 := url.Values{"name": {"new-bucket"}, "csrf_token": {token}}
	req2 := httptest.NewRequest("POST", "/buckets/new-bucket/delete", strings.NewReader(form2.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	if w2.Code != http.StatusSeeOther {
		t.Errorf("POST /buckets/new-bucket/delete: got %d want redirect", w2.Code)
	}
}

func TestWebUI_CreateBucketRequiresLocation(t *testing.T) {
	srv, structDB, _ := buildWebUIServer(t)
	structDB.locations = nil
	cookies := login(t, srv)

	token, cookies := csrfToken(t, srv, cookies, "/buckets")
	form := url.Values{"name": {"new-bucket"}, "csrf_token": {token}}
	req := httptest.NewRequest("POST", "/buckets", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /buckets without locations: got %d want 200", w.Code)
	}
	if _, err := structDB.GetBucket("new-bucket"); err == nil {
		t.Fatal("bucket was created despite missing location")
	}
	if !strings.Contains(w.Body.String(), "No WebDAV location configured") {
		t.Fatalf("expected missing location message, body: %s", w.Body.String())
	}
}

func TestWebUI_BrowseObjects(t *testing.T) {
	srv, _, objSvc := buildWebUIServer(t)
	cookies := login(t, srv)
	objSvc.Put(context.Background(), "my-bucket", "file.txt", "text/plain", 12, strings.NewReader("hello world!"))

	req := httptest.NewRequest("GET", "/buckets/my-bucket/browse", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /buckets/my-bucket/browse: got %d want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "file.txt") {
		t.Errorf("expected file.txt in browse body: %s", w.Body.String())
	}
}

func TestWebUI_BrowseUsesWorkspaceLayout(t *testing.T) {
	srv, _, objSvc := buildWebUIServer(t)
	cookies := login(t, srv)
	_, _ = objSvc.Put(context.Background(), "my-bucket", "docs/readme.txt", "text/plain", 6, strings.NewReader("readme"))

	req := httptest.NewRequest("GET", "/buckets/my-bucket/browse?prefix=docs/", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET browse: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="workspace-header"`) {
		t.Fatalf("expected workspace header, body: %s", body)
	}
	if !strings.Contains(body, `data-upload-drop-target`) {
		t.Fatalf("expected file list upload drop target, body: %s", body)
	}
	if !strings.Contains(body, `file-table`) {
		t.Fatalf("expected redesigned file table, body: %s", body)
	}
}

func TestWebUI_BrowseHTMXFragmentReturnsTableOnly(t *testing.T) {
	srv, _, objSvc := buildWebUIServer(t)
	cookies := login(t, srv)
	_, _ = objSvc.Put(context.Background(), "my-bucket", "hello.txt", "text/plain", 5, strings.NewReader("hello"))

	req := httptest.NewRequest("GET", "/buckets/my-bucket/browse", nil)
	req.Header.Set("HX-Request", "true")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "<html") {
		t.Fatalf("expected fragment response, body: %s", body)
	}
	if !strings.Contains(body, `file-table`) {
		t.Fatalf("expected file table fragment, body: %s", body)
	}
}

func TestWebUI_BrowseFileListExposesUploadEnhancementHooks(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)
	cookies := login(t, srv)

	req := httptest.NewRequest("GET", "/buckets/my-bucket/browse", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `data-upload-click-trigger`) {
		t.Fatalf("expected upload click trigger hook, body: %s", body)
	}
	if !strings.Contains(body, `data-upload-drop-target`) {
		t.Fatalf("expected file list drop target hook, body: %s", body)
	}
	if !strings.Contains(body, `data-upload-input="file_input"`) {
		t.Fatalf("expected upload input hook, body: %s", body)
	}
	if !strings.Contains(body, `data-upload-queue`) {
		t.Fatalf("expected upload queue hook, body: %s", body)
	}
	if !strings.Contains(body, `class="sr-only"`) {
		t.Fatalf("expected hidden file input helper class, body: %s", body)
	}
}

func TestWebUI_BrowseRowsRenderFileAndFolderIcons(t *testing.T) {
	srv, _, objSvc := buildWebUIServer(t)
	cookies := login(t, srv)
	_, _ = objSvc.Put(context.Background(), "my-bucket", "folder/", "application/x-directory", 0, strings.NewReader(""))
	_, _ = objSvc.Put(context.Background(), "my-bucket", "report.pdf", "application/pdf", 7, strings.NewReader("content"))

	req := httptest.NewRequest("GET", "/buckets/my-bucket/browse", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `folder-icon`) {
		t.Fatalf("expected folder icon hook, body: %s", body)
	}
	if !strings.Contains(body, `file-icon`) {
		t.Fatalf("expected file icon hook, body: %s", body)
	}
}

func TestWebUI_BrowseHidesCurrentFolderMarker(t *testing.T) {
	srv, _, objSvc := buildWebUIServer(t)
	cookies := login(t, srv)
	objSvc.Put(context.Background(), "my-bucket", "Testing 2/", "application/x-directory", 0, strings.NewReader(""))

	req := httptest.NewRequest("GET", "/buckets/my-bucket/browse?prefix=Testing+2%2F", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET browse folder: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "Download") {
		t.Fatalf("folder marker should not be rendered as a downloadable file: %s", body)
	}
	if !strings.Contains(body, "Upload a file or create a folder to add content here.") {
		t.Fatalf("empty folder message missing: %s", body)
	}
}

func TestWebUI_EmptyFolderShowsGuidedEmptyState(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)
	cookies := login(t, srv)

	req := httptest.NewRequest("GET", "/buckets/my-bucket/browse", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `Upload a file or create a folder to add content here.`) {
		t.Fatalf("expected guided empty state, body: %s", body)
	}
}

func TestWebUI_BrowseLevelUpLinkGoesToParentPrefix(t *testing.T) {
	srv, _, objSvc := buildWebUIServer(t)
	cookies := login(t, srv)
	objSvc.Put(context.Background(), "my-bucket", "Testing 2/", "application/x-directory", 0, strings.NewReader(""))

	req := httptest.NewRequest("GET", "/buckets/my-bucket/browse?prefix=Testing+2%2F", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET browse folder: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/buckets/my-bucket/browse?prefix="`) {
		t.Fatalf("level up link should point to bucket root: %s", body)
	}
	if strings.Contains(body, `href="/buckets/my-bucket/browse?prefix=Testing+2"`) {
		t.Fatalf("level up link still points at current folder: %s", body)
	}
}

func TestWebUI_UploadAndDownload(t *testing.T) {
	srv, _, _ := buildWebUIServer(t)
	cookies := login(t, srv)
	token, cookies := csrfToken(t, srv, cookies, "/buckets/my-bucket/browse")

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("prefix", "")
	_ = mw.WriteField("csrf_token", token)
	fw, _ := mw.CreateFormFile("file", "upload.txt")
	fw.Write([]byte("uploaded content"))
	mw.Close()

	req := httptest.NewRequest("POST", "/buckets/my-bucket/upload", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST upload: got %d want redirect", w.Code)
	}

	req2 := httptest.NewRequest("GET", "/buckets/my-bucket/download?key=upload.txt", nil)
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("GET download: got %d want 200", w2.Code)
	}
	if w2.Body.String() != "uploaded content" {
		t.Errorf("download body: got %q want %q", w2.Body.String(), "uploaded content")
	}
}

func TestWebUI_DeleteObject(t *testing.T) {
	srv, _, objSvc := buildWebUIServer(t)
	cookies := login(t, srv)
	objSvc.Put(context.Background(), "my-bucket", "delete-me.txt", "text/plain", 4, strings.NewReader("bye!"))

	token, cookies := csrfToken(t, srv, cookies, "/buckets/my-bucket/browse")
	form := url.Values{"key": {"delete-me.txt"}, "csrf_token": {token}}
	req := httptest.NewRequest("POST", "/buckets/my-bucket/objects/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST delete: got %d want redirect", w.Code)
	}
	_, _, err := objSvc.Get(context.Background(), "my-bucket", "delete-me.txt")
	if err == nil {
		t.Fatal("expected object to be deleted")
	}
}

func TestWebUI_Mkdir(t *testing.T) {
	srv, _, objSvc := buildWebUIServer(t)
	cookies := login(t, srv)
	token, cookies := csrfToken(t, srv, cookies, "/buckets/my-bucket/browse")

	form := url.Values{"prefix": {""}, "name": {"newfolder"}, "csrf_token": {token}}
	req := httptest.NewRequest("POST", "/buckets/my-bucket/mkdir", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST mkdir: got %d want redirect", w.Code)
	}
	obj, _, err := objSvc.Get(context.Background(), "my-bucket", "newfolder/")
	if err != nil {
		t.Fatalf("expected folder marker: %v", err)
	}
	if obj.ContentType != "application/x-directory" {
		t.Errorf("folder content type: got %q want directory", obj.ContentType)
	}
}

func TestWebUI_RenameObject(t *testing.T) {
	srv, _, objSvc := buildWebUIServer(t)
	cookies := login(t, srv)
	objSvc.Put(context.Background(), "my-bucket", "old.txt", "text/plain", 5, strings.NewReader("hello"))

	token, cookies := csrfToken(t, srv, cookies, "/buckets/my-bucket/browse")
	form := url.Values{"key": {"old.txt"}, "newKey": {"renamed.txt"}, "csrf_token": {token}}
	req := httptest.NewRequest("POST", "/buckets/my-bucket/objects/rename", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST rename: got %d want redirect", w.Code)
	}
	if _, _, err := objSvc.Get(context.Background(), "my-bucket", "old.txt"); err == nil {
		t.Fatal("expected old key to be removed")
	}
	obj, rc, err := objSvc.Get(context.Background(), "my-bucket", "renamed.txt")
	if err != nil {
		t.Fatalf("expected renamed object: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "hello" {
		t.Errorf("renamed content: got %q want %q", string(data), "hello")
	}
	if obj.SizeBytes != 5 {
		t.Errorf("renamed size: got %d want 5", obj.SizeBytes)
	}
}

func TestWebUI_RenameFolderMovesPrefixContents(t *testing.T) {
	srv, _, objSvc := buildWebUIServer(t)
	cookies := login(t, srv)
	objSvc.Put(context.Background(), "my-bucket", "old/", "application/x-directory", 0, strings.NewReader(""))
	objSvc.Put(context.Background(), "my-bucket", "old/report.txt", "text/plain", 7, strings.NewReader("content"))

	token, cookies := csrfToken(t, srv, cookies, "/buckets/my-bucket/browse")
	form := url.Values{"key": {"old/"}, "newKey": {"renamed"}, "csrf_token": {token}}
	req := httptest.NewRequest("POST", "/buckets/my-bucket/objects/rename", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST rename folder: got %d want redirect", w.Code)
	}
	if _, _, err := objSvc.Get(context.Background(), "my-bucket", "old/report.txt"); err == nil {
		t.Fatal("expected old folder contents to be removed")
	}
	if _, _, err := objSvc.Get(context.Background(), "my-bucket", "renamed/"); err != nil {
		t.Fatalf("expected renamed folder marker: %v", err)
	}
	_, rc, err := objSvc.Get(context.Background(), "my-bucket", "renamed/report.txt")
	if err != nil {
		t.Fatalf("expected renamed folder contents: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "content" {
		t.Errorf("renamed folder content: got %q want %q", string(data), "content")
	}
}
