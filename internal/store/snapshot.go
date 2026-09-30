package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// ErrInvalidSnapshot marks a file that cannot be restored as a herma database.
var ErrInvalidSnapshot = errors.New("invalid herma snapshot")

// SnapshotInfo summarizes a validated snapshot.
type SnapshotInfo struct {
	Records   int64
	Revisions int64
}

// Snapshot writes a complete, compacted copy of the database to path in one
// read transaction. SQLite refuses a path that already holds data.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	return nil
}

// RevisionCount changes exactly when a create or update commits, so it marks
// whether anything was written since the last snapshot.
func (s *Store) RevisionCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM revisions").Scan(&n)
	return n, err
}

// CheckSnapshot validates a snapshot without modifying it or creating journal
// files beside it.
func CheckSnapshot(path string) (SnapshotInfo, error) {
	invalidf := func(format string, args ...any) (SnapshotInfo, error) {
		return SnapshotInfo{}, fmt.Errorf("%w: %s: %s", ErrInvalidSnapshot, path, fmt.Sprintf(format, args...))
	}
	info, err := os.Stat(path)
	if err != nil {
		return invalidf("%v", err)
	}
	if !info.Mode().IsRegular() {
		return invalidf("not a regular file")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return invalidf("%v", err)
	}
	// immutable=1 tells SQLite the file cannot change, so it takes no locks and
	// creates no -wal or -shm files next to the snapshot.
	dsn := (&url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro&immutable=1"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return invalidf("%v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var check string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil {
		return invalidf("%v", err)
	}
	if check != "ok" {
		return invalidf("integrity check failed: %s", check)
	}
	var tables int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('records', 'revisions', 'idempotency')").Scan(&tables); err != nil {
		return invalidf("%v", err)
	}
	if tables != 3 {
		return invalidf("not a herma database")
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return invalidf("%v", err)
	}
	if version > schemaVersion {
		return invalidf("schema version %d is newer than this herma supports (%d)", version, schemaVersion)
	}
	var result SnapshotInfo
	if err := db.QueryRow("SELECT (SELECT count(*) FROM records), (SELECT count(*) FROM revisions)").Scan(&result.Records, &result.Revisions); err != nil {
		return invalidf("%v", err)
	}
	return result, nil
}
