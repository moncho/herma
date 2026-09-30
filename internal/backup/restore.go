package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/moncho/herma/internal/store"
)

// Newest returns the full path of the newest valid snapshot in dir.
func Newest(dir string) (string, store.SnapshotInfo, []string, error) {
	name, info, skipped, err := newestValid(dir)
	if err != nil {
		return "", info, skipped, err
	}
	return filepath.Join(dir, name), info, skipped, nil
}

type InstallResult struct {
	Snapshot     string     `json:"snapshot"`
	SnapshotTime *time.Time `json:"snapshot_time,omitempty"`
	Records      int64      `json:"records"`
	MovedAside   []string   `json:"moved_aside,omitempty"`
}

// Install validates snapshot and makes it the database at dbPath. Existing
// database files are refused unless replace is set, in which case they are
// renamed aside, never deleted. No -wal or -shm file is left beside the result.
func Install(snapshot, dbPath string, replace bool, now time.Time) (InstallResult, error) {
	info, err := store.CheckSnapshot(snapshot)
	if err != nil {
		return InstallResult{}, err
	}
	result := InstallResult{Snapshot: snapshot, Records: info.Records}
	if at, ok := SnapshotTime(filepath.Base(snapshot)); ok {
		result.SnapshotTime = &at
	}
	var existing []string
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Lstat(dbPath + suffix); err == nil {
			existing = append(existing, dbPath+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return InstallResult{}, err
		}
	}
	if len(existing) > 0 && !replace {
		return InstallResult{}, fmt.Errorf("%s already exists; pass --replace to move the current database aside", existing[0])
	}
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return InstallResult{}, fmt.Errorf("create database directory: %w", err)
	}
	staged, err := stage(snapshot, dir)
	if err != nil {
		return InstallResult{}, err
	}
	defer os.Remove(staged)
	// Never overwrite an earlier restore's aside files: step the stamp forward
	// until none of this restore's targets exists.
	at := now.UTC().Truncate(time.Second)
	for asideTaken(existing, at) {
		at = at.Add(time.Second)
	}
	stamp := ".before-restore-" + at.Format(TimeLayout)
	for _, path := range existing {
		if err := os.Rename(path, path+stamp); err != nil {
			return result, fmt.Errorf("move %s aside: %w", path, err)
		}
		result.MovedAside = append(result.MovedAside, path+stamp)
	}
	if err := os.Rename(staged, dbPath); err != nil {
		return result, fmt.Errorf("install restored database: %w", err)
	}
	syncDir(dir)
	return result, nil
}

func asideTaken(paths []string, at time.Time) bool {
	for _, path := range paths {
		if _, err := os.Lstat(path + ".before-restore-" + at.Format(TimeLayout)); err == nil {
			return true
		}
	}
	return false
}

// stage copies the snapshot to a private temporary file in dir.
func stage(snapshot, dir string) (string, error) {
	in, err := os.Open(snapshot)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(dir, ".restore-*")
	if err != nil {
		return "", fmt.Errorf("prepare restored database: %w", err)
	}
	name := out.Name()
	_, copyErr := io.Copy(out, in)
	chmodErr := out.Chmod(0600)
	syncErr := out.Sync()
	closeErr := out.Close()
	if err := errors.Join(copyErr, chmodErr, syncErr, closeErr); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("copy snapshot: %w", err)
	}
	return name, nil
}
