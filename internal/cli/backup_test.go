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

func TestServeRejectsBadBackupConfiguration(t *testing.T) {
	cleanEnv(t)
	dir := shortDir(t)
	credentials := filepath.Join(dir, "credentials.json")
	if err := initCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(dir, "backups")
	if err := os.Mkdir(backups, 0700); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		args []string
		want string
	}{
		"missing dir":          {[]string{"--backup-dir", filepath.Join(dir, "missing")}, "backup directory"},
		"not a directory":      {[]string{"--backup-dir", file}, "not a directory"},
		"interval too short":   {[]string{"--backup-dir", backups, "--backup-every", "1m"}, "at least 5m"},
		"keep too small":       {[]string{"--backup-dir", backups, "--backup-keep", "0"}, "at least 1"},
		"interval without dir": {[]string{"--backup-every", "1h"}, "require --backup-dir"},
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"--credentials", credentials, "serve", "--db", filepath.Join(dir, "herma.sqlite3"), "--listen", freeAddress(t)}, test.args...)
			if _, err := runCLI(t, args...); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want an error containing %q", err, test.want)
			}
		})
	}
}

func TestBackupStatusCommandAfterStartupSnapshot(t *testing.T) {
	backups := shortDir(t)
	startServeWith(t, []string{"--backup-dir", backups})
	data, err := runCLI(t, "backup", "status")
	if err != nil {
		t.Fatal(err)
	}
	var status backup.Status
	if err := json.Unmarshal(data, &status); err != nil || !status.Enabled || status.LastSuccess == nil || status.Stale {
		t.Fatalf("status: %s %v", data, err)
	}
}

func TestRestoreRefusesWhileServerRuns(t *testing.T) {
	backups := shortDir(t)
	startServeWith(t, []string{"--backup-dir", backups})
	if _, err := runCLI(t, "restore", backups, "--db", filepath.Join(shortDir(t), "herma.sqlite3")); err == nil || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("restore while serving: %v", err)
	}
}

func TestBackupAndRestoreOnANewMachine(t *testing.T) {
	backups := shortDir(t)
	_, _, stop := startServeWith(t, []string{"--backup-dir", backups})
	var created []store.Record
	for _, title := range []string{"Keep this decision", "And this handoff"} {
		data, err := runCLI(t, "create", "--kind", "task", "--title", title)
		if err != nil {
			t.Fatal(err)
		}
		var record store.Record
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		created = append(created, record)
	}
	stop() // graceful shutdown writes a final snapshot with both records

	newMachine := shortDir(t)
	credentials := filepath.Join(newMachine, "credentials.json")
	if _, err := runCLI(t, "--credentials", credentials, "init"); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(newMachine, "herma.sqlite3")
	data, err := runCLI(t, "--credentials", credentials, "restore", backups, "--db", db)
	if err != nil {
		t.Fatal(err)
	}
	var result backup.InstallResult
	if err := json.Unmarshal(data, &result); err != nil || result.Records != 2 {
		t.Fatalf("restore output: %s %v", data, err)
	}
	restored, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, record := range created {
		got, err := restored.Get(context.Background(), record.ID)
		if err != nil || got.Title != record.Title {
			t.Fatalf("restored %s: %+v %v", record.ID, got, err)
		}
		history, err := restored.History(context.Background(), record.ID)
		if err != nil || len(history) != 1 {
			t.Fatalf("restored history for %s: %d %v", record.ID, len(history), err)
		}
	}
}
