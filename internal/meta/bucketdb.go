package meta

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Object is a stored S3 object.
type Object struct {
	ID             string
	Key            string
	HashPath       string
	SizeBytes      int64
	ETag           string
	ContentType    string
	LastModified   time.Time
	UploadComplete bool
	Chunks         []ChunkRef // nil = single /_data/ file; non-nil = chunked
}

// MultipartUpload tracks an in-progress multipart upload.
type MultipartUpload struct {
	ID          string
	Key         string
	ContentType string
	Initiated   time.Time
}

// ChunkRef points to one chunk of a multi-part stored object on WebDAV.
type ChunkRef struct {
	PartNumber int
	Path       string
	Size       int64
	MD5Hex     string
}

// MultipartPart is one part within a multipart upload.
type MultipartPart struct {
	UploadID   string
	PartNumber int
	HashPath   string
	SizeBytes  int64
	ETag       string
}

// BucketDB is the interface for per-bucket metadata operations.
type BucketDB interface {
	PutObject(obj Object) error
	GetObject(key string) (Object, error)
	DeleteObject(key string) error
	CountByHashPath(hashPath string) (int, error)
	ListObjects(prefix, delimiter, startAfter string, maxKeys int) ([]Object, []string, error)
	CreateMultipartUpload(up MultipartUpload) error
	GetMultipartUpload(id string) (MultipartUpload, error)
	ListMultipartUploads() ([]MultipartUpload, error)
	AddPart(part MultipartPart) error
	ListParts(uploadID string) ([]MultipartPart, error)
	CompleteMultipartUpload(uploadID string, obj Object) error
	AbortMultipartUpload(uploadID string) error
	SaveToFile(path string) error
	Close() error
}

type bucketDB struct {
	db *sql.DB
}

// OpenBucketDB opens or creates a bucket SQLite database.
func OpenBucketDB(path string) (BucketDB, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	b := &bucketDB{db: db}
	if err := b.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return b, nil
}

func (b *bucketDB) migrate() error {
	_, err := b.db.Exec(`
		CREATE TABLE IF NOT EXISTS objects (
			id TEXT PRIMARY KEY,
			key TEXT NOT NULL UNIQUE,
			hash_path TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			etag TEXT NOT NULL,
			content_type TEXT NOT NULL,
			last_modified INTEGER NOT NULL,
			upload_complete INTEGER NOT NULL DEFAULT 1
		);
		CREATE INDEX IF NOT EXISTS idx_objects_list ON objects(upload_complete, key);
		CREATE TABLE IF NOT EXISTS object_chunks (
			object_key TEXT NOT NULL,
			part_number INTEGER NOT NULL,
			path TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			md5_hex TEXT NOT NULL,
			PRIMARY KEY (object_key, part_number)
		);
		CREATE TABLE IF NOT EXISTS multipart_uploads (
			id TEXT PRIMARY KEY,
			key TEXT NOT NULL,
			content_type TEXT NOT NULL DEFAULT '',
			initiated INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS multipart_parts (
			upload_id TEXT NOT NULL,
			part_number INTEGER NOT NULL,
			hash_path TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			etag TEXT NOT NULL,
			PRIMARY KEY (upload_id, part_number)
		);
	`)
	return err
}

func (b *bucketDB) Close() error { return b.db.Close() }

