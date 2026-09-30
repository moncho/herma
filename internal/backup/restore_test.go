package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moncho/herma/internal/store"
)

func snapshotsIn(t *testing.T, dir string, count int) *store.Store {
	t.Helper()
	s := openStore(t)
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	r, _ := newRunner(t, s, dir, 14, c)
	for i := 0; i < count; i++ {
		write(t, s, "record")
		if _, err := r.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Hour)
	}
	return s
}

func TestNewestSkipsInvalidSnapshotsAndIgnoresOtherFiles(t *testing.T) {
	dir := t.TempDir()
	snapshotsIn(t, dir, 2) // 12:00 and 13:00
	if err := os.WriteFile(filepath.Join(dir, "herma-20260930T140000Z.sqlite3"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "herma-20260930T150000Z (1).sqlite3"), []byte("conflict copy"), 0600); err != nil {
		t.Fatal(err)
	}
	path, info, skipped, err := Newest(dir)
	if err != nil || filepath.Base(path) != "herma-20260930T130000Z.sqlite3" || info.Records != 2 {
		t.Fatalf("newest: %s %+v %v", path, info, err)
	}
	if len(skipped) != 1 || skipped[0] != "herma-20260930T140000Z.sqlite3" {
		t.Fatalf("skipped: %v", skipped)
	}
	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, ".herma-20260930T120000Z.sqlite3.partial"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Newest(empty); err == nil || !strings.Contains(err.Error(), "no valid herma snapshot") {
		t.Fatalf("empty dir: %v", err)
	}
}

func TestInstallRefusesExistingDatabaseFilesWithoutReplace(t *testing.T) {
	dir := t.TempDir()
	snapshotsIn(t, dir, 1)
	snapshot, _, _, err := Newest(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range []string{"knowledge.sqlite3", "knowledge.sqlite3-wal", "knowledge.sqlite3-shm"} {
		target := t.TempDir()
		if err := os.WriteFile(filepath.Join(target, existing), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Install(snapshot, filepath.Join(target, "knowledge.sqlite3"), false, time.Now()); err == nil || !strings.Contains(err.Error(), "--replace") {
			t.Fatalf("%s: %v", existing, err)
		}
		if data, _ := os.ReadFile(filepath.Join(target, existing)); string(data) != "old" {
			t.Fatalf("%s was modified", existing)
		}
	}
}

func TestInstallReplaceMovesOldFilesAside(t *testing.T) {
	dir := t.TempDir()
	snapshotsIn(t, dir, 1)
	snapshot, _, _, err := Newest(dir)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	db := filepath.Join(target, "knowledge.sqlite3")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(db+suffix, []byte("old"+suffix), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	result, err := Install(snapshot, db, true, now)
	if err != nil || result.Records != 1 || result.SnapshotTime == nil || len(result.MovedAside) != 3 {
		t.Fatalf("install: %+v %v", result, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		aside := db + suffix + ".before-restore-20261001T080000Z"
		if data, err := os.ReadFile(aside); err != nil || string(data) != "old"+suffix {
			t.Fatalf("aside %s: %q %v", aside, data, err)
		}
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(db + suffix); err == nil {
			t.Fatalf("%s left beside the restored database", suffix)
		}
	}
	info, err := os.Stat(db)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("restored file: %v %v", info, err)
	}
	restored, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	restored.Close()
}

func TestInstallCreatesMissingDatabaseDirectory(t *testing.T) {
	dir := t.TempDir()
	snapshotsIn(t, dir, 1)
	snapshot, _, _, err := Newest(dir)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "new-machine", ".herma")
	if _, err := Install(snapshot, filepath.Join(target, "knowledge.sqlite3"), false, time.Now()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("created directory: %v %v", info, err)
	}
}

func TestInstallRefusesInvalidSnapshot(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "herma-20260930T120000Z.sqlite3")
	if err := os.WriteFile(bad, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "knowledge.sqlite3")
	if _, err := Install(bad, target, false, time.Now()); err == nil {
		t.Fatal("installed an invalid snapshot")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("invalid snapshot created a database")
	}
}
