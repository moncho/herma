package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moncho/herma/internal/store"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var owner = store.Author{Name: "owner", Role: store.RoleReviewer}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "knowledge.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func write(t *testing.T, s *store.Store, title string) {
	t.Helper()
	if _, _, err := s.Create(context.Background(), owner, "", store.CreateInput{Kind: "task", Title: title}); err != nil {
		t.Fatal(err)
	}
}

func newRunner(t *testing.T, s *store.Store, dir string, keep int, c *clock) (*Runner, *bytes.Buffer) {
	t.Helper()
	var log bytes.Buffer
	r, err := New(s, Config{Dir: dir, Every: time.Hour, Keep: keep}, &log, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	return r, &log
}

func TestSnapshotWritesVerifiedUTCFileAndSkipsWhenUnchanged(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	c := &clock{now: time.Date(2026, 9, 30, 17, 0, 0, 0, time.FixedZone("UTC+5", 5*3600))}
	r, log := newRunner(t, s, dir, 14, c)
	write(t, s, "first")
	written, err := r.Snapshot(context.Background())
	if err != nil || !written {
		t.Fatalf("first snapshot: %t %v", written, err)
	}
	path := filepath.Join(dir, "herma-20260930T120000Z.sqlite3")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot file: %v %v", info, err)
	}
	if _, err := store.CheckSnapshot(path); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if written, err := r.Snapshot(context.Background()); err != nil || written {
		t.Fatalf("unchanged snapshot: %t %v", written, err)
	}
	write(t, s, "second")
	c.Advance(time.Minute)
	if written, err := r.Snapshot(context.Background()); err != nil || !written {
		t.Fatalf("changed snapshot: %t %v", written, err)
	}
	names, err := List(dir)
	if err != nil || len(names) != 2 || names[0] != "herma-20260930T120200Z.sqlite3" {
		t.Fatalf("snapshots: %v %v", names, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".partial") {
			t.Fatalf("partial file left behind: %s", entry.Name())
		}
	}
	if !strings.Contains(log.String(), "backup written") || !strings.Contains(log.String(), "backup skipped") {
		t.Fatalf("log: %s", log.String())
	}
}

func TestSnapshotsInTheSameSecondGetDistinctOrderedNames(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 500, time.UTC)}
	r, _ := newRunner(t, s, dir, 14, c)
	for i := 0; i < 3; i++ {
		write(t, s, "same second")
		if written, err := r.Snapshot(context.Background()); err != nil || !written {
			t.Fatalf("snapshot %d: %t %v", i, written, err)
		}
	}
	names, err := List(dir)
	want := []string{"herma-20260930T120002Z.sqlite3", "herma-20260930T120001Z.sqlite3", "herma-20260930T120000Z.sqlite3"}
	if err != nil || strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names: %v %v", names, err)
	}
}

func TestNewResumesFromNewestSnapshot(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	write(t, s, "before restart")
	first, _ := newRunner(t, s, dir, 14, c)
	if _, err := first.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Hour)
	second, _ := newRunner(t, s, dir, 14, c)
	status := second.Status()
	if status.LastSuccess == nil || status.LastSuccess.File != "herma-20260930T120000Z.sqlite3" || !status.LastSuccess.At.Equal(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("resumed status: %+v", status)
	}
	if written, err := second.Snapshot(context.Background()); err != nil || written {
		t.Fatalf("restart without writes wrote a duplicate: %t %v", written, err)
	}
}

func TestPruneKeepsNewestAndIgnoresOtherFiles(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	others := []string{"notes.txt", "herma-20260101T000000Z (1).sqlite3", ".herma-20260101T000000Z.sqlite3.icloud", "knowledge.sqlite3", "knowledge.sqlite3-wal"}
	for _, name := range others {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	r, _ := newRunner(t, s, dir, 2, c)
	for i := 0; i < 3; i++ {
		write(t, s, "change")
		if _, err := r.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Hour)
	}
	names, err := List(dir)
	if err != nil || len(names) != 2 || names[0] != "herma-20260930T140000Z.sqlite3" || names[1] != "herma-20260930T130000Z.sqlite3" {
		t.Fatalf("kept: %v %v", names, err)
	}
	for _, name := range others {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("pruning touched %s: %v", name, err)
		}
	}
}

func TestSnapshotFailureRecordsErrorAndPrunesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s := openStore(t)
	dir := t.TempDir()
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	r, log := newRunner(t, s, dir, 1, c)
	write(t, s, "saved")
	if _, err := r.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	write(t, s, "not saved")
	c.Advance(time.Hour)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if written, err := r.Snapshot(context.Background()); err == nil || written {
		t.Fatalf("snapshot into read-only dir: %t %v", written, err)
	}
	status := r.Status()
	if status.LastError == nil || status.LastSuccess == nil || status.LastSuccess.File != "herma-20260930T120000Z.sqlite3" {
		t.Fatalf("status after failure: %+v", status)
	}
	names, _ := List(dir)
	if len(names) != 1 {
		t.Fatalf("failure changed snapshots: %v", names)
	}
	if !strings.Contains(log.String(), "backup failed") {
		t.Fatalf("log: %s", log.String())
	}
}

