package meta_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/hpolthof/webdavs3/internal/meta"
	_ "modernc.org/sqlite"
)

func TestNormalizeBaseDir(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"backup", "/backup/"},
		{"/backup", "/backup/"},
		{"/backup/", "/backup/"},
		{"/", "/"},
		{"", "/"},
		{"  ", "/"},
		{"data-archive", "/data-archive/"},
		{"/my/nested/path", "/my/nested/path/"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := meta.NormalizeBaseDir(tc.input)
			if result != tc.expected {
				t.Errorf("NormalizeBaseDir(%q) = %q, want %q", tc.input, result, tc.expected)
			}
		})
	}
}

func TestStructureDB_Locations(t *testing.T) {
	db, err := meta.OpenStructureDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStructureDB: %v", err)
	}
	defer db.Close()

	loc := meta.Location{
		ID:          "loc-1",
		URL:         "http://localhost/dav",
		Username:    "user",
		PasswordEnc: "enc",
		DisplayName: "Local",
		QuotaBytes:  1 << 30,
		CreatedAt:   time.Now().Truncate(time.Second),
	}
	if err := db.AddLocation(loc); err != nil {
		t.Fatalf("AddLocation: %v", err)
	}

	got, err := db.GetLocation("loc-1")
	if err != nil {
		t.Fatalf("GetLocation: %v", err)
	}
	if got.URL != loc.URL {
		t.Errorf("URL: got %q want %q", got.URL, loc.URL)
	}
	if got.QuotaBytes != loc.QuotaBytes {
		t.Errorf("QuotaBytes: got %d want %d", got.QuotaBytes, loc.QuotaBytes)
	}

	locs, err := db.ListLocations()
	if err != nil {
		t.Fatalf("ListLocations: %v", err)
	}
	if len(locs) != 1 {
		t.Errorf("ListLocations count: got %d want 1", len(locs))
	}
}

func TestStructureDB_Users(t *testing.T) {
	db, err := meta.OpenStructureDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStructureDB: %v", err)
	}
	defer db.Close()

	u := meta.User{
		ID:              "u-1",
		AccessKey:       "AKIAIOSFODNN7EXAMPLE",
		SecretKeyHash:   "$2a$10$hash",
		WebPasswordHash: "$2a$10$webhash",
		DisplayName:     "Alice",
		Enabled:         true,
		CreatedAt:       time.Now().Truncate(time.Second),
	}
	if err := db.AddUser(u); err != nil {
		t.Fatalf("AddUser: %v", err)
	}

	got, err := db.GetUserByAccessKey("AKIAIOSFODNN7EXAMPLE")
	if err != nil {
		t.Fatalf("GetUserByAccessKey: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("User ID: got %q want %q", got.ID, u.ID)
	}
	if got.WebPasswordHash != u.WebPasswordHash {
		t.Errorf("WebPasswordHash: got %q want %q", got.WebPasswordHash, u.WebPasswordHash)
	}

	if err := db.SetUserEnabled("u-1", false); err != nil {
		t.Fatalf("SetUserEnabled: %v", err)
	}
	got2, _ := db.GetUser("u-1")
	if got2.Enabled {
		t.Error("expected user disabled after SetUserEnabled(false)")
	}

	users, err := db.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Errorf("ListUsers count: got %d want 1", len(users))
	}
}

func TestStructureDB_PersistsWebPasswordEnc(t *testing.T) {
	db, err := meta.OpenStructureDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStructureDB: %v", err)
	}
	defer db.Close()

	u := meta.User{
		ID:              "u-1",
		AccessKey:       "AKIAEXAMPLE",
		SecretKeyHash:   "secret-hash",
		SecretKeyEnc:    "secret-enc",
		WebPasswordHash: "web-hash",
		WebPasswordEnc:  "web-enc",
		DisplayName:     "Backup Bot",
		Enabled:         true,
		CreatedAt:       time.Now(),
	}
	if err := db.AddUser(u); err != nil {
		t.Fatalf("AddUser: %v", err)
	}

	got, err := db.GetUser("u-1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.WebPasswordEnc != "web-enc" {
		t.Fatalf("WebPasswordEnc = %q, want %q", got.WebPasswordEnc, "web-enc")
	}
}

func TestStructureDB_MigratesExistingUsersTableForWebPasswordEnc(t *testing.T) {
	path := t.TempDir() + "/structure.db"

	legacyDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open legacy db: %v", err)
	}
	defer legacyDB.Close()

	if err := seedLegacyStructureDB(legacyDB); err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	db, err := meta.OpenStructureDB(path)
	if err != nil {
		t.Fatalf("OpenStructureDB migrated db: %v", err)
	}
	defer db.Close()

	got, err := db.GetUser("u-legacy")
	if err != nil {
		t.Fatalf("GetUser after migration: %v", err)
	}
	if got.WebPasswordEnc != "" {
		t.Fatalf("WebPasswordEnc after migration = %q, want empty default", got.WebPasswordEnc)
	}

	got.WebPasswordEnc = "migrated-web-enc"
	if err := db.UpdateUser(got); err != nil {
		t.Fatalf("UpdateUser after migration: %v", err)
	}

	got, err = db.GetUser("u-legacy")
	if err != nil {
		t.Fatalf("GetUser after update: %v", err)
	}
	if got.WebPasswordEnc != "migrated-web-enc" {
		t.Fatalf("WebPasswordEnc after update = %q, want %q", got.WebPasswordEnc, "migrated-web-enc")
	}
}

