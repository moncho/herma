package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/moncho/herma/internal/api"
	"github.com/moncho/herma/internal/backup"
	"github.com/moncho/herma/internal/store"
)

// minBackupInterval keeps a misconfigured interval from rewriting the backup
// folder continuously.
const minBackupInterval = 5 * time.Minute

// maxSocketPath leaves room for the terminator in macOS's 104-byte sun_path.
const maxSocketPath = 103

// requireLoopback keeps the TCP listener local. Remote agents reach it through
// a tunnel or proxy, as docs/operations.md describes.
func requireLoopback(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", address, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("listen address %q is not loopback; herma listens only on 127.0.0.1, ::1 or localhost. Reach it from other machines through a tunnel or proxy such as Tailscale Serve", address)
}

// listenSocket opens the private reviewer socket. A leftover socket is removed
// only when nothing answers on it; any other file at the path is left alone.
func listenSocket(path string) (net.Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is %d bytes; Unix sockets allow at most %d. Pass --socket with a shorter path", path, len(path), maxSocketPath)
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&os.ModeSocket == 0:
		return nil, fmt.Errorf("%s exists and is not a socket; move it aside before starting herma serve", path)
	case err == nil:
		if conn, dialErr := net.DialTimeout("unix", path, time.Second); dialErr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("another herma server is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("inspect socket %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("make socket %s private: %w", path, err)
	}
	return listener, nil
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
}

// serverCredentialPaths lists the credentials files herma serve reads. The
// reviewer file is optional and must differ from the main one.
func serverCredentialPaths(credentials, reviewerCredentials string) ([]string, error) {
	if reviewerCredentials == "" {
		return []string{credentials}, nil
	}
	main, err := filepath.Abs(credentials)
	if err != nil {
		return nil, fmt.Errorf("resolve credentials path: %w", err)
	}
	reviewer, err := filepath.Abs(reviewerCredentials)
	if err != nil {
		return nil, fmt.Errorf("resolve reviewer credentials path: %w", err)
	}
	if main == reviewer {
		return nil, errors.New("--reviewer-credentials must name a different file from --credentials")
	}
	return []string{credentials, reviewerCredentials}, nil
}

func serve(ctx context.Context, cfg config, args []string, stderr io.Writer) error {
	fs := flags("serve", stderr)
	dbPath := fs.String("db", ".herma/knowledge.sqlite3", "SQLite database path")
	listen := fs.String("listen", "127.0.0.1:8765", "HTTP listen address (loopback only)")
	backupDir := fs.String("backup-dir", "", "write snapshots into this directory (off when empty)")
	backupEvery := fs.Duration("backup-every", 6*time.Hour, "interval between snapshots (minimum 5m)")
	backupKeep := fs.Int("backup-keep", 14, "number of snapshots to keep (minimum 1)")
	reviewerCredentials := fs.String("reviewer-credentials", envDefault("HERMA_REVIEWER_CREDENTIALS", ""), "second credentials file, typically holding reviewer identities kept apart from agents")
	if err := parse(fs, args); err != nil {
		return err
	}
	set := supplied(fs)
	if *backupDir == "" && (set["backup-every"] || set["backup-keep"]) {
		return errors.New("--backup-every and --backup-keep require --backup-dir")
	}
	if *backupDir != "" && *backupEvery < minBackupInterval {
		return fmt.Errorf("--backup-every must be at least %s", minBackupInterval)
	}
	if *backupDir != "" && *backupKeep < 1 {
		return errors.New("--backup-keep must be at least 1")
	}
	if *backupDir != "" {
		if err := backup.CheckDir(*backupDir); err != nil {
			return err
		}
	}
	stderr = &lockedWriter{w: stderr}
	if err := requireLoopback(*listen); err != nil {
		return err
	}
	socketPath, err := cfg.socketPath()
	if err != nil {
		return err
	}
	credentialPaths, err := serverCredentialPaths(cfg.credentials, *reviewerCredentials)
	if err != nil {
		return err
	}
	baselines := make([]os.FileInfo, len(credentialPaths))
	for i, path := range credentialPaths {
		baselines[i], _ = os.Stat(path)
	}
	identities, err := loadServerIdentities(credentialPaths)
	if err != nil {
		return err
	}
	if err := prepareDatabase(*dbPath); err != nil {
		return err
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open knowledge database: %w", err)
	}
	defer db.Close()
	handler := api.NewHandler(db, apiIdentities(identities))
	var backups *backup.Runner
	if *backupDir != "" {
		backups, err = backup.New(db, backup.Config{Dir: *backupDir, Every: *backupEvery, Keep: *backupKeep}, stderr, time.Now)
		if err != nil {
			return err
		}
		// A failed startup snapshot is logged and reported by status; serving
		// continues so the next interval can retry.
		_, _ = backups.Snapshot(ctx)
		handler.SetBackupReporter(backups)
	}
	tcpListener, err := net.Listen("tcp", *listen)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("address %s is already in use: another process, possibly a herma serve left running after its terminal closed, is using it: %w", *listen, err)
		}
		return fmt.Errorf("listen for knowledge base requests: %w", err)
	}
	socketListener, err := listenSocket(socketPath)
	if err != nil {
		_ = tcpListener.Close()
		return err
	}
	servers := []*http.Server{newHTTPServer(handler), newHTTPServer(handler.Socket())}
	listeners := []net.Listener{tcpListener, socketListener}
	finished := make(chan error, len(servers))
	for i, server := range servers {
		go func(server *http.Server, listener net.Listener) { finished <- server.Serve(listener) }(server, listeners[i])
	}
	fmt.Fprintf(stderr, "Knowledge base listening on %s and %s; database %s\n", tcpListener.Addr(), socketPath, *dbPath)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go watchCredentials(watchCtx, credentialPaths, baselines, credentialsCheckInterval, hup, func(credentials map[string]credential) error {
		return handler.SetIdentities(apiIdentities(credentials))
	}, stderr)
	backupCtx, stopBackups := context.WithCancel(ctx)
	defer stopBackups()
	backupsDone := make(chan struct{})
	if backups != nil {
		go func() { backups.Run(backupCtx); close(backupsDone) }()
	} else {
		close(backupsDone)
	}
	shutdown := func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var result error
		for _, server := range servers {
			if err := server.Shutdown(shutdownCtx); err != nil {
				_ = server.Close()
				result = fmt.Errorf("shut down server: %w", err)
			}
		}
		stopBackups()
		<-backupsDone
		if backups != nil {
			// The final snapshot captures writes since the last tick.
			_, _ = backups.Snapshot(context.Background())
		}
		return result
	}
	select {
	case err := <-finished:
		shutdownErr := shutdown()
		if errors.Is(err, http.ErrServerClosed) {
			return shutdownErr
		}
		return fmt.Errorf("serve knowledge base: %w", err)
	case <-ctx.Done():
		return shutdown()
	}
}
