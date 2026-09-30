package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/moncho/herma/internal/backup"
	"github.com/moncho/herma/internal/store"
)

func backupCommand(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "status" {
		return errors.New("usage: herma [global flags] backup status")
	}
	if err := noOptions("backup status", args[1:], stderr); err != nil {
		return err
	}
	return cfg.request(ctx, stdout, http.MethodGet, "/v1/backup", nil, nil, "")
}

// restore works on files directly, so the service must be stopped: replacing
// a database that a running server holds open would corrupt it.
func restore(cfg config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: herma [global flags] restore SNAPSHOT|DIR [--db PATH] [--replace]")
	}
	source := args[0]
	fs := flags("restore", stderr)
	dbPath := fs.String("db", ".herma/knowledge.sqlite3", "database path to restore into")
	replace := fs.Bool("replace", false, "move an existing database aside instead of refusing")
	if err := parse(fs, args[1:]); err != nil {
		return err
	}
	socket, err := cfg.socketPath()
	if err != nil {
		return err
	}
	if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
		_ = conn.Close()
		return fmt.Errorf("a herma server is running on %s; stop it before restoring", socket)
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	snapshot := source
	var skipped []string
	if info.IsDir() {
		if snapshot, _, skipped, err = backup.Newest(source); err != nil {
			return err
		}
	} else if _, err := store.CheckSnapshot(source); err != nil {
		return err
	}
	result, err := backup.Install(snapshot, *dbPath, *replace, time.Now())
	if err != nil {
		if len(result.MovedAside) > 0 {
			// Install's renames are not atomic; name every file already moved
			// so the user can put them back by hand.
			return fmt.Errorf("%w; already moved aside: %s", err, strings.Join(result.MovedAside, ", "))
		}
		return err
	}
	return output(stdout, struct {
		backup.InstallResult
		Skipped []string `json:"skipped,omitempty"`
	}{result, skipped})
}
