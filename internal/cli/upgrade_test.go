package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/version"
)

// fakeRelease serves one release whose herma binary is a shell script that
// answers `version` as tag, and installs a stand-in for the running binary.
// It returns that binary's path.
func fakeRelease(t *testing.T, tag, reports string, damage bool) string {
	t.Helper()
	cleanEnv(t)
	script := "#!/bin/sh\necho \"herma " + reports + "\"\n"
	var archive bytes.Buffer
	zipped := gzip.NewWriter(&archive)
	files := tar.NewWriter(zipped)
	if err := files.WriteHeader(&tar.Header{Name: "herma", Mode: 0755, Size: int64(len(script)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("herma_%s_%s_%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(archive.Bytes())
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n"
	body := archive.Bytes()
	if damage {
		body = append([]byte{}, body...)
		body[len(body)-1] ^= 0xff
	}
	prefix := "/" + releaseRepository + "/releases/download/" + tag + "/"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + releaseRepository + "/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
		case prefix + "SHA256SUMS":
			_, _ = w.Write([]byte(sums))
		case prefix + name:
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	target := filepath.Join(t.TempDir(), "herma")
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho old\n"), 0755); err != nil {
		t.Fatal(err)
	}
	oldAPI, oldDownloads, oldTarget := releaseAPI, releaseDownloads, upgradeTarget
	releaseAPI, releaseDownloads, upgradeTarget = server.URL, server.URL, func() (string, error) { return target, nil }
	t.Cleanup(func() { releaseAPI, releaseDownloads, upgradeTarget = oldAPI, oldDownloads, oldTarget })
	return target
}

func upgradeResult(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	data, err := runCLI(t, append([]string{"upgrade"}, args...)...)
	result := map[string]any{}
	if len(data) > 0 {
		if jsonErr := json.Unmarshal(data, &result); jsonErr != nil {
			t.Fatalf("upgrade output %s: %v", data, jsonErr)
		}
	}
	return result, err
}

func TestUpgradeInstallsTheNewestRelease(t *testing.T) {
	target := fakeRelease(t, "v9.9.9", "v9.9.9", false)
	result, err := upgradeResult(t)
	if err != nil || result["upgraded"] != true || result["release"] != "v9.9.9" || result["installed"] != version.Version || result["binary"] != target {
		t.Fatalf("%v %v", result, err)
	}
	data, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(data), "herma v9.9.9") {
		t.Fatalf("binary not replaced: %q %v", data, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("mode: %v %v", info, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".herma-upgrade-*")); len(leftovers) != 0 {
		t.Fatalf("staged files left behind: %v", leftovers)
	}
}

func TestUpgradeCheckChangesNothing(t *testing.T) {
	target := fakeRelease(t, "v9.9.9", "v9.9.9", false)
	result, err := upgradeResult(t, "--check")
	if err != nil || result["upgraded"] != false || result["release"] != "v9.9.9" {
		t.Fatalf("%v %v", result, err)
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), "echo old") {
		t.Fatalf("--check replaced the binary: %q", data)
	}
}

func TestUpgradeLeavesTheBinaryWhenTheReleaseIsNotUsable(t *testing.T) {
	for name, tc := range map[string]struct {
		reports string
		damage  bool
		args    []string
		want    string
	}{
		"checksum mismatch":       {"v9.9.9", true, nil, "does not match its checksum"},
		"binary is another build": {"v1.0.0", false, nil, "does not run as herma v9.9.9"},
		"release does not exist":  {"v9.9.9", false, []string{"--version", "v8.0.0"}, "checksums of v8.0.0"},
		"not a release tag":       {"v9.9.9", false, []string{"--version", "../v9.9.9"}, "not a release tag"},
	} {
		t.Run(name, func(t *testing.T) {
			target := fakeRelease(t, "v9.9.9", tc.reports, tc.damage)
			_, err := upgradeResult(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if data, _ := os.ReadFile(target); !strings.Contains(string(data), "echo old") {
				t.Fatalf("binary changed: %q", data)
			}
		})
	}
}

func TestUpgradeDoesNothingWhenAlreadyCurrent(t *testing.T) {
	target := fakeRelease(t, "v9.9.9", "v9.9.9", false)
	old := version.Version
	version.Version = "v9.9.9"
	t.Cleanup(func() { version.Version = old })
	result, err := upgradeResult(t)
	if err != nil || result["upgraded"] != false {
		t.Fatalf("%v %v", result, err)
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), "echo old") {
		t.Fatalf("binary changed: %q", data)
	}
}

func TestUpgradePluginRewritesOnlyAnInstalledPlugin(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	t.Setenv("HERMA_DIR", dir)
	if result, err := upgradeResult(t, "--plugin"); err != nil || result["plugin_refreshed"] != false {
		t.Fatalf("without a plugin: %v %v", result, err)
	}
	manifest := filepath.Join(dir, "plugins", "claude", ".claude-plugin", "plugin.json")
	if err := os.MkdirAll(filepath.Dir(manifest), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"name":"herma","version":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := upgradeResult(t, "--plugin"); err != nil || result["plugin_refreshed"] != true {
		t.Fatalf("with a plugin: %v %v", result, err)
	}
	if data, _ := os.ReadFile(manifest); strings.Contains(string(data), `"old"`) {
		t.Fatalf("plugin not rewritten: %s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "claude", "hooks", "status.ts")); err != nil {
		t.Fatalf("built-in plugin files missing: %v", err)
	}
}
