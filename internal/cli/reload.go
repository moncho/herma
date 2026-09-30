package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// credentialsCheckInterval is how often herma serve checks the credentials file.
// Tests shorten it.
var credentialsCheckInterval = 2 * time.Second

// watchCredentials applies credential changes to a running server. It reloads
// on every signal from hup and whenever the file's identity, size or
// modification time changes from baseline, which the caller stats before
// starting the watcher so an edit in between is not missed. An invalid file is reported and never applied.
func watchCredentials(ctx context.Context, path string, baseline os.FileInfo, interval time.Duration, hup <-chan os.Signal, apply func(map[string]credential) error, log io.Writer) {
	last := baseline
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
		info, err := os.Stat(path)
		unchanged := err == nil && last != nil && os.SameFile(info, last) && info.Size() == last.Size() && info.ModTime().Equal(last.ModTime())
		stillMissing := err != nil && last == nil
		if !forced && (unchanged || stillMissing) {
			continue
		}
		last = info
		credentials, err := loadCredentials(path)
		if err == nil {
			err = apply(credentials)
		}
		if err != nil {
			fmt.Fprintf(log, "herma: credentials not reloaded: %v; the previous identities remain in effect, including any this change removes\n", err)
			continue
		}
		fmt.Fprintf(log, "herma: reloaded %d identities from %s\n", len(credentials), path)
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
