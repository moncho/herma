package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// prepareDatabase creates a private database before SQLite opens it. SQLite
// derives new WAL and shared-memory permissions from the database file mode.
func prepareDatabase(path string) error {
	if path == ":memory:" {
		return nil
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := privateRegularFile(path, true); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := privateRegularFile(path+suffix, false); err != nil {
			return err
		}
	}
	return nil
}

func privateRegularFile(path string, create bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil
		}
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("create private database file %s: %w", path, err)
		}
		return f.Close()
	}
	if err != nil {
		return fmt.Errorf("inspect database file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database file %s must be a regular file, not a symlink", path)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open database file %s: %w", path, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect open database file: %w", err)
	}
	if !os.SameFile(info, opened) {
		return errors.New("database file changed while preparing it; retry startup")
	}
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("make database file %s private: %w", path, err)
	}
	return nil
}