func (b *bucketDB) PutObject(obj Object) error {
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	uc := 0
	if obj.UploadComplete {
		uc = 1
	}
	_, err = tx.Exec(`
		INSERT INTO objects (id, key, hash_path, size_bytes, etag, content_type, last_modified, upload_complete)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			id=excluded.id, hash_path=excluded.hash_path, size_bytes=excluded.size_bytes,
			etag=excluded.etag, content_type=excluded.content_type,
			last_modified=excluded.last_modified, upload_complete=excluded.upload_complete`,
		obj.ID, obj.Key, obj.HashPath, obj.SizeBytes, obj.ETag,
		obj.ContentType, obj.LastModified.Unix(), uc,
	)
	if err != nil {
		return err
	}

	// Replace chunks for this key.
	if _, err := tx.Exec(`DELETE FROM object_chunks WHERE object_key = ?`, obj.Key); err != nil {
		return err
	}
	for _, c := range obj.Chunks {
		if _, err := tx.Exec(
			`INSERT INTO object_chunks (object_key, part_number, path, size_bytes, md5_hex) VALUES (?, ?, ?, ?, ?)`,
			obj.Key, c.PartNumber, c.Path, c.Size, c.MD5Hex,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (b *bucketDB) GetObject(key string) (Object, error) {
	row := b.db.QueryRow(`
		SELECT id, key, hash_path, size_bytes, etag, content_type, last_modified, upload_complete
		FROM objects WHERE key = ?`, key)
	obj, err := scanObject(row)
	if err != nil {
		return Object{}, err
	}

	rows, err := b.db.Query(`
		SELECT part_number, path, size_bytes, md5_hex
		FROM object_chunks WHERE object_key = ? ORDER BY part_number`, key)
	if err != nil {
		return Object{}, fmt.Errorf("load chunks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c ChunkRef
		if err := rows.Scan(&c.PartNumber, &c.Path, &c.Size, &c.MD5Hex); err != nil {
			return Object{}, fmt.Errorf("scan chunk: %w", err)
		}
		obj.Chunks = append(obj.Chunks, c)
	}
	return obj, rows.Err()
}

func (b *bucketDB) DeleteObject(key string) error {
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM objects WHERE key = ?`, key)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("object %q not found", key)
	}
	if _, err := tx.Exec(`DELETE FROM object_chunks WHERE object_key = ?`, key); err != nil {
		return err
	}
	return tx.Commit()
}

// CountByHashPath returns the number of objects that reference the given hash path.
// Used to detect whether a /_data/ file is shared before deletion (dedup guard).
func (b *bucketDB) CountByHashPath(hashPath string) (int, error) {
	var n int
	err := b.db.QueryRow(`SELECT COUNT(*) FROM objects WHERE hash_path = ?`, hashPath).Scan(&n)
	return n, err
}

func (b *bucketDB) ListObjects(prefix, delimiter, startAfter string, maxKeys int) ([]Object, []string, error) {
	// Without a delimiter we can rely on a single SQL LIMIT because every row
	// maps to exactly one object. With a delimiter many rows can collapse into
	// a single common prefix, so we fetch in batches until we have enough
	// distinct items or run out of rows.
	if delimiter == "" {
		return b.listObjectsSingleQuery(prefix, startAfter, maxKeys)
	}
	return b.listObjectsBatched(prefix, delimiter, startAfter, maxKeys)
}

// listObjectsSingleQuery executes one indexed query. maxKeys <= 0 means no limit.
func (b *bucketDB) listObjectsSingleQuery(prefix, startAfter string, maxKeys int) ([]Object, []string, error) {
	query, args := b.buildListQuery(prefix, startAfter, maxKeys)
	rows, err := b.db.Query(query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var objects []Object
	for rows.Next() {
		obj, err := scanObject(rows)
		if err != nil {
			return nil, nil, err
		}
		objects = append(objects, obj)
	}
	return objects, nil, rows.Err()
}

// listObjectsBatched fetches rows in batches until we have at least maxKeys
// distinct items (objects + common prefixes) or there are no more rows.
// The caller is responsible for truncating the result to the desired page size.
func (b *bucketDB) listObjectsBatched(prefix, delimiter, startAfter string, maxKeys int) ([]Object, []string, error) {
	batchSize := maxKeys
	if batchSize <= 0 {
		// No explicit limit: use a reasonable batch size to avoid one huge query.
		batchSize = 10000
	}

	objects := make([]Object, 0, maxKeys)
	commonPrefixSet := make(map[string]struct{})
	var commonPrefixes []string
	cursor := startAfter

	for {
		query, args := b.buildListQuery(prefix, cursor, batchSize)
		rows, err := b.db.Query(query, args...)
		if err != nil {
			return nil, nil, err
		}

		rowCount := 0
		lastKey := ""
		for rows.Next() {
			obj, err := scanObject(rows)
			if err != nil {
				rows.Close()
				return nil, nil, err
			}
			rowCount++
			lastKey = obj.Key

			rest := obj.Key[len(prefix):]
			idx := strings.Index(rest, delimiter)
			if idx >= 0 {
				cp := prefix + rest[:idx+len(delimiter)]
				if _, seen := commonPrefixSet[cp]; !seen {
					commonPrefixSet[cp] = struct{}{}
					commonPrefixes = append(commonPrefixes, cp)
				}
			} else {
				objects = append(objects, obj)
			}

			if maxKeys > 0 && len(objects)+len(commonPrefixes) >= maxKeys {
				// We have enough items for the caller to detect truncation.
				// Drain the rest of the rows so the connection can be reused.
				for rows.Next() {
				}
				rows.Close()
				return objects, commonPrefixes, nil
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()

		if rowCount == 0 {
			break
		}

		// Continue scanning after the last key seen in this batch.
		cursor = lastKey
	}

	return objects, commonPrefixes, nil
}

// buildListQuery builds the indexed query for listing objects.
// maxKeys <= 0 means no LIMIT.
func (b *bucketDB) buildListQuery(prefix, startAfter string, maxKeys int) (string, []any) {
	query := `SELECT id, key, hash_path, size_bytes, etag, content_type, last_modified, upload_complete
	          FROM objects WHERE upload_complete = 1`
	args := []any{}

	if prefix != "" {
		query += ` AND key LIKE ? ESCAPE '\'`
		args = append(args, escapeLike(prefix)+"%")
	}
	if startAfter != "" {
		query += ` AND key > ?`
		args = append(args, startAfter)
	}
	query += ` ORDER BY key`
	if maxKeys > 0 {
		query += ` LIMIT ?`
		args = append(args, maxKeys)
	}
	return query, args
}

func scanObject(row scanner) (Object, error) {
	var obj Object
	var lm int64
	var uc int
	err := row.Scan(&obj.ID, &obj.Key, &obj.HashPath, &obj.SizeBytes,
		&obj.ETag, &obj.ContentType, &lm, &uc)
	if err != nil {
		return Object{}, fmt.Errorf("scan object: %w", err)
	}
	obj.LastModified = time.Unix(lm, 0)
	obj.UploadComplete = uc != 0
	return obj, nil
}

func escapeLike(s string) string {
	out := make([]byte, 0, len(s)*2)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' || c == '_' || c == '\\' {
			out = append(out, '\\')
		}
		out = append(out, c)
	}
	return string(out)
}

// --- Multipart ---

func (b *bucketDB) CreateMultipartUpload(up MultipartUpload) error {
	_, err := b.db.Exec(`INSERT INTO multipart_uploads (id, key, content_type, initiated) VALUES (?, ?, ?, ?)`,
		up.ID, up.Key, up.ContentType, up.Initiated.Unix())
	return err
}

func (b *bucketDB) GetMultipartUpload(id string) (MultipartUpload, error) {
	var up MultipartUpload
	var ts int64
	err := b.db.QueryRow(`SELECT id, key, content_type, initiated FROM multipart_uploads WHERE id = ?`, id).
		Scan(&up.ID, &up.Key, &up.ContentType, &ts)
	if err != nil {
		return MultipartUpload{}, fmt.Errorf("get multipart upload %s: %w", id, err)
	}
	up.Initiated = time.Unix(ts, 0)
	return up, nil
}

func (b *bucketDB) ListMultipartUploads() ([]MultipartUpload, error) {
	rows, err := b.db.Query(`SELECT id, key, content_type, initiated FROM multipart_uploads ORDER BY initiated`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ups []MultipartUpload
	for rows.Next() {
		var up MultipartUpload
		var ts int64
		if err := rows.Scan(&up.ID, &up.Key, &up.ContentType, &ts); err != nil {
			return nil, err
		}
		up.Initiated = time.Unix(ts, 0)
		ups = append(ups, up)
	}
	return ups, rows.Err()
}

func (b *bucketDB) AddPart(part MultipartPart) error {
	_, err := b.db.Exec(`
		INSERT INTO multipart_parts (upload_id, part_number, hash_path, size_bytes, etag)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(upload_id, part_number) DO UPDATE SET
			hash_path=excluded.hash_path, size_bytes=excluded.size_bytes, etag=excluded.etag`,
		part.UploadID, part.PartNumber, part.HashPath, part.SizeBytes, part.ETag)
	return err
}

func (b *bucketDB) ListParts(uploadID string) ([]MultipartPart, error) {
	rows, err := b.db.Query(`
		SELECT upload_id, part_number, hash_path, size_bytes, etag
		FROM multipart_parts WHERE upload_id = ? ORDER BY part_number`, uploadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []MultipartPart
	for rows.Next() {
		var p MultipartPart
		if err := rows.Scan(&p.UploadID, &p.PartNumber, &p.HashPath, &p.SizeBytes, &p.ETag); err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

func (b *bucketDB) CompleteMultipartUpload(uploadID string, obj Object) error {
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	uc := 0
	if obj.UploadComplete {
		uc = 1
	}
	_, err = tx.Exec(`
		INSERT INTO objects (id, key, hash_path, size_bytes, etag, content_type, last_modified, upload_complete)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			id=excluded.id, hash_path=excluded.hash_path, size_bytes=excluded.size_bytes,
			etag=excluded.etag, content_type=excluded.content_type,
			last_modified=excluded.last_modified, upload_complete=excluded.upload_complete`,
		obj.ID, obj.Key, obj.HashPath, obj.SizeBytes, obj.ETag,
		obj.ContentType, obj.LastModified.Unix(), uc,
	)
	if err != nil {
		return err
	}

	// Write chunk manifest.
	if _, err := tx.Exec(`DELETE FROM object_chunks WHERE object_key = ?`, obj.Key); err != nil {
		return err
	}
	for _, c := range obj.Chunks {
		if _, err := tx.Exec(
			`INSERT INTO object_chunks (object_key, part_number, path, size_bytes, md5_hex) VALUES (?, ?, ?, ?, ?)`,
			obj.Key, c.PartNumber, c.Path, c.Size, c.MD5Hex,
		); err != nil {
			return err
		}
	}

	// Clean up multipart records (part data stays on WebDAV).
	if _, err := tx.Exec(`DELETE FROM multipart_parts WHERE upload_id = ?`, uploadID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM multipart_uploads WHERE id = ?`, uploadID); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *bucketDB) AbortMultipartUpload(uploadID string) error {
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM multipart_parts WHERE upload_id = ?`, uploadID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM multipart_uploads WHERE id = ?`, uploadID); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *bucketDB) SaveToFile(path string) error {
	if err := checkpointSQLite(b.db); err != nil {
		return fmt.Errorf("checkpoint bucket db: %w", err)
	}
	_, err := b.db.Exec(`VACUUM INTO ?`, path)
	return err
}
