package meta

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Location represents a WebDAV backend.
type Location struct {
	ID          string
	URL         string
	Username    string
	PasswordEnc string
	DisplayName string
	QuotaBytes  int64
	BaseDir     string // normalized: "/name/" or "/"
	CreatedAt   time.Time
}

// NormalizeBaseDir ensures the directory has leading and trailing slashes.
// Examples: "backup" → "/backup/", "/backup" → "/backup/", "/" → "/", "" → "/"
func NormalizeBaseDir(input string) string {
	s := strings.TrimSpace(input)
	if s == "" || s == "/" {
		return "/"
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	if !strings.HasSuffix(s, "/") {
		s = s + "/"
	}
	return s
}

// User represents an S3 API user.
type User struct {
	ID              string
	AccessKey       string
	SecretKeyHash   string
	SecretKeyEnc    string // AES-256-GCM encrypted plaintext secret key (base64 nonce||ciphertext)
	WebPasswordHash string // bcrypt hash for the browser user UI
	WebPasswordEnc  string // AES-256-GCM encrypted plaintext web password for export/restore
	DisplayName     string
	Enabled         bool
	CreatedAt       time.Time
}

// Bucket represents an S3 bucket.
type Bucket struct {
	ID               string
	Name             string
	OwnerUserID      string
	WebDAVLocationID string
	CreatedAt        time.Time
}

// StructureDB is the interface for structure.db operations.
type StructureDB interface {
	AddLocation(loc Location) error
	GetLocation(id string) (Location, error)
	ListLocations() ([]Location, error)
	UpdateLocation(loc Location) error
	DeleteLocation(id string) error
	AddUser(u User) error
	GetUserByAccessKey(key string) (User, error)
	GetUser(id string) (User, error)
	ListUsers() ([]User, error)
	UpdateUser(u User) error
	DeleteUser(id string) error
	SetUserEnabled(id string, enabled bool) error
	SetUserWebPassword(id string, hash string) error
	AddBucket(b Bucket) error
	UpdateBucket(b Bucket) error
	GetBucket(name string) (Bucket, error)
	ListBuckets() ([]Bucket, error)
	ListBucketsByUser(userID string) ([]Bucket, error)
	DeleteBucket(name string) error
	SaveToFile(path string) error
	LoadFromFile(path string) error
	Close() error
}

type structureDB struct {
	db *sql.DB
}

// OpenStructureDB opens (or creates) structure.db at path and runs migrations.
func OpenStructureDB(path string) (StructureDB, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	s := &structureDB{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *structureDB) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS webdav_locations (
			id TEXT PRIMARY KEY,
			url TEXT NOT NULL,
			username TEXT NOT NULL,
			password_enc TEXT NOT NULL,
			display_name TEXT NOT NULL,
			quota_bytes INTEGER NOT NULL,
			base_dir TEXT NOT NULL DEFAULT '/',
			created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			access_key TEXT NOT NULL UNIQUE,
			secret_key_hash TEXT NOT NULL,
			secret_key_enc TEXT NOT NULL DEFAULT '',
			web_password_hash TEXT NOT NULL DEFAULT '',
			web_password_enc TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS buckets (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			owner_user_id TEXT NOT NULL,
			webdav_location_id TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	return s.ensureColumn("main", "users", "web_password_enc", "TEXT NOT NULL DEFAULT ''")
}

func (s *structureDB) ensureColumn(schema, table, column, columnDef string) error {
	hasColumn, err := s.tableHasColumn(schema, table, column)
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}

	_, err = s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, columnDef))
	if err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *structureDB) tableHasColumn(schema, table, column string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA %s.table_info(%s)`, schema, table))
	if err != nil {
		return false, fmt.Errorf("table info %s.%s: %w", schema, table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, fmt.Errorf("scan table info %s.%s: %w", schema, table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate table info %s.%s: %w", schema, table, err)
	}
	return false, nil
}

func (s *structureDB) Close() error { return s.db.Close() }

// --- Locations ---

func (s *structureDB) AddLocation(loc Location) error {
	loc.BaseDir = NormalizeBaseDir(loc.BaseDir)
	_, err := s.db.Exec(
		`INSERT INTO webdav_locations (id, url, username, password_enc, display_name, quota_bytes, base_dir, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		loc.ID, loc.URL, loc.Username, loc.PasswordEnc, loc.DisplayName,
		loc.QuotaBytes, loc.BaseDir, loc.CreatedAt.Unix(),
	)
	return err
}

func (s *structureDB) GetLocation(id string) (Location, error) {
	row := s.db.QueryRow(
		`SELECT id, url, username, password_enc, display_name, quota_bytes, base_dir, created_at
		 FROM webdav_locations WHERE id = ?`, id)
	return scanLocation(row)
}

