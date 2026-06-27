package meta

import (
	"database/sql"
	"fmt"
	"io"
	"os"

	_ "modernc.org/sqlite"
)

// openSQLite opens a SQLite database at path using the pure-Go driver.
// Use ":memory:" for in-process tests.
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// Enable WAL and foreign keys
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	// Serialize writers to avoid SQLITE_BUSY under high concurrency.
	db.SetMaxOpenConns(1)
	return db, nil
}

// checkpointSQLite forces a WAL checkpoint so the main .db file contains all
// committed data and the .db-wal/.db-shm sidecars are minimized. Call this
// before copying or uploading a live SQLite file to avoid an incomplete view
// of the database.
func checkpointSQLite(db *sql.DB) error {
	_, err := db.Exec(`PRAGMA wal_checkpoint(FULL)`)
	return err
}

// copyFile copies src file to dst, creating or truncating dst.
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
