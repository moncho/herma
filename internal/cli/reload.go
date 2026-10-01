package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// credentialsCheckInterval is how often herma serve checks the credentials file.
// Tests shorten it.
var credentialsCheckInterval = 2 * time.Second

// watchCredentials applies credential changes to a running server. It reloads
// every file in paths on each signal from hup and whenever any file's identity,
// size or modification time changes from its baseline, which the caller stats
// before starting the watcher so an edit in between is not missed. An invalid
// file, or a merged set without a reviewer, is reported and never applied.
func watchCredentials(ctx context.Context, paths []string, baselines []os.FileInfo, interval time.Duration, hup <-chan os.Signal, apply func(map[string]credential) error, log io.Writer) {
	last := append([]os.FileInfo(nil), baselines...)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		forced := false
		select {
		case <-ctx.Done():
			return
		case <-hup:
			forced = true
		case <-ticker.C:
		}
		changed := false
		for i, path := range paths {
			info, err := os.Stat(path)
			unchanged := err == nil && last[i] != nil && os.SameFile(info, last[i]) && info.Size() == last[i].Size() && info.ModTime().Equal(last[i].ModTime())
			stillMissing := err != nil && last[i] == nil
			if !unchanged && !stillMissing {
				changed = true
			}
			last[i] = info
		}
		if !forced && !changed {
			continue
		}
		credentials, err := loadServerIdentities(paths)
		if err == nil {
			err = apply(credentials)
		}
		if err != nil {
			fmt.Fprintf(log, "herma: credentials not reloaded: %v; the previous identities remain in effect, including any this change removes\n", err)
			continue
		}
		fmt.Fprintf(log, "herma: reloaded %d identities from %s\n", len(credentials), strings.Join(paths, " and "))
	}
}

// lockedWriter serializes log lines from the server and the credentials watcher.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