func (s *structureDB) ListLocations() ([]Location, error) {
	rows, err := s.db.Query(
		`SELECT id, url, username, password_enc, display_name, quota_bytes, base_dir, created_at
		 FROM webdav_locations ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var locs []Location
	for rows.Next() {
		loc, err := scanLocation(rows)
		if err != nil {
			return nil, err
		}
		locs = append(locs, loc)
	}
	return locs, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanLocation(row scanner) (Location, error) {
	var loc Location
	var ts int64
	err := row.Scan(&loc.ID, &loc.URL, &loc.Username, &loc.PasswordEnc,
		&loc.DisplayName, &loc.QuotaBytes, &loc.BaseDir, &ts)
	if err != nil {
		return Location{}, fmt.Errorf("scan location: %w", err)
	}
	if loc.BaseDir == "" {
		loc.BaseDir = "/"
	}
	loc.CreatedAt = time.Unix(ts, 0)
	return loc, nil
}

func (s *structureDB) UpdateLocation(loc Location) error {
	loc.BaseDir = NormalizeBaseDir(loc.BaseDir)
	_, err := s.db.Exec(
		`UPDATE webdav_locations SET url = ?, username = ?, password_enc = ?, display_name = ?, quota_bytes = ?, base_dir = ? WHERE id = ?`,
		loc.URL, loc.Username, loc.PasswordEnc, loc.DisplayName, loc.QuotaBytes, loc.BaseDir, loc.ID,
	)
	return err
}

func (s *structureDB) DeleteLocation(id string) error {
	res, err := s.db.Exec(`DELETE FROM webdav_locations WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("location %q not found", id)
	}
	return nil
}

// --- Users ---

func (s *structureDB) AddUser(u User) error {
	enabled := 0
	if u.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO users (id, access_key, secret_key_hash, secret_key_enc, web_password_hash, web_password_enc, display_name, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.AccessKey, u.SecretKeyHash, u.SecretKeyEnc, u.WebPasswordHash, u.WebPasswordEnc, u.DisplayName, enabled, u.CreatedAt.Unix(),
	)
	return err
}

func (s *structureDB) GetUserByAccessKey(key string) (User, error) {
	row := s.db.QueryRow(
		`SELECT id, access_key, secret_key_hash, secret_key_enc, web_password_hash, web_password_enc, display_name, enabled, created_at
		 FROM users WHERE access_key = ?`, key)
	return scanUser(row)
}

func (s *structureDB) GetUser(id string) (User, error) {
	row := s.db.QueryRow(
		`SELECT id, access_key, secret_key_hash, secret_key_enc, web_password_hash, web_password_enc, display_name, enabled, created_at
		 FROM users WHERE id = ?`, id)
	return scanUser(row)
}

func (s *structureDB) ListUsers() ([]User, error) {
	rows, err := s.db.Query(
		`SELECT id, access_key, secret_key_hash, secret_key_enc, web_password_hash, web_password_enc, display_name, enabled, created_at
		 FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *structureDB) SetUserEnabled(id string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE users SET enabled = ? WHERE id = ?`, v, id)
	return err
}

func (s *structureDB) UpdateUser(u User) error {
	enabled := 0
	if u.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(
		`UPDATE users SET access_key = ?, secret_key_hash = ?, secret_key_enc = ?, web_password_hash = ?, web_password_enc = ?, display_name = ?, enabled = ? WHERE id = ?`,
		u.AccessKey, u.SecretKeyHash, u.SecretKeyEnc, u.WebPasswordHash, u.WebPasswordEnc, u.DisplayName, enabled, u.ID,
	)
	return err
}

func (s *structureDB) DeleteUser(id string) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %q not found", id)
	}
	return nil
}

func (s *structureDB) SetUserWebPassword(id string, hash string) error {
	_, err := s.db.Exec(`UPDATE users SET web_password_hash = ? WHERE id = ?`, hash, id)
	return err
}

func (s *structureDB) SetUserWebPasswordAndEnc(id, hash, enc string) error {
	_, err := s.db.Exec(`UPDATE users SET web_password_hash = ?, web_password_enc = ? WHERE id = ?`, hash, enc, id)
	return err
}

func scanUser(row scanner) (User, error) {
	var u User
	var enabled int
	var ts int64
	err := row.Scan(&u.ID, &u.AccessKey, &u.SecretKeyHash, &u.SecretKeyEnc, &u.WebPasswordHash, &u.WebPasswordEnc, &u.DisplayName, &enabled, &ts)
	if err != nil {
		return User{}, fmt.Errorf("scan user: %w", err)
	}
	u.Enabled = enabled != 0
	u.CreatedAt = time.Unix(ts, 0)
	return u, nil
}

