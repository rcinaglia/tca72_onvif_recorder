package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openTestDB opens the database in its own directory, separate from the
// returned recordings directory, mirroring how config.json keeps
// database.path outside recording.output_dir. FolderSize only ever scans
// the latter, so the db file itself must never land inside it.
func openTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	root := t.TempDir()
	dbDir := filepath.Join(root, "db")
	recordingsDir := filepath.Join(root, "recordings")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	if err := os.MkdirAll(recordingsDir, 0o755); err != nil {
		t.Fatalf("mkdir recordings dir: %v", err)
	}
	db, err := Open(filepath.Join(dbDir, "nvr.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, recordingsDir
}

func writeFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestEnforceLimitDeletesOldestFirst(t *testing.T) {
	db, dir := openTestDB(t)

	base := time.Now()
	makeFinished := func(name string, size int, age time.Duration) {
		path := writeFile(t, dir, name, size)
		id, err := db.InsertRecording(path, name, base.Add(-age))
		if err != nil {
			t.Fatalf("InsertRecording: %v", err)
		}
		if err := db.FinishRecording(id, base.Add(-age), int64(size)); err != nil {
			t.Fatalf("FinishRecording: %v", err)
		}
	}

	// oldest -> newest
	makeFinished("a.mkv", 100, 3*time.Hour)
	makeFinished("b.mkv", 100, 2*time.Hour)
	makeFinished("c.mkv", 100, 1*time.Hour)

	// Total is 300 bytes; limit to 150 should evict the two oldest, keeping c.mkv.
	if err := db.EnforceLimit(dir, 150); err != nil {
		t.Fatalf("EnforceLimit: %v", err)
	}

	for _, gone := range []string{"a.mkv", "b.mkv"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted, stat err = %v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "c.mkv")); err != nil {
		t.Errorf("expected c.mkv to survive: %v", err)
	}

	total, err := FolderSize(dir)
	if err != nil {
		t.Fatalf("FolderSize: %v", err)
	}
	if total > 150 {
		t.Errorf("folder size %d still exceeds limit 150", total)
	}
}

func TestEnforceLimitNeverDeletesActiveRecording(t *testing.T) {
	db, dir := openTestDB(t)

	path := writeFile(t, dir, "active.mkv", 1000)
	if _, err := db.InsertRecording(path, "active.mkv", time.Now()); err != nil {
		t.Fatalf("InsertRecording: %v", err)
	}
	// status stays 'recording': there is nothing finished to evict.

	if err := db.EnforceLimit(dir, 10); err != nil {
		t.Fatalf("EnforceLimit: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("active recording should not have been deleted: %v", err)
	}
}

func TestReconcileRecoversInterruptedRecording(t *testing.T) {
	db, dir := openTestDB(t)

	path := writeFile(t, dir, "crashed.mkv", 42)
	id, err := db.InsertRecording(path, "crashed.mkv", time.Now())
	if err != nil {
		t.Fatalf("InsertRecording: %v", err)
	}

	if err := db.Reconcile(dir); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rec, err := db.OldestFinished()
	if err != nil {
		t.Fatalf("OldestFinished: %v", err)
	}
	if rec.ID != id || rec.SizeBytes != 42 {
		t.Errorf("reconciled recording = %+v, want id=%d size=42", rec, id)
	}
}

func TestReconcileDropsMissingFile(t *testing.T) {
	db, dir := openTestDB(t)

	missingPath := filepath.Join(dir, "gone.mkv")
	if _, err := db.InsertRecording(missingPath, "gone.mkv", time.Now()); err != nil {
		t.Fatalf("InsertRecording: %v", err)
	}

	if err := db.Reconcile(dir); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := db.OldestFinished(); err == nil {
		t.Error("expected no finished recordings after reconciling a missing file")
	}
}
