package provisioning

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hpolthof/webdavs3/internal/adminui"
	"github.com/hpolthof/webdavs3/internal/bucket"
	"github.com/hpolthof/webdavs3/internal/meta"
)

func TestDump_IncludesSecretsAndBucketOwners(t *testing.T) {
	structure, err := meta.OpenStructureDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStructureDB: %v", err)
	}
	defer structure.Close()

	const encryptionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

	secretEnc, err := adminui.EncryptPassword("secret-value", encryptionKey)
	if err != nil {
		t.Fatalf("EncryptPassword user secret: %v", err)
	}
	webPasswordEnc, err := adminui.EncryptPassword("browser-pass", encryptionKey)
	if err != nil {
		t.Fatalf("EncryptPassword web password: %v", err)
	}
	locationPasswordEnc, err := adminui.EncryptPassword("dav-pass", encryptionKey)
	if err != nil {
		t.Fatalf("EncryptPassword location password: %v", err)
	}

	createdAt := time.Now()
	if err := structure.AddUser(meta.User{
		ID:              "user-1",
		AccessKey:       "AKIA1",
		SecretKeyHash:   "secret-hash",
		SecretKeyEnc:    secretEnc,
		WebPasswordHash: "web-hash",
		WebPasswordEnc:  webPasswordEnc,
		DisplayName:     "Backup Bot",
		Enabled:         true,
		CreatedAt:       createdAt,
	}); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	if err := structure.AddLocation(meta.Location{
		ID:          "loc-1",
		URL:         "https://dav.example.com",
		Username:    "admin",
		PasswordEnc: locationPasswordEnc,
		DisplayName: "Primary Storage",
		QuotaBytes:  50 * (1 << 30),
		BaseDir:     "/nested/path/",
		CreatedAt:   createdAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("AddLocation: %v", err)
	}
	if err := structure.AddBucket(meta.Bucket{
		ID:               "bucket-1",
		Name:             "backups",
		OwnerUserID:      "user-1",
		WebDAVLocationID: "loc-1",
		CreatedAt:        createdAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("AddBucket: %v", err)
	}

	var out bytes.Buffer
	err = Dump(testingContext(), DumpConfig{
		Structure:     structure,
		EncryptionKey: encryptionKey,
	}, &out)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"secret_key: secret-value",
		"web_password: browser-pass",
		"password: dav-pass",
		"owner: backup-bot",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Dump output missing %q:\n%s", want, got)
		}
	}
}

func TestOpenDumpStructure_FailsWhenStructureDBMissing(t *testing.T) {
	dir := t.TempDir()

	db, err := OpenDumpStructure(dir)
	if err == nil {
		db.Close()
		t.Fatalf("OpenDumpStructure error = nil, want missing structure.db failure")
	}
	if !strings.Contains(err.Error(), "structure.db") {
		t.Fatalf("OpenDumpStructure error = %v, want structure.db mention", err)
	}
}

