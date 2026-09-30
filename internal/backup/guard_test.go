package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func folderState(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]int64{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		state[entry.Name()] = info.Size()
	}
	return state
}

func TestNewRefusesDatabaseWithFewerRevisionsThanNewestSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshotsIn(t, dir, 2)
	before := folderState(t, dir)
	_, err := New(openStore(t), Config{Dir: dir, Every: time.Hour, Keep: 1}, &bytes.Buffer{}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "herma restore") || !strings.Contains(err.Error(), "2 revisions but the database has 0") {
		t.Fatalf("got %v", err)
	}
	if after := folderState(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("folder changed: %v -> %v", before, after)
	}
}

func TestNewLogsSkippedInvalidSnapshots(t *testing.T) {
	dir := t.TempDir()
	snapshotsIn(t, dir, 1)
	if err := os.WriteFile(filepath.Join(dir, "herma-20991231T000000Z.sqlite3"), []byte("junk"), 0600); err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	write(t, s, "record")
	var out bytes.Buffer
	if _, err := New(s, Config{Dir: dir, Every: time.Hour, Keep: 1}, &out, time.Now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "herma: backup ignored invalid snapshot herma-20991231T000000Z.sqlite3\n") {
		t.Fatalf("log: %q", out.String())
	}
}

func TestSnapshotRecreatesMissingFileOfIdleStore(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	r, _ := newRunner(t, s, dir, 14, c)
	write(t, s, "only write")
	if _, err := r.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, r.Status().LastSuccess.File)); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	written, err := r.Snapshot(context.Background())
	if err != nil || !written {
		t.Fatalf("snapshot after deletion: %t %v", written, err)
	}
	file := r.Status().LastSuccess.File
	if _, err := os.Lstat(filepath.Join(dir, file)); err != nil {
		t.Fatalf("status names a missing file %s: %v", file, err)
	}
}

func TestCheckDir(t *testing.T) {
	if err := CheckDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing accepted")
	}
}

func TestInstallReplaceTwiceInOneSecondKeepsBothAsideSets(t *testing.T) {
	dir := t.TempDir()
	snapshotsIn(t, dir, 1)
	snapshot, _, _, err := Newest(dir)
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "knowledge.sqlite3")
	if err := os.WriteFile(db, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	first, err := Install(snapshot, db, true, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Install(snapshot, db, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(first.MovedAside[0]); err != nil || string(data) != "original" {
		t.Fatalf("first aside lost: %q %v", data, err)
	}
	if second.MovedAside[0] == first.MovedAside[0] {
		t.Fatalf("second restore reused %s", second.MovedAside[0])
	}
	if _, err := os.Stat(second.MovedAside[0]); err != nil {
		t.Fatal(err)
	}
}
