// Package backup writes verified database snapshots into a directory that the
// user's sync tool copies off the machine, and restores them.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/moncho/herma/internal/store"
)

// TimeLayout is the UTC timestamp inside snapshot names.
const TimeLayout = "20060102T150405Z"

var snapshotPattern = regexp.MustCompile(`^herma-(\d{8}T\d{6}Z)\.sqlite3$`)

func SnapshotName(t time.Time) string { return "herma-" + t.UTC().Format(TimeLayout) + ".sqlite3" }

func SnapshotTime(name string) (time.Time, bool) {
	match := snapshotPattern.FindStringSubmatch(name)
	if match == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(TimeLayout, match[1])
	return t, err == nil
}

// List returns snapshot file names in dir, newest first. Sync conflict copies,
// placeholders and partial files do not match and are never returned.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && snapshotPattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// newestValid returns the newest snapshot in dir that passes validation and
// the names it skipped because they were invalid.
func newestValid(dir string) (string, store.SnapshotInfo, []string, error) {
	names, err := List(dir)
	if err != nil {
		return "", store.SnapshotInfo{}, nil, err
	}
	var skipped []string
	for _, name := range names {
		info, err := store.CheckSnapshot(filepath.Join(dir, name))
		if err == nil {
			return name, info, skipped, nil
		}
		skipped = append(skipped, name)
	}
	return "", store.SnapshotInfo{}, skipped, fmt.Errorf("no valid herma snapshot in %s", dir)
}

type Config struct {
	Dir   string
	Every time.Duration
	Keep  int
}

type Success struct {
	At   time.Time `json:"at"`
	File string    `json:"file"`
}

type Failure struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

type Status struct {
	Enabled     bool     `json:"enabled"`
	Dir         string   `json:"dir"`
	Every       string   `json:"every"`
	Keep        int      `json:"keep"`
	LastSuccess *Success `json:"last_success,omitempty"`
	LastError   *Failure `json:"last_error,omitempty"`
	Stale       bool     `json:"stale"`
}

// Runner takes snapshots of one store. Snapshot calls are serialized; Status
// never waits for a snapshot in progress.
type Runner struct {
	store *store.Store
	cfg   Config
	log   io.Writer
	now   func() time.Time
	check time.Duration // how often Run compares the wall clock with the interval

	snapshotMu sync.Mutex
	stateMu    sync.Mutex
	haveMarker bool
	marker     int64
	success    *Success
	confirmed  time.Time // last time an unchanged store was found current
	failure    *Failure
}

// New validates the configuration and resumes from the newest valid snapshot
// already in the directory.
func New(s *store.Store, cfg Config, log io.Writer, now func() time.Time) (*Runner, error) {
	if cfg.Every <= 0 {
		return nil, errors.New("backup interval must be positive")
	}
	if cfg.Keep < 1 {
		return nil, errors.New("backup keep count must be at least 1")
	}
	if err := CheckDir(cfg.Dir); err != nil {
		return nil, err
	}
	r := &Runner{store: s, cfg: cfg, log: log, now: now, check: min(cfg.Every, time.Minute)}
	name, snapshot, skipped, err := newestValid(cfg.Dir)
	for _, bad := range skipped {
		fmt.Fprintf(log, "herma: backup ignored invalid snapshot %s\n", bad)
	}
	if err == nil {
		live, err := s.RevisionCount(context.Background())
		if err != nil {
			return nil, fmt.Errorf("count revisions: %w", err)
		}
		if live < snapshot.Revisions {
			return nil, fmt.Errorf("backup folder %s holds a snapshot with %d revisions but the database has %d; run herma restore %s before serving, or use a different --backup-dir", cfg.Dir, snapshot.Revisions, live, cfg.Dir)
		}
		at, _ := SnapshotTime(name)
		r.haveMarker, r.marker = true, snapshot.Revisions
		r.success = &Success{At: at, File: name}
	}
	return r, nil
}