func TestProvisioningAllowed_RejectsMarkerFile(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "provisioned.flag")
	if err := os.WriteFile(markerPath, []byte("2026-01-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := ProvisioningAllowed(markerPath)
	if !errors.Is(err, ErrProvisioningSkipped) {
		t.Fatalf("ProvisioningAllowed error = %v, want ErrProvisioningSkipped", err)
	}
}

func TestApplyStartupProvisioning_SkipsExistingUserAndBucket(t *testing.T) {
	dir := t.TempDir()
	provisionPath := filepath.Join(dir, "provisioning.yaml")
	content := strings.Join([]string{
		"users:",
		"  - slug: backup-bot",
		"    display_name: Backup Bot",
		"    access_key: AKIA1",
		"    secret_key: secret-value",
		"    web_password: browser-pass",
		"locations:",
		"  - slug: primary",
		"    display_name: Primary",
		"    url: https://dav.example.com",
		"    username: admin",
		"    password: dav-pass",
		"    quota_gb: 50",
		"    base_dir: /",
		"    buckets:",
		"      - name: backups",
		"        owner: backup-bot",
	}, "\n")
	if err := os.WriteFile(provisionPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	structure, err := meta.OpenStructureDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStructureDB: %v", err)
	}
	defer structure.Close()

	stats, err := meta.OpenStatsDB(":memory:", "test-daemon")
	if err != nil {
		t.Fatalf("OpenStatsDB: %v", err)
	}
	defer stats.Close()

	const encKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

	// Pre-populate a user with the same AccessKey and a bucket with the same name,
	// simulating what sync would have imported from the WebDAV remote.
	preUserID := "pre-existing-user-id"
	if err := structure.AddUser(meta.User{
		ID: preUserID, AccessKey: "AKIA1",
		SecretKeyHash: "h", SecretKeyEnc: "e", WebPasswordHash: "h", WebPasswordEnc: "e",
		DisplayName: "Remote Bot", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddUser pre-existing: %v", err)
	}
	preLoc := meta.Location{ID: "pre-loc-id", URL: "https://dav.example.com", Username: "admin",
		PasswordEnc: "x", DisplayName: "Pre", BaseDir: "/", CreatedAt: time.Now()}
	if err := structure.AddLocation(preLoc); err != nil {
		t.Fatalf("AddLocation pre-existing: %v", err)
	}
	if err := structure.AddBucket(meta.Bucket{
		ID: "pre-bucket-id", Name: "backups",
		OwnerUserID: preUserID, WebDAVLocationID: preLoc.ID, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddBucket pre-existing: %v", err)
	}

	bucketSvc := bucket.New(structure, stats, &fakeWebDAVClient{})
	err = ApplyStartupProvisioning(testingContext(), ApplyConfig{
		FilePath:      provisionPath,
		LocalCacheDir: dir,
		EncryptionKey: encKey,
		Structure:     structure,
		Buckets:       bucketSvc,
	})
	if err != nil {
		t.Fatalf("ApplyStartupProvisioning: %v", err)
	}

	users, _ := structure.ListUsers()
	if len(users) != 1 {
		t.Fatalf("ListUsers count = %d, want 1 (no duplicate)", len(users))
	}
	if users[0].ID != preUserID {
		t.Fatalf("user ID = %s, want pre-existing %s", users[0].ID, preUserID)
	}

	buckets, _ := structure.ListBuckets()
	if len(buckets) != 1 {
		t.Fatalf("ListBuckets count = %d, want 1 (no duplicate)", len(buckets))
	}
	if buckets[0].ID != "pre-bucket-id" {
		t.Fatalf("bucket ID = %s, want pre-existing pre-bucket-id", buckets[0].ID)
	}
}

func TestApplyStartupProvisioning_CreatesUsersLocationsBucketsAndMarker(t *testing.T) {
	dir := t.TempDir()
	provisionPath := filepath.Join(dir, "provisioning.yaml")
	content := strings.Join([]string{
		"users:",
		"  - slug: backup-bot",
		"    display_name: Backup Bot",
		"    access_key: AKIA1",
		"    secret_key: secret-value",
		"    web_password: browser-pass",
		"locations:",
		"  - slug: primary",
		"    display_name: Primary",
		"    url: https://dav.example.com",
		"    username: admin",
		"    password: dav-pass",
		"    quota_gb: 50",
		"    base_dir: nested/path",
		"    buckets:",
		"      - name: backups",
		"        owner: backup-bot",
	}, "\n")
	if err := os.WriteFile(provisionPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	structure, err := meta.OpenStructureDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStructureDB: %v", err)
	}
	defer structure.Close()

	stats, err := meta.OpenStatsDB(":memory:", "test-daemon")
	if err != nil {
		t.Fatalf("OpenStatsDB: %v", err)
	}
	defer stats.Close()

	refreshCalls := 0
	bucketSvc := bucket.New(structure, stats, &fakeWebDAVClient{})

	err = ApplyStartupProvisioning(testingContext(), ApplyConfig{
		FilePath:      provisionPath,
		LocalCacheDir: dir,
		EncryptionKey: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
		Structure:     structure,
		Buckets:       bucketSvc,
		RefreshWebDAV: func() { refreshCalls++ },
	})
	if err != nil {
		t.Fatalf("ApplyStartupProvisioning: %v", err)
	}

	users, err := structure.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("ListUsers count = %d, want 1", len(users))
	}
	if users[0].AccessKey != "AKIA1" || users[0].DisplayName != "Backup Bot" || users[0].SecretKeyEnc == "" || users[0].WebPasswordEnc == "" || users[0].WebPasswordHash == "" || users[0].SecretKeyHash == "" || !users[0].Enabled {
		t.Fatalf("ListUsers[0] = %#v, want encrypted + hashed provisioned user", users[0])
	}

	locations, err := structure.ListLocations()
	if err != nil {
		t.Fatalf("ListLocations: %v", err)
	}
	if len(locations) != 1 {
		t.Fatalf("ListLocations count = %d, want 1", len(locations))
	}
	if locations[0].DisplayName != "Primary" || locations[0].PasswordEnc == "" || locations[0].BaseDir != "/nested/path/" || locations[0].QuotaBytes != 50*(1<<30) {
		t.Fatalf("ListLocations[0] = %#v, want normalized provisioned location", locations[0])
	}

	buckets, err := structure.ListBuckets()
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("ListBuckets count = %d, want 1", len(buckets))
	}
	if buckets[0].Name != "backups" || buckets[0].OwnerUserID != users[0].ID || buckets[0].WebDAVLocationID != locations[0].ID {
		t.Fatalf("ListBuckets[0] = %#v, want mapped bucket", buckets[0])
	}

	markerPath := filepath.Join(dir, "provisioned.flag")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("Stat marker: %v", err)
	}
	if refreshCalls != 1 {
		t.Fatalf("RefreshWebDAV calls = %d, want 1", refreshCalls)
	}
}

