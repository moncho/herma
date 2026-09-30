package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func fileStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "live.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSnapshotCopiesRecordsAndHistory(t *testing.T) {
	s := fileStore(t)
	ctx := context.Background()
	r := createRecord(t, s, CreateInput{Kind: "task", Title: "Snapshot me"})
	if _, _, err := s.Update(ctx, r.ID, reviewer("agent-b"), "", UpdateInput{Version: 1, Title: pointer("Snapshot me twice")}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snap.sqlite3")
	if err := s.Snapshot(ctx, path); err != nil {
		t.Fatal(err)
	}
	info, err := CheckSnapshot(path)
	if err != nil || info.Records != 1 || info.Revisions != 2 {
		t.Fatalf("check: %+v %v", info, err)
	}
	restored, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.Get(ctx, r.ID)
	if err != nil || got.Title != "Snapshot me twice" || got.Version != 2 {
		t.Fatalf("restored record: %+v %v", got, err)
	}
	history, err := restored.History(ctx, r.ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("restored history: %d %v", len(history), err)
	}
}

func TestSnapshotDuringWritesIsConsistent(t *testing.T) {
	s := fileStore(t)
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r, _, err := s.Create(ctx, reviewer("writer"), "", CreateInput{Kind: "task", Title: fmt.Sprint("write ", i)})
			if err != nil {
				t.Error(err)
				return
			}
			if _, _, err := s.Update(ctx, r.ID, reviewer("writer"), "", UpdateInput{Version: 1, Title: pointer(fmt.Sprint("rewrite ", i))}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	path := filepath.Join(t.TempDir(), "snap.sqlite3")
	err := s.Snapshot(ctx, path)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	info, err := CheckSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	// Every record has exactly as many revisions as its version: no snapshot
	// can hold a record without the revision that produced it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mismatched int
	err = db.QueryRow(`SELECT count(*) FROM records r WHERE json_extract(r.data, '$.version') !=
 (SELECT count(*) FROM revisions v WHERE v.record_id = r.id)`).Scan(&mismatched)
	if err != nil || mismatched != 0 {
		t.Fatalf("inconsistent snapshot: %d mismatched records (%v), info %+v", mismatched, err, info)
	}
}

func TestSnapshotRefusesExistingFile(t *testing.T) {
	s := fileStore(t)
	path := filepath.Join(t.TempDir(), "taken.sqlite3")
	if err := os.WriteFile(path, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(context.Background(), path); err == nil {
		t.Fatal("snapshot overwrote an existing file")
	}
	if data, _ := os.ReadFile(path); string(data) != "keep me" {
		t.Fatalf("existing file changed: %q", data)
	}
}

func TestRevisionCountTracksCommittedWritesOnly(t *testing.T) {
	s := fileStore(t)
	ctx := context.Background()
	count := func() int64 {
		t.Helper()
		n, err := s.RevisionCount(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 0 {
		t.Fatal("empty store has revisions")
	}
	r := createRecord(t, s, CreateInput{Kind: "task", Title: "Counted"})
	if _, _, err := s.Update(ctx, r.ID, reviewer("agent-b"), "", UpdateInput{Version: 1, Title: pointer("Counted again")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, agent("agent-a"), "", CreateInput{Kind: "knowledge", Title: "Forbidden", Status: "accepted", Sources: reviewSources}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected forbidden write: %v", err)
	}
	if got := count(); got != 2 {
		t.Fatalf("revision count = %d, want 2", got)
	}
}

func TestCheckSnapshotRejectsInvalidFilesWithoutChangingThem(t *testing.T) {
	s := fileStore(t)
	createRecord(t, s, CreateInput{Kind: "task", Title: "Valid"})
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.sqlite3")
	if err := s.Snapshot(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(valid)
	if _, err := CheckSnapshot(valid); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}
	if after, _ := os.ReadFile(valid); !bytes.Equal(before, after) {
		t.Fatal("CheckSnapshot modified the snapshot")
	}
	for _, name := range []string{"valid.sqlite3-wal", "valid.sqlite3-shm", "valid.sqlite3-journal"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("CheckSnapshot created %s", name)
		}
	}

	text := filepath.Join(dir, "text.sqlite3")
	if err := os.WriteFile(text, []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "foreign.sqlite3")
	db, err := sql.Open("sqlite", foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE other (x)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	truncated := filepath.Join(dir, "truncated.sqlite3")
	if err := os.WriteFile(truncated, before[:len(before)/2], 0600); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(dir, "newer.sqlite3")
	copyFile(t, valid, newer)
	db, err = sql.Open("sqlite", newer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for name, path := range map[string]string{"text": text, "foreign": foreign, "truncated": truncated, "newer": newer, "missing": filepath.Join(dir, "missing.sqlite3")} {
		if _, err := CheckSnapshot(path); !errors.Is(err, ErrInvalidSnapshot) {
			t.Errorf("%s: got %v, want ErrInvalidSnapshot", name, err)
		}
	}
}

func TestCheckSnapshotOpensReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	s := fileStore(t)
	createRecord(t, s, CreateInput{Kind: "task", Title: "Read only"})
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.sqlite3")
	if err := s.Snapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	header, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot header bytes 18/19 (2 means WAL): %d/%d", header[18], header[19])
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700); _ = os.Chmod(path, 0600) })
	info, err := CheckSnapshot(path)
	if err != nil || info.Records != 1 || info.Revisions != 1 {
		t.Fatalf("check in read-only directory: %+v %v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory changed: %v %v", entries, err)
	}
}