// Snapshot writes one snapshot unless nothing changed since the last one. It
// returns true when it wrote.
func (r *Runner) Snapshot(ctx context.Context) (bool, error) {
	r.snapshotMu.Lock()
	defer r.snapshotMu.Unlock()
	marker, err := r.store.RevisionCount(ctx)
	if err != nil {
		return false, r.fail(err)
	}
	r.stateMu.Lock()
	unchanged := r.haveMarker && marker == r.marker
	if unchanged {
		// A synced folder can lose the file; an idle store is only current
		// while its snapshot still exists.
		unchanged = false
		if r.success != nil {
			_, statErr := os.Lstat(filepath.Join(r.cfg.Dir, r.success.File))
			unchanged = statErr == nil
		}
	}
	r.stateMu.Unlock()
	if unchanged {
		r.stateMu.Lock()
		r.confirmed = r.now().UTC()
		r.failure = nil
		r.stateMu.Unlock()
		fmt.Fprintf(r.log, "herma: backup skipped: no changes since the last snapshot\n")
		return false, nil
	}
	// Names have one-second resolution. Moving forward past existing names keeps
	// them in time order and never drops a snapshot, such as the final one on
	// a quick restart.
	// It also starts after the newest existing name so a clock that moved
	// backwards cannot produce a snapshot that sorts oldest and is pruned.
	at := r.now().UTC().Truncate(time.Second)
	if names, err := List(r.cfg.Dir); err == nil && len(names) > 0 {
		if newest, ok := SnapshotTime(names[0]); ok && !newest.Add(time.Second).Before(at) {
			at = newest.Add(time.Second)
		}
	}
	for {
		if _, err := os.Lstat(filepath.Join(r.cfg.Dir, SnapshotName(at))); err != nil {
			break
		}
		at = at.Add(time.Second)
	}
	name := SnapshotName(at)
	final := filepath.Join(r.cfg.Dir, name)
	partial := filepath.Join(r.cfg.Dir, "."+name+".partial")
	_ = os.Remove(partial)
	info, size, err := r.write(ctx, partial, final)
	if err != nil {
		_ = os.Remove(partial)
		return false, r.fail(err)
	}
	r.stateMu.Lock()
	r.haveMarker, r.marker = true, info.Revisions
	r.success = &Success{At: at, File: name}
	r.failure = nil
	r.stateMu.Unlock()
	fmt.Fprintf(r.log, "herma: backup written %s (%d bytes, %d revisions)\n", name, size, info.Revisions)
	if err := r.prune(); err != nil {
		fmt.Fprintf(r.log, "herma: backup prune failed: %v\n", err)
	}
	return true, nil
}

func (r *Runner) write(ctx context.Context, partial, final string) (store.SnapshotInfo, int64, error) {
	if err := r.store.Snapshot(ctx, partial); err != nil {
		return store.SnapshotInfo{}, 0, err
	}
	info, err := store.CheckSnapshot(partial)
	if err != nil {
		return store.SnapshotInfo{}, 0, err
	}
	if err := os.Chmod(partial, 0600); err != nil {
		return store.SnapshotInfo{}, 0, err
	}
	f, err := os.Open(partial)
	if err != nil {
		return store.SnapshotInfo{}, 0, err
	}
	stat, statErr := f.Stat()
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(statErr, syncErr, closeErr); err != nil {
		return store.SnapshotInfo{}, 0, err
	}
	if err := os.Rename(partial, final); err != nil {
		return store.SnapshotInfo{}, 0, err
	}
	syncDir(r.cfg.Dir)
	return info, stat.Size(), nil
}

// syncDir makes the rename durable where the platform supports it.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

func (r *Runner) fail(err error) error {
	r.stateMu.Lock()
	r.failure = &Failure{At: r.now().UTC(), Message: err.Error()}
	r.stateMu.Unlock()
	fmt.Fprintf(r.log, "herma: backup failed: %v\n", err)
	return err
}

func (r *Runner) prune() error {
	names, err := List(r.cfg.Dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range names[min(len(names), r.cfg.Keep):] {
		if err := os.Remove(filepath.Join(r.cfg.Dir, name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Run takes a snapshot each time the interval passes on the wall clock, until
// ctx ends. Timers pause while macOS sleeps, so Run checks the wall clock
// often instead of waiting one interval. Failures are logged and recorded by
// Snapshot; the next interval retries.
func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(r.check)
	defer ticker.Stop()
	// UTC strips the monotonic reading, so Sub measures wall-clock time.
	last := r.now().UTC()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if now := r.now().UTC(); now.Sub(last) >= r.cfg.Every {
				last = now
				_, _ = r.Snapshot(ctx)
			}
		}
	}
}

func (r *Runner) Status() Status {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	status := Status{Enabled: true, Dir: r.cfg.Dir, Every: r.cfg.Every.String(), Keep: r.cfg.Keep}
	if r.success != nil {
		success := *r.success
		status.LastSuccess = &success
	}
	if r.failure != nil {
		failure := *r.failure
		status.LastError = &failure
	}
	if r.success == nil {
		status.Stale = true
	} else {
		fresh := r.success.At
		if r.confirmed.After(fresh) {
			fresh = r.confirmed
		}
		status.Stale = r.now().Sub(fresh) > 2*r.cfg.Every
	}
	return status
}

// CheckDir reports whether dir exists, is a directory and is writable.
func CheckDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("backup directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("backup directory %s is not a directory", dir)
	}
	probe, err := os.CreateTemp(dir, ".herma-write-check-*")
	if err != nil {
		return fmt.Errorf("backup directory %s is not writable: %w", dir, err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return nil
}