func testingContext() context.Context {
	return context.Background()
}

type fakeWebDAVClient struct{}

func (f *fakeWebDAVClient) Upload(context.Context, string, io.Reader, int64) error { return nil }
func (f *fakeWebDAVClient) Download(context.Context, string) (io.ReadCloser, error) {
	return nil, os.ErrNotExist
}
func (f *fakeWebDAVClient) Delete(context.Context, string) error               { return nil }
func (f *fakeWebDAVClient) MkdirAll(context.Context, string) error             { return nil }
func (f *fakeWebDAVClient) Exists(context.Context, string) (bool, error)       { return false, nil }
func (f *fakeWebDAVClient) Rename(context.Context, string, string, bool) error { return nil }
func (f *fakeWebDAVClient) DownloadToFile(context.Context, string, string) error {
	return os.ErrNotExist
}
func (f *fakeWebDAVClient) UploadFromFile(context.Context, string, string) error { return nil }
func (f *fakeWebDAVClient) ReadDir(context.Context, string) ([]string, error)    { return nil, nil }
func (f *fakeWebDAVClient) ReadDirInfo(context.Context, string) ([]os.FileInfo, error) {
	return nil, nil
}
func (f *fakeWebDAVClient) Ping(context.Context) error { return nil }
func (f *fakeWebDAVClient) Stat(context.Context, string) (os.FileInfo, error) {
	return nil, os.ErrNotExist
}

func TestValidate_RejectsUnknownBucketOwner(t *testing.T) {
	doc := Document{
		Users: []UserSpec{{
			Slug:        "backup-bot",
			DisplayName: "Backup Bot",
			AccessKey:   "AKIA1",
			SecretKey:   "secret",
			WebPassword: "browser-pass",
		}},
		Locations: []LocationSpec{{
			Slug:        "primary",
			DisplayName: "Primary",
			URL:         "https://dav.example.com",
			Username:    "admin",
			Password:    "dav-pass",
			Buckets: []BucketSpec{{
				Name:  "backups",
				Owner: "missing-user",
			}},
		}},
	}

	err := Validate(doc)
	if err == nil || !strings.Contains(err.Error(), "missing-user") {
		t.Fatalf("Validate error = %v, want unknown owner", err)
	}
}