// --- Buckets ---

func (s *structureDB) AddBucket(b Bucket) error {
	_, err := s.db.Exec(
		`INSERT INTO buckets (id, name, owner_user_id, webdav_location_id, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		b.ID, b.Name, b.OwnerUserID, b.WebDAVLocationID, b.CreatedAt.Unix(),
	)
	return err
}

func (s *structureDB) UpdateBucket(b Bucket) error {
	_, err := s.db.Exec(
		`UPDATE buckets SET owner_user_id = ?, webdav_location_id = ? WHERE name = ?`,
		b.OwnerUserID, b.WebDAVLocationID, b.Name,
	)
	return err
}

func (s *structureDB) GetBucket(name string) (Bucket, error) {
	row := s.db.QueryRow(
		`SELECT id, name, owner_user_id, webdav_location_id, created_at
		 FROM buckets WHERE name = ?`, name)
	return scanBucket(row)
}

func (s *structureDB) ListBuckets() ([]Bucket, error) {
	rows, err := s.db.Query(
		`SELECT id, name, owner_user_id, webdav_location_id, created_at
		 FROM buckets ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBuckets(rows)
}

func (s *structureDB) ListBucketsByUser(userID string) ([]Bucket, error) {
	rows, err := s.db.Query(
		`SELECT id, name, owner_user_id, webdav_location_id, created_at
		 FROM buckets WHERE owner_user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBuckets(rows)
}

func (s *structureDB) DeleteBucket(name string) error {
	res, err := s.db.Exec(`DELETE FROM buckets WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bucket %q not found", name)
	}
	return nil
}

func scanBucket(row scanner) (Bucket, error) {
	var b Bucket
	var ts int64
	err := row.Scan(&b.ID, &b.Name, &b.OwnerUserID, &b.WebDAVLocationID, &ts)
	if err != nil {
		return Bucket{}, fmt.Errorf("scan bucket: %w", err)
	}
	b.CreatedAt = time.Unix(ts, 0)
	return b, nil
}

func scanBuckets(rows *sql.Rows) ([]Bucket, error) {
	var buckets []Bucket
	for rows.Next() {
		b, err := scanBucket(rows)
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// --- Persistence ---

// SaveToFile writes the in-memory DB to a file-backed SQLite database.
// It uses the SQLite VACUUM INTO command when path is a file.
func (s *structureDB) SaveToFile(path string) error {
	if err := checkpointSQLite(s.db); err != nil {
		return fmt.Errorf("checkpoint structure db: %w", err)
	}
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}

// LoadFromFile replaces the current DB contents with data from a file.
func (s *structureDB) LoadFromFile(path string) error {
	// Attach the source DB, copy tables, detach.
	_, err := s.db.Exec(`ATTACH DATABASE ? AS src`, path)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	defer s.db.Exec(`DETACH DATABASE src`)

	// Re-create schema then copy rows
	if err := s.migrate(); err != nil {
		return err
	}
	for _, tbl := range []string{"webdav_locations", "buckets"} {
		if _, err := s.db.Exec(fmt.Sprintf(`DELETE FROM %s`, tbl)); err != nil {
			return err
		}
		if _, err := s.db.Exec(fmt.Sprintf(`INSERT INTO %s SELECT * FROM src.%s`, tbl, tbl)); err != nil {
			return err
		}
	}
	return s.copyUsersFromAttachedDB("src")
}

func (s *structureDB) copyUsersFromAttachedDB(schema string) error {
	if _, err := s.db.Exec(`DELETE FROM users`); err != nil {
		return err
	}

	sourceHasWebPasswordEnc, err := s.tableHasColumn(schema, "users", "web_password_enc")
	if err != nil {
		return err
	}

	query := fmt.Sprintf(`
		INSERT INTO users (id, access_key, secret_key_hash, secret_key_enc, web_password_hash, web_password_enc, display_name, enabled, created_at)
		SELECT id, access_key, secret_key_hash, secret_key_enc, web_password_hash, web_password_enc, display_name, enabled, created_at
		FROM %s.users`, schema)
	if !sourceHasWebPasswordEnc {
		query = fmt.Sprintf(`
			INSERT INTO users (id, access_key, secret_key_hash, secret_key_enc, web_password_hash, web_password_enc, display_name, enabled, created_at)
			SELECT id, access_key, secret_key_hash, secret_key_enc, web_password_hash, '', display_name, enabled, created_at
			FROM %s.users`, schema)
	}

	_, err = s.db.Exec(query)
	return err
}