func TestStructureDB_LoadFromFile_MigratesLegacyUsersTable(t *testing.T) {
	path := t.TempDir() + "/legacy-structure.db"

	legacyDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open legacy db: %v", err)
	}
	defer legacyDB.Close()

	if err := seedLegacyStructureDB(legacyDB); err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	db, err := meta.OpenStructureDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStructureDB destination db: %v", err)
	}
	defer db.Close()

	if err := db.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile legacy db: %v", err)
	}

	got, err := db.GetUser("u-legacy")
	if err != nil {
		t.Fatalf("GetUser after legacy load: %v", err)
	}
	if got.WebPasswordHash != "web-hash" {
		t.Fatalf("WebPasswordHash after legacy load = %q, want %q", got.WebPasswordHash, "web-hash")
	}
	if got.WebPasswordEnc != "" {
		t.Fatalf("WebPasswordEnc after legacy load = %q, want empty default", got.WebPasswordEnc)
	}

	loc, err := db.GetLocation("loc-legacy")
	if err != nil {
		t.Fatalf("GetLocation after legacy load: %v", err)
	}
	if loc.URL != "http://legacy" {
		t.Fatalf("Location URL after legacy load = %q, want %q", loc.URL, "http://legacy")
	}

	bucket, err := db.GetBucket("legacy-bucket")
	if err != nil {
		t.Fatalf("GetBucket after legacy load: %v", err)
	}
	if bucket.OwnerUserID != "u-legacy" {
		t.Fatalf("Bucket owner after legacy load = %q, want %q", bucket.OwnerUserID, "u-legacy")
	}
}

func seedLegacyStructureDB(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE webdav_locations (
			id TEXT PRIMARY KEY,
			url TEXT NOT NULL,
			username TEXT NOT NULL,
			password_enc TEXT NOT NULL,
			display_name TEXT NOT NULL,
			quota_bytes INTEGER NOT NULL,
			base_dir TEXT NOT NULL DEFAULT '/',
			created_at INTEGER NOT NULL
		);
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			access_key TEXT NOT NULL UNIQUE,
			secret_key_hash TEXT NOT NULL,
			secret_key_enc TEXT NOT NULL DEFAULT '',
			web_password_hash TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL
		);
		CREATE TABLE buckets (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			owner_user_id TEXT NOT NULL,
			webdav_location_id TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
		INSERT INTO webdav_locations (id, url, username, password_enc, display_name, quota_bytes, base_dir, created_at)
		VALUES ('loc-legacy', 'http://legacy', 'legacy-user', 'legacy-pass', 'Legacy Location', 1073741824, '/', 1710000000);
		INSERT INTO users (id, access_key, secret_key_hash, secret_key_enc, web_password_hash, display_name, enabled, created_at)
		VALUES ('u-legacy', 'AKIALEGACY', 'secret-hash', 'secret-enc', 'web-hash', 'Legacy User', 1, 1710000000);
		INSERT INTO buckets (id, name, owner_user_id, webdav_location_id, created_at)
		VALUES ('b-legacy', 'legacy-bucket', 'u-legacy', 'loc-legacy', 1710000000);
	`)
	return err
}

func TestStructureDB_Buckets(t *testing.T) {
	db, err := meta.OpenStructureDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStructureDB: %v", err)
	}
	defer db.Close()

	// Add prerequisite user and location for FK-style reference
	db.AddUser(meta.User{ID: "u-1", AccessKey: "AK", SecretKeyHash: "h", DisplayName: "A", Enabled: true, CreatedAt: time.Now()})
	db.AddLocation(meta.Location{ID: "loc-1", URL: "http://x", Username: "u", PasswordEnc: "e", DisplayName: "L", QuotaBytes: 0, CreatedAt: time.Now()})

	b := meta.Bucket{
		ID:               "bkt-1",
		Name:             "my-bucket",
		OwnerUserID:      "u-1",
		WebDAVLocationID: "loc-1",
		CreatedAt:        time.Now().Truncate(time.Second),
	}
	if err := db.AddBucket(b); err != nil {
		t.Fatalf("AddBucket: %v", err)
	}

	got, err := db.GetBucket("my-bucket")
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	if got.ID != b.ID {
		t.Errorf("Bucket ID: got %q want %q", got.ID, b.ID)
	}

	byUser, err := db.ListBucketsByUser("u-1")
	if err != nil {
		t.Fatalf("ListBucketsByUser: %v", err)
	}
	if len(byUser) != 1 {
		t.Errorf("ListBucketsByUser count: got %d want 1", len(byUser))
	}

	if err := db.DeleteBucket("my-bucket"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	_, err = db.GetBucket("my-bucket")
	if err == nil {
		t.Fatal("expected error after DeleteBucket, got nil")
	}
}

func TestStructureDB_SaveLoad(t *testing.T) {
	db, err := meta.OpenStructureDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStructureDB: %v", err)
	}
	db.AddLocation(meta.Location{
		ID: "loc-1", URL: "http://x", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 100, CreatedAt: time.Now(),
	})

	path := t.TempDir() + "/structure.db"
	if err := db.SaveToFile(path); err != nil {
		t.Fatalf("SaveToFile: %v", err)
	}
	db.Close()

	db2, err := meta.OpenStructureDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStructureDB2: %v", err)
	}
	defer db2.Close()
	if err := db2.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	locs, err := db2.ListLocations()
	if err != nil {
		t.Fatalf("ListLocations after load: %v", err)
	}
	if len(locs) != 1 || locs[0].ID != "loc-1" {
		t.Errorf("unexpected locations after load: %+v", locs)
	}
}
