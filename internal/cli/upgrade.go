package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/moncho/herma/internal/hooks"
	"github.com/moncho/herma/internal/version"
	"github.com/moncho/herma/plugins"
)

// Releases are GitHub releases of this repository: one archive per platform
// holding the herma binary, and a SHA256SUMS file.
const (
	releaseRepository = "moncho/herma"
	maxChecksumBytes  = 64 << 10
	maxArchiveBytes   = 64 << 20
	maxBinaryBytes    = 256 << 20
	upgradeTimeout    = 2 * time.Minute
)

// Tests point these at a local server and a temporary file.
var (
	releaseAPI       = "https://api.github.com"
	releaseDownloads = "https://github.com"
	upgradeTarget    = runningBinary
)

var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// runningBinary is the file this process was started from, behind any symlink.
func runningBinary() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(executable)
}

// upgrade replaces the installed binary with a release build and rewrites
// the Claude Code plugin from it. It needs no server and no credentials.
func upgrade(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	fs := flags("upgrade", stderr)
	check := fs.Bool("check", false, "only report the installed and the newest release")
	wanted := fs.String("version", "", "release to install, such as v0.2.0; the newest when omitted")
	pluginOnly := fs.Bool("plugin", false, "only rewrite the installed Claude Code plugin from this binary")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *pluginOnly {
		if *check || *wanted != "" {
			return errors.New("upgrade --plugin takes no other option")
		}
		refreshed, err := refreshPlugin(cfg)
		if err != nil {
			return err
		}
		return output(stdout, map[string]any{"plugin_refreshed": refreshed})
	}
	if *wanted != "" && !releaseTag.MatchString(*wanted) {
		return fmt.Errorf("--version %q is not a release tag such as v0.2.0", *wanted)
	}
	ctx, cancel := context.WithTimeout(ctx, upgradeTimeout)
	defer cancel()
	tag := *wanted
	if tag == "" {
		var err error
		if tag, err = latestRelease(ctx); err != nil {
			return err
		}
	}
	result := map[string]any{"installed": version.Version, "release": tag, "upgraded": false}
	if *check || tag == version.Version {
		return output(stdout, result)
	}
	target, err := upgradeTarget()
	if err != nil {
		return fmt.Errorf("find the installed herma binary: %w", err)
	}
	binary, err := releaseBinary(ctx, tag)
	if err != nil {
		return err
	}
	if err := replaceBinary(ctx, target, binary, tag); err != nil {
		return err
	}
	result["upgraded"], result["binary"] = true, target
	// The plugin is built into the binary, so the new one has to write it.
	refresh := exec.CommandContext(ctx, target, "upgrade", "--plugin")
	refresh.Env = append(os.Environ(), "HERMA_DIR="+cfg.dir)
	if out, err := refresh.CombinedOutput(); err != nil {
		result["plugin_warning"] = "the Claude Code plugin was not refreshed (" + firstOutputLine(out, err) + "); rerun herma hook install --client claude"
	}
	result["message"] = "New sessions use " + tag + ". If this machine runs herma serve, restart it to serve the new version; herma status shows both."
	return output(stdout, result)
}

// refreshPlugin rewrites the plugin in the data folder when one is installed
// there, and reports whether it did.
func refreshPlugin(cfg config) (bool, error) {
	if _, err := os.Stat(filepath.Join(cfg.dir, "plugins", "claude", ".claude-plugin", "plugin.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if _, err := hooks.WritePlugin(cfg.dir, plugins.Claude()); err != nil {
		return false, err
	}
	return true, nil
}

func latestRelease(ctx context.Context) (string, error) {
	data, err := download(ctx, releaseAPI+"/repos/"+releaseRepository+"/releases/latest", maxChecksumBytes*4)
	if err != nil {
		return "", fmt.Errorf("find the newest release: %w", err)
	}
	var release struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(data, &release); err != nil || !releaseTag.MatchString(release.Tag) {
		return "", errors.New("find the newest release: GitHub returned no usable release tag; name one with --version")
	}
	return release.Tag, nil
}

// releaseBinary downloads the archive for this platform, checks it against
// the release's SHA256SUMS and returns the herma binary inside. The checksum
// catches a damaged download; it comes from the same release as the archive.
func releaseBinary(ctx context.Context, tag string) ([]byte, error) {
	name := fmt.Sprintf("herma_%s_%s_%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
	base := releaseDownloads + "/" + releaseRepository + "/releases/download/" + tag + "/"
	sums, err := download(ctx, base+"SHA256SUMS", maxChecksumBytes)
	if err != nil {
		return nil, fmt.Errorf("download the checksums of %s: %w", tag, err)
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if parts := strings.Fields(line); len(parts) == 2 && strings.TrimPrefix(parts[1], "*") == name {
			want = parts[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("release %s has no build for %s/%s; build from source with make install", tag, runtime.GOOS, runtime.GOARCH)
	}
	archive, err := download(ctx, base+name, maxArchiveBytes)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	if sum := sha256.Sum256(archive); !strings.EqualFold(hex.EncodeToString(sum[:]), want) {
		return nil, fmt.Errorf("%s does not match its checksum in SHA256SUMS; nothing was changed", name)
	}
	zipped, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	files := tar.NewReader(zipped)
	for {
		header, err := files.Next()
		if err != nil {
			return nil, fmt.Errorf("%s holds no herma binary", name)
		}
		if header.Name != "herma" || header.Typeflag != tar.TypeReg {
			continue
		}
		binary, err := io.ReadAll(io.LimitReader(files, maxBinaryBytes+1))
		if err != nil || len(binary) > maxBinaryBytes {
			return nil, fmt.Errorf("read herma from %s: too large or damaged", name)
		}
		return binary, nil
	}
}

// replaceBinary puts binary at target in one rename, after the staged file
// has answered `version` with the release it claims to be. A failure leaves
// the installed binary as it was.
func replaceBinary(ctx context.Context, target string, binary []byte, tag string) error {
	staged, err := os.CreateTemp(filepath.Dir(target), ".herma-upgrade-*")
	if err != nil {
		return fmt.Errorf("write next to %s: %w; rerun with permission to write that folder", target, err)
	}
	defer os.Remove(staged.Name())
	_, err = staged.Write(binary)
	if err == nil {
		err = staged.Chmod(0755)
	}
	if err == nil {
		err = staged.Sync()
	}
	if cerr := staged.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write the new binary: %w", err)
	}
	out, err := exec.CommandContext(ctx, staged.Name(), "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "herma "+tag {
		return fmt.Errorf("the downloaded binary does not run as herma %s on this machine (%s); nothing was changed", tag, firstOutputLine(out, err))
	}
	if err := os.Rename(staged.Name(), target); err != nil {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}

func firstOutputLine(out []byte, err error) string {
	if line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n"); line != "" {
		return line
	}
	if err != nil {
		return err.Error()
	}
	return "no output"
}

// download fetches a public release file. No herma credential is sent.
func download(ctx context.Context, address string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "herma-upgrade")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered HTTP %d", req.URL.Host, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}