func TestNewRejectsUnusableConfiguration(t *testing.T) {
	s := openStore(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]Config{
		"missing dir": {Dir: filepath.Join(t.TempDir(), "missing"), Every: time.Hour, Keep: 1},
		"file":        {Dir: file, Every: time.Hour, Keep: 1},
		"zero keep":   {Dir: t.TempDir(), Every: time.Hour, Keep: 0},
		"zero every":  {Dir: t.TempDir(), Every: 0, Keep: 1},
	}
	if os.Geteuid() != 0 {
		readOnly := t.TempDir()
		if err := os.Chmod(readOnly, 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(readOnly, 0700) })
		cases["read-only dir"] = Config{Dir: readOnly, Every: time.Hour, Keep: 1}
	}
	for name, cfg := range cases {
		if _, err := New(s, cfg, &bytes.Buffer{}, time.Now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStatusReportsStaleness(t *testing.T) {
	s := openStore(t)
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	r, _ := newRunner(t, s, t.TempDir(), 14, c)
	if status := r.Status(); !status.Enabled || !status.Stale || status.LastSuccess != nil || status.Every != "1h0m0s" || status.Keep != 14 {
		t.Fatalf("before any snapshot: %+v", status)
	}
	if _, err := r.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Advance(2 * time.Hour)
	if status := r.Status(); status.Stale {
		t.Fatalf("stale at exactly twice the interval: %+v", status)
	}
	c.Advance(time.Second)
	if status := r.Status(); !status.Stale {
		t.Fatalf("not stale after twice the interval: %+v", status)
	}
}

func TestRunSnapshotsOnEveryTick(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	// Each call returns a new second so snapshot names never collide.
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	tick := func() time.Time { c.Advance(time.Second); return c.Now() }
	r, err := New(s, Config{Dir: dir, Every: 10 * time.Millisecond, Keep: 14}, &bytes.Buffer{}, tick)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; ; i++ {
		write(t, s, "tick")
		if names, _ := List(dir); len(names) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not write snapshots on its interval")
		}
		time.Sleep(15 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRunSnapshotsWhenWallClockPassesIntervalDuringSleep(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	r, _ := newRunner(t, s, dir, 14, c)
	r.check = 5 * time.Millisecond
	write(t, s, "before sleep")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	if names, _ := List(dir); len(names) != 0 {
		t.Fatalf("snapshot written before the interval passed: %v", names)
	}
	// A sleeping machine pauses monotonic timers but not the wall clock.
	c.Advance(time.Hour)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if names, _ := List(dir); len(names) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not snapshot after the wall clock passed the interval")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSnapshotAfterClockMovesBackwardsStaysNewest(t *testing.T) {
	s := openStore(t)
	dir := t.TempDir()
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	r, _ := newRunner(t, s, dir, 2, c)
	for i := 0; i < 2; i++ {
		write(t, s, "forward")
		if _, err := r.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Hour)
	}
	c.Advance(-2 * time.Hour) // 11:00
	write(t, s, "after clock moved back")
	if written, err := r.Snapshot(context.Background()); err != nil || !written {
		t.Fatalf("snapshot: %t %v", written, err)
	}
	status := r.Status()
	if status.LastSuccess == nil || status.LastSuccess.File != "herma-20260930T130001Z.sqlite3" {
		t.Fatalf("status: %+v", status)
	}
	if _, err := os.Stat(filepath.Join(dir, status.LastSuccess.File)); err != nil {
		t.Fatalf("latest snapshot was pruned: %v", err)
	}
	names, _ := List(dir)
	if len(names) != 2 || names[0] != "herma-20260930T130001Z.sqlite3" {
		t.Fatalf("names: %v", names)
	}
}

func TestIdleStoreIsNotStale(t *testing.T) {
	s := openStore(t)
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	r, _ := newRunner(t, s, t.TempDir(), 14, c)
	write(t, s, "only write")
	if _, err := r.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Advance(3 * time.Hour)
	if status := r.Status(); !status.Stale {
		t.Fatalf("expected stale before the check: %+v", status)
	}
	if written, err := r.Snapshot(context.Background()); err != nil || written {
		t.Fatalf("idle snapshot: %t %v", written, err)
	}
	status := r.Status()
	if status.Stale || status.LastSuccess == nil || status.LastSuccess.File != "herma-20260930T120000Z.sqlite3" {
		t.Fatalf("idle status: %+v", status)
	}
}
