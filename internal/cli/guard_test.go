package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/backup"
	"github.com/moncho/herma/internal/store"
)

func TestServeRefusesFreshDatabaseAgainstExistingBackups(t *testing.T) {
	backups := shortDir(t)
	_, _, stop := startServeWith(t, []string{"--backup-dir", backups})
	data, err := runCLI(t, "create", "--kind", "task", "--title", "Precious")
	if err != nil {
		t.Fatal(err)
	}
	var record store.Record
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	stop()
	before, err := backup.List(backups)
	if err != nil || len(before) == 0 {
		t.Fatalf("snapshots: %v %v", before, err)
	}

	fresh := shortDir(t)
	credentials := filepath.Join(fresh, "credentials.json")
	if err := initCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	_, err = runCLI(t, "--credentials", credentials, "serve", "--db", filepath.Join(fresh, "herma.sqlite3"), "--listen", freeAddress(t), "--backup-dir", backups, "--backup-keep", "1")
	if err == nil || !strings.Contains(err.Error(), "herma restore") {
		t.Fatalf("serve: %v", err)
	}
	after, err := backup.List(backups)
	if err != nil || strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("snapshots changed: %v -> %v (%v)", before, after, err)
	}

	db := filepath.Join(shortDir(t), "herma.sqlite3")
	if _, err := runCLI(t, "--credentials", credentials, "restore", backups, "--db", db); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got, err := restored.Get(context.Background(), record.ID); err != nil || got.Title != "Precious" {
		t.Fatalf("restored: %+v %v", got, err)
	}
}

func TestServeWithMissingBackupDirCreatesNoDatabase(t *testing.T) {
	cleanEnv(t)
	dir := shortDir(t)
	credentials := filepath.Join(dir, "credentials.json")
	if err := initCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "data", "herma.sqlite3")
	_, err := runCLI(t, "--credentials", credentials, "serve", "--db", db, "--listen", freeAddress(t), "--backup-dir", filepath.Join(dir, "missing"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("database file created: %v", err)
	}
}
