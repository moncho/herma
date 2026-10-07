package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/moncho/herma/internal/store"
)

type identitySpec struct {
	name string
	role store.Role
}

func runCLI(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), args, &stdout, &stderr)
	return stdout.Bytes(), err
}

// shortDir returns a directory short enough for a Unix socket path on macOS,
// where t.TempDir paths can exceed the 104-byte limit.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "herma")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

// startServeWith runs herma serve with fresh credentials and extra serve flags.
// It returns the credentials path, the TCP URL and an idempotent stop function
// that also runs at cleanup. HERMA_CREDENTIALS and HERMA_URL point at the server.
func startServeWith(t *testing.T, serveArgs []string, extra ...identitySpec) (string, string, func()) {
	t.Helper()
	cleanEnv(t)
	dir := shortDir(t)
	credentials := filepath.Join(dir, "credentials.json")
	if err := initCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	for _, spec := range extra {
		if err := addIdentity(credentials, spec.name, spec.role, ""); err != nil {
			t.Fatal(err)
		}
	}
	address := freeAddress(t)
	endpoint := "http://" + address
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	args := append([]string{"--credentials", credentials, "serve", "--db", filepath.Join(dir, "herma.sqlite3"), "--listen", address}, serveArgs...)
	go func() { done <- Run(ctx, args, io.Discard, io.Discard) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("serve: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	waitForServe(t, endpoint, filepath.Join(dir, "herma.sock"))
	t.Setenv("HERMA_CREDENTIALS", credentials)
	t.Setenv("HERMA_URL", endpoint)
	return credentials, endpoint, stop
}

// waitForServe returns once herma serve answers on endpoint and has created socket.
func waitForServe(t *testing.T, endpoint, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get(endpoint + "/health")
		if _, socketErr := os.Stat(socket); err == nil && socketErr == nil {
			response.Body.Close()
			return
		} else if err == nil {
			response.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("herma serve did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func startServe(t *testing.T, extra ...identitySpec) (string, string) {
	t.Helper()
	credentials, endpoint, _ := startServeWith(t, nil, extra...)
	return credentials, endpoint
}

func TestRequireLoopback(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8765", "127.0.0.2:0", "[::1]:8765", "localhost:8765"} {
		if err := requireLoopback(address); err != nil {
			t.Errorf("%s: %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:8765", "[::]:8765", ":8765", "192.0.2.10:8765", "example.com:8765"} {
		if err := requireLoopback(address); err == nil || !strings.Contains(err.Error(), "tunnel") {
			t.Errorf("%s: want a loopback error, got %v", address, err)
		}
	}
}

func TestListenSocketHandlesExistingPaths(t *testing.T) {
	dir := shortDir(t)
	dead := filepath.Join(dir, "dead.sock")
	stale, err := net.Listen("unix", dead)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	listener, err := listenSocket(dead)
	if err != nil {
		t.Fatalf("stale socket was not replaced: %v", err)
	}
	defer listener.Close()
	info, err := os.Stat(dead)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket mode: %v %v", info, err)
	}
	if _, err := listenSocket(dead); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("live socket: %v", err)
	}
	regular := filepath.Join(dir, "file.sock")
	if err := os.WriteFile(regular, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenSocket(regular); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("regular file: %v", err)
	}
	long := filepath.Join(dir, strings.Repeat("d", 120), "herma.sock")
	if _, err := listenSocket(long); err == nil || !strings.Contains(err.Error(), "--socket") {
		t.Fatalf("long path: %v", err)
	}
}

func TestServeRefusesNonLoopbackListen(t *testing.T) {
	cleanEnv(t)
	dir := shortDir(t)
	credentials := filepath.Join(dir, "credentials.json")
	if err := initCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, "--credentials", credentials, "serve", "--db", filepath.Join(dir, "herma.sqlite3"), "--listen", "0.0.0.0:0")
	if err == nil || !strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("serve on all interfaces: %v", err)
	}
}

func TestReviewerCommandsUseSocketAndAgentsUseTCP(t *testing.T) {
	startServe(t)
	for identity, want := range map[string]string{"owner": "socket", "local-agent": "tcp"} {
		data, err := runCLI(t, "--identity", identity, "whoami")
		if err != nil {
			t.Fatalf("%s whoami: %v", identity, err)
		}
		var got map[string]string
		if err := json.Unmarshal(data, &got); err != nil || got["listener"] != want || got["identity"] != identity {
			t.Fatalf("%s whoami = %s (%v), want listener %s", identity, data, err, want)
		}
	}
}

func TestReviewerCommandReportsStoppedServer(t *testing.T) {
	cleanEnv(t)
	credentials := filepath.Join(shortDir(t), "credentials.json")
	if err := initCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, "--credentials", credentials, "--identity", "owner", "whoami")
	socket := filepath.Join(filepath.Dir(credentials), "herma.sock")
	if err == nil || !strings.Contains(err.Error(), "not listening on socket "+socket) {
		t.Fatalf("stopped server: %v", err)
	}
}

func TestServeReportsAddressInUse(t *testing.T) {
	cleanEnv(t)
	dir := shortDir(t)
	credentials := filepath.Join(dir, "credentials.json")
	if err := initCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	_, err = runCLI(t, "--credentials", credentials, "serve", "--db", filepath.Join(dir, "herma.sqlite3"), "--listen", occupied.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "herma serve left running") {
		t.Fatalf("want an already-running herma serve error, got %v", err)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("underlying error not wrapped: %v", err)
	}
}
