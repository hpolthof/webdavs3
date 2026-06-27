package meta

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	wdv "github.com/hpolthof/webdavs3/internal/webdav"
)

// StatsDB tracks byte and object-count deltas locally before flushing to WebDAV.
type StatsDB interface {
	AddDelta(locationID, userID, bucketID string, bytes, count int64) error
	GetTotalUsage(locationID string) (int64, error)
	MergeFromFile(path, sourceFile string, locationIDMap map[string]string) error
	Flush(ctx context.Context, wdc wdv.Client, remotePath string) error
	Close() error
}

type statsDB struct {
	db       *sql.DB
	daemonID string
	cacheDir string
}

// OpenStatsDB opens the local stats SQLite database for the given daemon.
func OpenStatsDB(path, daemonID string) (StatsDB, error) {
	return OpenStatsDBWithCacheDir(path, daemonID, "")
}

// OpenStatsDBWithCacheDir opens the local stats SQLite database and writes
// temp files to cacheDir during Flush (instead of the OS default /tmp).
func OpenStatsDBWithCacheDir(path, daemonID, cacheDir string) (StatsDB, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	s := &statsDB{db: db, daemonID: daemonID, cacheDir: cacheDir}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *statsDB) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS usage_delta (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			webdav_location_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			bucket_id TEXT NOT NULL,
			bytes_delta INTEGER NOT NULL,
			object_count_delta INTEGER NOT NULL,
			ts INTEGER NOT NULL,
			source_file TEXT NOT NULL DEFAULT '',
			source_row_id INTEGER NOT NULL DEFAULT 0
		);
	`)
	if err != nil {
		return err
	}
	if err := s.ensureColumn("usage_delta", "source_file", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("usage_delta", "source_row_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err = s.db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_delta_source
		ON usage_delta(source_file, source_row_id)
		WHERE source_file <> '';
	`)
	return err
}

func (s *statsDB) ensureColumn(table, column, definition string) error {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition))
	return err
}

func (s *statsDB) Close() error { return s.db.Close() }

func (s *statsDB) AddDelta(locationID, userID, bucketID string, bytes, count int64) error {
	_, err := s.db.Exec(
		`INSERT INTO usage_delta (webdav_location_id, user_id, bucket_id, bytes_delta, object_count_delta, ts)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		locationID, userID, bucketID, bytes, count, time.Now().Unix(),
	)
	return err
}

func (s *statsDB) GetTotalUsage(locationID string) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRow(
		`SELECT SUM(bytes_delta) FROM usage_delta WHERE webdav_location_id = ?`, locationID,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("GetTotalUsage: %w", err)
	}
	return total.Int64, nil
}

func (s *statsDB) MergeFromFile(path, sourceFile string, locationIDMap map[string]string) error {
	src, err := OpenStatsDB(path, "remote")
	if err != nil {
		return fmt.Errorf("open stats source: %w", err)
	}
	defer src.Close()

	srcDB := src.(*statsDB)
	rows, err := srcDB.db.Query(`
		SELECT id, webdav_location_id, user_id, bucket_id, bytes_delta, object_count_delta, ts
		FROM usage_delta ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query stats source: %w", err)
	}
	defer rows.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for rows.Next() {
		var id, bytesDelta, countDelta, ts int64
		var locationID, userID, bucketID string
		if err := rows.Scan(&id, &locationID, &userID, &bucketID, &bytesDelta, &countDelta, &ts); err != nil {
			return fmt.Errorf("scan stats source: %w", err)
		}
		if mapped, ok := locationIDMap[locationID]; ok {
			locationID = mapped
		}
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO usage_delta (
				webdav_location_id, user_id, bucket_id, bytes_delta,
				object_count_delta, ts, source_file, source_row_id
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			locationID, userID, bucketID, bytesDelta, countDelta, ts, sourceFile, id,
		); err != nil {
			return fmt.Errorf("insert stats source row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

// Flush saves the current stats DB to a temp file and uploads it to WebDAV.
func (s *statsDB) Flush(ctx context.Context, wdc wdv.Client, remotePath string) error {
	tmp, err := os.CreateTemp(s.cacheDir, "stats-*.db")
	if err != nil {
		return fmt.Errorf("create temp for flush: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if err := checkpointSQLite(s.db); err != nil {
		return fmt.Errorf("checkpoint stats db: %w", err)
	}
	if _, err := s.db.Exec(`VACUUM INTO ?`, tmpPath); err != nil {
		return fmt.Errorf("vacuum into temp: %w", err)
	}
	return wdc.UploadFromFile(ctx, remotePath, tmpPath)
}