func TestValidate_RejectsDuplicateBucketNames(t *testing.T) {
	doc := Document{
		Users: []UserSpec{{
			Slug:        "backup-bot",
			DisplayName: "Backup Bot",
			AccessKey:   "AKIA1",
			SecretKey:   "secret",
			WebPassword: "browser-pass",
		}},
		Locations: []LocationSpec{
			{
				Slug:        "primary",
				DisplayName: "Primary",
				URL:         "https://dav-1.example.com",
				Username:    "admin",
				Password:    "dav-pass",
				Buckets: []BucketSpec{{
					Name:  "shared",
					Owner: "backup-bot",
				}},
			},
			{
				Slug:        "secondary",
				DisplayName: "Secondary",
				URL:         "https://dav-2.example.com",
				Username:    "admin",
				Password:    "dav-pass",
				Buckets: []BucketSpec{{
					Name:  "shared",
					Owner: "backup-bot",
				}},
			},
		},
	}

	err := Validate(doc)
	if err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("Validate error = %v, want duplicate bucket", err)
	}
}

func TestValidate_RejectsDuplicateUserSlug(t *testing.T) {
	doc := Document{
		Users: []UserSpec{
			{
				Slug:        "backup-bot",
				DisplayName: "Backup Bot",
				AccessKey:   "AKIA1",
				SecretKey:   "secret-1",
				WebPassword: "browser-pass-1",
			},
			{
				Slug:        "backup-bot",
				DisplayName: "Backup Bot 2",
				AccessKey:   "AKIA2",
				SecretKey:   "secret-2",
				WebPassword: "browser-pass-2",
			},
		},
	}

	err := Validate(doc)
	if err == nil || !strings.Contains(err.Error(), "backup-bot") {
		t.Fatalf("Validate error = %v, want duplicate user slug", err)
	}
}

func TestValidate_RejectsDuplicateAccessKey(t *testing.T) {
	doc := Document{
		Users: []UserSpec{
			{
				Slug:        "backup-bot",
				DisplayName: "Backup Bot",
				AccessKey:   "AKIA1",
				SecretKey:   "secret-1",
				WebPassword: "browser-pass-1",
			},
			{
				Slug:        "reports-bot",
				DisplayName: "Reports Bot",
				AccessKey:   "AKIA1",
				SecretKey:   "secret-2",
				WebPassword: "browser-pass-2",
			},
		},
	}

	err := Validate(doc)
	if err == nil || !strings.Contains(err.Error(), "AKIA1") {
		t.Fatalf("Validate error = %v, want duplicate access key", err)
	}
}

func TestValidate_RejectsDuplicateLocationSlug(t *testing.T) {
	doc := Document{
		Users: []UserSpec{{
			Slug:        "backup-bot",
			DisplayName: "Backup Bot",
			AccessKey:   "AKIA1",
			SecretKey:   "secret",
			WebPassword: "browser-pass",
		}},
		Locations: []LocationSpec{
			{
				Slug:        "primary",
				DisplayName: "Primary",
				URL:         "https://dav-1.example.com",
				Username:    "admin",
				Password:    "dav-pass",
			},
			{
				Slug:        "primary",
				DisplayName: "Primary 2",
				URL:         "https://dav-2.example.com",
				Username:    "admin",
				Password:    "dav-pass",
			},
		},
	}

	err := Validate(doc)
	if err == nil || !strings.Contains(err.Error(), "primary") {
		t.Fatalf("Validate error = %v, want duplicate location slug", err)
	}
}

