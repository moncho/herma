package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/moncho/herma/internal/client"
)

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startWatcher(t *testing.T, path string) (chan map[string]credential, chan os.Signal, *safeBuffer) {
	t.Helper()
	applied := make(chan map[string]credential, 8)
	hup := make(chan os.Signal, 1)
	log := &safeBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	baseline, _ := os.Stat(path)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchCredentials(ctx, []string{path}, []os.FileInfo{baseline}, 10*time.Millisecond, hup, func(c map[string]credential) error { applied <- c; return nil }, log)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return applied, hup, log
}

func TestWatchCredentialsAppliesChangesAndSignals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := initCredentials(path); err != nil {
		t.Fatal(err)
	}
	applied, hup, _ := startWatcher(t, path)
	if _, err := revokeIdentity(path, "local-agent"); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-applied:
		if _, ok := c["local-agent"]; ok {
			t.Fatal("reload kept the revoked identity")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("file change was not applied")
	}
	hup <- syscall.SIGHUP
	select {
	case <-applied:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGHUP did not reload")
	}
}

func TestWatchCredentialsKeepsPreviousOnInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := initCredentials(path); err != nil {
		t.Fatal(err)
	}
	applied, _, log := startWatcher(t, path)
	if err := os.WriteFile(path, []byte(`{"owner":`), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(log.String(), "previous identities remain in effect") {
		if time.Now().After(deadline) {
			t.Fatalf("invalid file was not reported: %q", log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case c := <-applied:
		t.Fatalf("invalid file was applied: %+v", c)
	default:
	}
}

func TestServeStopsAcceptingRevokedToken(t *testing.T) {
	previous := credentialsCheckInterval
	credentialsCheckInterval = 20 * time.Millisecond
	t.Cleanup(func() { credentialsCheckInterval = previous })
	credentials, endpoint := startServe(t)
	loaded, err := loadCredentials(credentials)
	if err != nil {
		t.Fatal(err)
	}
	agentClient, err := client.New(endpoint, loaded["local-agent"].Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentClient.Do(context.Background(), "GET", "/v1/whoami", nil, nil, ""); err != nil {
		t.Fatalf("before revoke: %v", err)
	}
	if _, err := runCLI(t, "identity", "revoke", "local-agent"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := agentClient.Do(context.Background(), "GET", "/v1/whoami", nil, nil, "")
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 401 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("revoked token still accepted: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
