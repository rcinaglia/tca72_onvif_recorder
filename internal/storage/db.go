// Package storage tracks recordings in a SQLite database and enforces the
// configured folder size limit by deleting the oldest recordings first.
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, no cgo required
)

const schema = `
CREATE TABLE IF NOT EXISTS recordings (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	filename   TEXT NOT NULL,
	path       TEXT NOT NULL UNIQUE,
	started_at TEXT NOT NULL,
	ended_at   TEXT,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	status     TEXT NOT NULL DEFAULT 'recording'
);
CREATE INDEX IF NOT EXISTS idx_recordings_started_at ON recordings(started_at);
`

// DB is a handle to the recordings database.
type DB struct {
	sql *sql.DB
}

// Recording is one row of the recordings table.
type Recording struct {
	ID        int64
	Filename  string
	Path      string
	StartedAt time.Time
	SizeBytes int64
	Status    string
}

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	// SQLite handles one writer at a time; this app is effectively
	// single-writer anyway, so keep it simple and avoid "database is locked"
	// errors from concurrent connections.
	sqlDB.SetMaxOpenConns(1)

	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	return &DB{sql: sqlDB}, nil
}

// Close closes the underlying database.
func (db *DB) Close() error {
	return db.sql.Close()
}

// InsertRecording records the start of a new recording and returns its id.
func (db *DB) InsertRecording(path, filename string, startedAt time.Time) (int64, error) {
	res, err := db.sql.Exec(
		`INSERT INTO recordings (filename, path, started_at, status) VALUES (?, ?, ?, 'recording')`,
		filename, path, startedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return 0, fmt.Errorf("inserting recording: %w", err)
	}
	return res.LastInsertId()
}

// FinishRecording marks a recording as finished with its final size.
func (db *DB) FinishRecording(id int64, endedAt time.Time, sizeBytes int64) error {
	_, err := db.sql.Exec(
		`UPDATE recordings SET ended_at = ?, size_bytes = ?, status = 'finished' WHERE id = ?`,
		endedAt.Format(time.RFC3339Nano), sizeBytes, id,
	)
	if err != nil {
		return fmt.Errorf("finishing recording %d: %w", id, err)
	}
	return nil
}

// DeleteRecording removes a recording's row.
func (db *DB) DeleteRecording(id int64) error {
	_, err := db.sql.Exec(`DELETE FROM recordings WHERE id = ?`, id)
	return err
}

// OldestFinished returns the least-recently-started finished recording, or
// sql.ErrNoRows if there is none left to delete.
func (db *DB) OldestFinished() (*Recording, error) {
	row := db.sql.QueryRow(
		`SELECT id, filename, path, started_at, size_bytes, status
		 FROM recordings WHERE status = 'finished'
		 ORDER BY started_at ASC LIMIT 1`,
	)
	var r Recording
	var startedAt string
	if err := row.Scan(&r.ID, &r.Filename, &r.Path, &startedAt, &r.SizeBytes, &r.Status); err != nil {
		return nil, err
	}
	r.StartedAt, _ = time.Parse(time.RFC3339Nano, startedAt)
	return &r, nil
}

// Reconcile is run at startup to recover from an unclean shutdown: any
// recording left in 'recording' status is closed out using whatever ended up
// on disk (finished, with its current size) or dropped if the file is gone.
func (db *DB) Reconcile(outputDir string) error {
	rows, err := db.sql.Query(`SELECT id, path FROM recordings WHERE status = 'recording'`)
	if err != nil {
		return fmt.Errorf("querying dangling recordings: %w", err)
	}
	type pending struct {
		id   int64
		path string
	}
	var stale []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.path); err != nil {
			rows.Close()
			return err
		}
		stale = append(stale, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterating dangling recordings: %w", err)
	}
	rows.Close()

	for _, p := range stale {
		fi, err := os.Stat(p.path)
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("storage: dropping db entry for missing file %s", p.path)
			if err := db.DeleteRecording(p.id); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("statting %s: %w", p.path, err)
		}
		log.Printf("storage: recovering interrupted recording %s (%d bytes)", p.path, fi.Size())
		if err := db.FinishRecording(p.id, fi.ModTime(), fi.Size()); err != nil {
			return err
		}
	}
	return nil
}

// FolderSize returns the total size of regular files directly inside dir.
// It scans the filesystem rather than trusting the database's size_bytes
// column so it stays accurate even if a recording is still growing or the
// two ever drift out of sync.
func FolderSize(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading output dir: %w", err)
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}

// EnforceLimit deletes the oldest finished recordings until the output
// folder's total size is at or below maxBytes. The recording currently in
// progress (status='recording') is never touched.
func (db *DB) EnforceLimit(outputDir string, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	for {
		total, err := FolderSize(outputDir)
		if err != nil {
			return err
		}
		if total <= maxBytes {
			return nil
		}

		rec, err := db.OldestFinished()
		if errors.Is(err, sql.ErrNoRows) {
			// Nothing left we're allowed to delete (e.g. the active
			// recording alone already exceeds the limit).
			log.Printf("storage: folder at %d bytes (limit %d) but no finished recording left to delete", total, maxBytes)
			return nil
		}
		if err != nil {
			return fmt.Errorf("finding oldest recording: %w", err)
		}

		if err := os.Remove(rec.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("storage: failed to remove %s: %v", rec.Path, err)
		}
		if err := db.DeleteRecording(rec.ID); err != nil {
			return fmt.Errorf("deleting recording row %d: %w", rec.ID, err)
		}
		log.Printf("storage: deleted %s to stay under the %d byte limit", filepath.Base(rec.Path), maxBytes)
	}
}