func TestValidate_RejectsMissingRequiredUserFields(t *testing.T) {
	tests := []struct {
		name       string
		user       UserSpec
		wantSubstr string
	}{
		{
			name: "slug",
			user: UserSpec{
				DisplayName: "Backup Bot",
				AccessKey:   "AKIA1",
				SecretKey:   "secret",
				WebPassword: "browser-pass",
			},
			wantSubstr: "users.slug",
		},
		{
			name: "display_name",
			user: UserSpec{
				Slug:        "backup-bot",
				AccessKey:   "AKIA1",
				SecretKey:   "secret",
				WebPassword: "browser-pass",
			},
			wantSubstr: "users.display_name",
		},
		{
			name: "access_key",
			user: UserSpec{
				Slug:        "backup-bot",
				DisplayName: "Backup Bot",
				SecretKey:   "secret",
				WebPassword: "browser-pass",
			},
			wantSubstr: "users.access_key",
		},
		{
			name: "secret_key",
			user: UserSpec{
				Slug:        "backup-bot",
				DisplayName: "Backup Bot",
				AccessKey:   "AKIA1",
				WebPassword: "browser-pass",
			},
			wantSubstr: "users.secret_key",
		},
		{
			name: "web_password",
			user: UserSpec{
				Slug:        "backup-bot",
				DisplayName: "Backup Bot",
				AccessKey:   "AKIA1",
				SecretKey:   "secret",
			},
			wantSubstr: "users.web_password",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Document{Users: []UserSpec{tc.user}})
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("Validate error = %v, want required-field error containing %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestValidate_RejectsMissingRequiredLocationFields(t *testing.T) {
	tests := []struct {
		name       string
		location   LocationSpec
		wantSubstr string
	}{
		{
			name: "slug",
			location: LocationSpec{
				DisplayName: "Primary",
				URL:         "https://dav.example.com",
				Username:    "admin",
				Password:    "dav-pass",
			},
			wantSubstr: "locations.slug",
		},
		{
			name: "display_name",
			location: LocationSpec{
				Slug:     "primary",
				URL:      "https://dav.example.com",
				Username: "admin",
				Password: "dav-pass",
			},
			wantSubstr: "locations.display_name",
		},
		{
			name: "url",
			location: LocationSpec{
				Slug:        "primary",
				DisplayName: "Primary",
				Username:    "admin",
				Password:    "dav-pass",
			},
			wantSubstr: "locations.url",
		},
		{
			name: "username",
			location: LocationSpec{
				Slug:        "primary",
				DisplayName: "Primary",
				URL:         "https://dav.example.com",
				Password:    "dav-pass",
			},
			wantSubstr: "locations.username",
		},
		{
			name: "password",
			location: LocationSpec{
				Slug:        "primary",
				DisplayName: "Primary",
				URL:         "https://dav.example.com",
				Username:    "admin",
			},
			wantSubstr: "locations.password",
		},
	}

	validUser := UserSpec{
		Slug:        "backup-bot",
		DisplayName: "Backup Bot",
		AccessKey:   "AKIA1",
		SecretKey:   "secret",
		WebPassword: "browser-pass",
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Document{
				Users:     []UserSpec{validUser},
				Locations: []LocationSpec{tc.location},
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("Validate error = %v, want required-field error containing %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestValidate_RejectsMissingRequiredBucketFields(t *testing.T) {
	tests := []struct {
		name       string
		bucket     BucketSpec
		wantSubstr string
	}{
		{
			name: "name",
			bucket: BucketSpec{
				Owner: "backup-bot",
			},
			wantSubstr: "buckets.name",
		},
		{
			name: "owner",
			bucket: BucketSpec{
				Name: "backups",
			},
			wantSubstr: "buckets.owner",
		},
	}

	validUser := UserSpec{
		Slug:        "backup-bot",
		DisplayName: "Backup Bot",
		AccessKey:   "AKIA1",
		SecretKey:   "secret",
		WebPassword: "browser-pass",
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Document{
				Users: []UserSpec{validUser},
				Locations: []LocationSpec{{
					Slug:        "primary",
					DisplayName: "Primary",
					URL:         "https://dav.example.com",
					Username:    "admin",
					Password:    "dav-pass",
					Buckets:     []BucketSpec{tc.bucket},
				}},
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("Validate error = %v, want required-field error containing %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestValidate_RejectsDuplicateUserSlugAfterTrim(t *testing.T) {
	doc := Document{
		Users: []UserSpec{
			{
				Slug:        " backup-bot ",
				DisplayName: "Backup Bot",
				AccessKey:   "AKIA1",
				SecretKey:   "secret-1",
				WebPassword: "browser-pass-1",
			},
			{
				Slug:        "backup-bot",
				DisplayName: "Backup Bot 2",
				AccessKey:   "AKIA2",
				SecretKey:   "secret-2",
				WebPassword: "browser-pass-2",
			},
		},
	}

	err := Validate(doc)
	if err == nil || !strings.Contains(err.Error(), "backup-bot") {
		t.Fatalf("Validate error = %v, want duplicate trimmed user slug", err)
	}
}

func TestValidate_RejectsDuplicateLocationSlugAfterTrim(t *testing.T) {
	doc := Document{
		Users: []UserSpec{{
			Slug:        "backup-bot",
			DisplayName: "Backup Bot",
			AccessKey:   "AKIA1",
			SecretKey:   "secret",
			WebPassword: "browser-pass",
		}},
		Locations: []LocationSpec{
			{
				Slug:        " primary ",
				DisplayName: "Primary",
				URL:         "https://dav-1.example.com",
				Username:    "admin",
				Password:    "dav-pass",
			},
			{
				Slug:        "primary",
				DisplayName: "Primary 2",
				URL:         "https://dav-2.example.com",
				Username:    "admin",
				Password:    "dav-pass",
			},
		},
	}

	err := Validate(doc)
	if err == nil || !strings.Contains(err.Error(), "primary") {
		t.Fatalf("Validate error = %v, want duplicate trimmed location slug", err)
	}
}

func TestValidate_ResolvesBucketOwnerAfterTrim(t *testing.T) {
	doc := Document{
		Users: []UserSpec{{
			Slug:        " backup-bot ",
			DisplayName: "Backup Bot",
			AccessKey:   "AKIA1",
			SecretKey:   "secret",
			WebPassword: "browser-pass",
		}},
		Locations: []LocationSpec{{
			Slug:        "primary",
			DisplayName: "Primary",
			URL:         "https://dav.example.com",
			Username:    "admin",
			Password:    "dav-pass",
			Buckets: []BucketSpec{{
				Name:  "backups",
				Owner: "backup-bot",
			}},
		}},
	}

	if err := Validate(doc); err != nil {
		t.Fatalf("Validate error = %v, want nil for trimmed owner match", err)
	}
}

func TestDocumentNormalize_DefaultsEnabledAndBaseDir(t *testing.T) {
	doc := Document{
		Users: []UserSpec{{
			Slug:        "backup-bot",
			DisplayName: "Backup Bot",
			AccessKey:   "AKIA1",
			SecretKey:   "secret",
			WebPassword: "browser-pass",
		}},
		Locations: []LocationSpec{
			{Slug: "primary", BaseDir: "nested/path"},
			{Slug: "secondary"},
		},
	}

	doc.Normalize()

	if doc.Users[0].Enabled == nil || !*doc.Users[0].Enabled {
		t.Fatalf("Normalize enabled = %v, want true", doc.Users[0].Enabled)
	}
	if got := doc.Locations[0].BaseDir; got != "/nested/path/" {
		t.Fatalf("Normalize base dir = %q, want %q", got, "/nested/path/")
	}
	if got := doc.Locations[1].BaseDir; got != "/" {
		t.Fatalf("Normalize empty base dir = %q, want %q", got, "/")
	}
}

func TestParseFile_ParsesDocument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provisioning.yaml")
	content := strings.Join([]string{
		"users:",
		"  - slug: backup-bot",
		"    display_name: Backup Bot",
		"    access_key: AKIA1",
		"    secret_key: secret",
		"    web_password: browser-pass",
		"locations:",
		"  - slug: primary",
		"    display_name: Primary",
		"    url: https://dav.example.com",
		"    username: admin",
		"    password: dav-pass",
		"    quota_gb: 50",
		"    base_dir: data",
		"    buckets:",
		"      - name: backups",
		"        owner: backup-bot",
	}, "\n")

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	doc, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile error = %v", err)
	}

	want := Document{
		Users: []UserSpec{{
			Slug:        "backup-bot",
			DisplayName: "Backup Bot",
			AccessKey:   "AKIA1",
			SecretKey:   "secret",
			WebPassword: "browser-pass",
		}},
		Locations: []LocationSpec{{
			Slug:        "primary",
			DisplayName: "Primary",
			URL:         "https://dav.example.com",
			Username:    "admin",
			Password:    "dav-pass",
			QuotaGB:     50,
			BaseDir:     "data",
			Buckets: []BucketSpec{{
				Name:  "backups",
				Owner: "backup-bot",
			}},
		}},
	}

	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("ParseFile document = %#v, want %#v", doc, want)
	}
}
