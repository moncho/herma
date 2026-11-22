package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moncho/herma/internal/store"
)

// watcher is a running herma watch: changes arrive on lines, and stop ends
// it and returns its error and what it wrote to stderr.
type watcher struct {
	lines  chan store.Change
	stop   func() (error, string)
	stderr *safeBuffer
}

func startWatch(t *testing.T, args ...string) *watcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	w := &watcher{lines: make(chan store.Change, 16), stderr: &safeBuffer{}}
	done := make(chan error, 1)
	go func() {
		err := RunWithInput(ctx, append([]string{"watch"}, args...), strings.NewReader(""), writer, w.stderr)
		writer.Close()
		done <- err
	}()
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			var c store.Change
			if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
				t.Errorf("watch printed a line that is not a change: %q", scanner.Text())
				continue
			}
			w.lines <- c
		}
	}()
	var once sync.Once
	var result error
	w.stop = func() (error, string) {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(5 * time.Second):
				t.Error("herma watch did not stop when cancelled")
			}
			reader.Close()
		})
		return result, w.stderr.String()
	}
	t.Cleanup(func() { w.stop() })
	return w
}

func (w *watcher) next(t *testing.T, what string) store.Change {
	t.Helper()
	select {
	case c := <-w.lines:
		return c
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: herma watch printed nothing within 5s (stderr: %s)", what, w.stderr.String())
		return store.Change{}
	}
}

func (w *watcher) quiet(t *testing.T, what string) {
	t.Helper()
	select {
	case c := <-w.lines:
		t.Fatalf("%s: herma watch printed %+v", what, c)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWatchReplaysFromACursorThenFollowsNewWrites(t *testing.T) {
	startServe(t)
	t.Chdir(t.TempDir())
	first := createAs(t, "local-agent", "--kind", "task", "--title", "First")
	createAs(t, "local-agent", "--kind", "note", "--title", "A note", "--body", "b")
	w := startWatch(t, "--after", "0", "--kind", "task")
	if got := w.next(t, "history"); got.Seq != 1 || got.Record.ID != first.ID || got.Action != "created" || got.Actor != "local-agent" {
		t.Fatalf("replayed change: %+v", got)
	}
	w.quiet(t, "the note is not a task")
	second := createAs(t, "local-agent", "--kind", "task", "--title", "Second")
	if got := w.next(t, "a new task"); got.Seq != 3 || got.Record.ID != second.ID {
		t.Fatalf("followed change: %+v, want change 3 for %s", got, second.ID)
	}
	if _, err := runCLI(t, "update", second.ID, "--version", "1", "--status", "done"); err != nil {
		t.Fatal(err)
	}
	if got := w.next(t, "an update"); got.Seq != 4 || got.Action != "updated" || got.Record.Status != "done" || got.Record.Version != 2 {
		t.Fatalf("update change: %+v", got)
	}
	if err, stderr := w.stop(); err != nil || stderr != "" {
		t.Errorf("stopping watch: %v, stderr %q", err, stderr)
	}
}

func TestWatchStartsFromNowAndFollowsTheBinding(t *testing.T) {
	startServe(t)
	here := createAs(t, "local-agent", "--kind", "project", "--title", "Here")
	elsewhere := createAs(t, "local-agent", "--kind", "project", "--title", "Elsewhere")
	createAs(t, "local-agent", "--kind", "task", "--title", "Before watching", "--project", here.ID)
	t.Chdir(boundCheckout(t, here.ID))
	w := startWatch(t)
	w.quiet(t, "history is not replayed without --after")
	createAs(t, "local-agent", "--kind", "task", "--title", "Theirs", "--project", elsewhere.ID)
	w.quiet(t, "another project's task")
	mine := createAs(t, "local-agent", "--kind", "task", "--title", "Mine")
	if got := w.next(t, "a task in the bound project"); got.Record.ID != mine.ID || got.Record.ProjectID != here.ID {
		t.Fatalf("change: %+v, want %s", got, mine.ID)
	}
}

func TestWatchRetriesWhileTheServerIsAwayAndExitsOnARefusal(t *testing.T) {
	watchRetry = 20 * time.Millisecond
	t.Cleanup(func() { watchRetry = time.Second })
	_, _, stopServer := startServeWith(t, nil)
	t.Chdir(t.TempDir())
	// Bounded, so a watch that wrongly retried a refusal fails here instead
	// of running forever.
	bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var refused safeBuffer
	if err := Run(bounded, []string{"watch", "--kind", "Not-A-Kind"}, io.Discard, &refused); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("a refused request: %v (stderr %q), want the server's HTTP 400", err, refused.String())
	}
	if _, err := runCLI(t, "watch", "extra"); err == nil {
		t.Error("a positional argument was accepted")
	}

	w := startWatch(t, "--after", "0")
	// A server shutting down ends the wait it is holding; stopServer fails
	// the test if shutdown had to give up on an open request.
	time.Sleep(200 * time.Millisecond)
	began := time.Now()
	stopServer()
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("the server took %v to stop with a watcher waiting", took)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(w.stderr.String(), "retrying") {
		if time.Now().After(deadline) {
			t.Fatalf("watch did not report a retry; stderr: %q", w.stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err, _ := w.stop(); err != nil {
		t.Errorf("stopping a retrying watch: %v", err)
	}
}
