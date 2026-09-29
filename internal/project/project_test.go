package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const projectA = "rec_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const projectB = "rec_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func makeRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return physical
}

func writeConfig(t *testing.T, dir, data string) string {
	t.Helper()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverNearestBindingFromPhysicalSessionDirectory(t *testing.T) {
	root := makeRepository(t)
	rootPath, err := Bind(root, Binding{ProjectID: projectA})
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "packages", "app")
	nested := filepath.Join(child, "internal", "feature")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	binding, path, err := Discover(nested)
	if err != nil || path != rootPath || binding != (Binding{ProjectID: projectA, MaxBytes: DefaultMaxBytes}) {
		t.Fatalf("nested root discovery: %+v %q %v", binding, path, err)
	}
	childPath, err := Bind(child, Binding{ProjectID: projectB, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	binding, path, err = Discover(nested)
	if err != nil || path != childPath || binding.ProjectID != projectB || binding.MaxBytes != 4096 {
		t.Fatalf("nearest binding: %+v %q %v", binding, path, err)
	}
	alias := filepath.Join(t.TempDir(), "session-directory")
	if err := os.Symlink(nested, alias); err != nil {
		t.Fatal(err)
	}
	binding, path, err = Discover(alias)
	if err != nil || path != childPath || binding.ProjectID != projectB {
		t.Fatalf("physical session directory: %+v %q %v", binding, path, err)
	}
}

func TestDiscoverStopsAtRepositoryAndWorktreeBoundaries(t *testing.T) {
	for _, gitEntry := range []string{"directory", "worktree-file"} {
		t.Run(gitEntry, func(t *testing.T) {
			outside := makeRepository(t)
			if _, err := Bind(outside, Binding{ProjectID: projectA}); err != nil {
				t.Fatal(err)
			}
			checkout := filepath.Join(outside, "nested-checkout")
			nested := filepath.Join(checkout, "src", "feature")
			if err := os.MkdirAll(nested, 0755); err != nil {
				t.Fatal(err)
			}
			gitPath := filepath.Join(checkout, ".git")
			if gitEntry == "directory" {
				if err := os.Mkdir(gitPath, 0755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(gitPath, []byte("gitdir: "+filepath.Join(outside, ".git", "worktrees", "example")+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			binding, path, err := Discover(nested)
			if !errors.Is(err, ErrNoBinding) || path != "" || binding != (Binding{}) {
				t.Fatalf("crossed %s boundary: %+v %q %v", gitEntry, binding, path, err)
			}
			config, err := Bind(checkout, Binding{ProjectID: projectB})
			if err != nil {
				t.Fatal(err)
			}
			binding, path, err = Discover(nested)
			if err != nil || path != config || binding.ProjectID != projectB {
				t.Fatalf("checkout-root binding not discovered: %+v %q %v", binding, path, err)
			}
		})
	}
}

func TestMalformedNearestBindingNeverFallsBackOrGetsOverwritten(t *testing.T) {
	validID := fmt.Sprintf(`"project_id":%q`, projectA)
	cases := map[string]string{
		"empty":              "",
		"malformed":          `{` + validID,
		"array":              `[]`,
		"null":               `null`,
		"missing project":    `{"max_bytes":4096}`,
		"duplicate project":  `{` + validID + `,` + validID + `}`,
		"duplicate budget":   `{` + validID + `,"max_bytes":4096,"max_bytes":8192}`,
		"multiple objects":   `{` + validID + `} {` + validID + `}`,
		"trailing garbage":   `{` + validID + `} invalid`,
		"token forbidden":    `{` + validID + `,"token":"do-not-echo-this-value"}`,
		"endpoint forbidden": `{` + validID + `,"endpoint":"https://example.com"}`,
		"field casing":       `{"PROJECT_ID":"` + projectA + `"}`,
		"invalid id":         `{"project_id":"../another-project"}`,
		"whitespace id":      `{"project_id":" ` + projectA + `"}`,
		"uppercase id":       `{"project_id":"rec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`,
		"null project":       `{"project_id":null}`,
		"project number":     `{"project_id":7}`,
		"budget zero":        `{` + validID + `,"max_bytes":0}`,
		"budget small":       `{` + validID + `,"max_bytes":2047}`,
		"budget large":       `{` + validID + `,"max_bytes":65537}`,
		"budget negative":    `{` + validID + `,"max_bytes":-1}`,
		"budget fraction":    `{` + validID + `,"max_bytes":2048.5}`,
		"budget string":      `{` + validID + `,"max_bytes":"4096"}`,
		"budget null":        `{` + validID + `,"max_bytes":null}`,
		"budget overflow":    `{` + validID + `,"max_bytes":99999999999999999999999999}`,
		"invalid UTF8":       "{\"project_id\":\"" + string([]byte{0xff}) + "\"}",
		"oversized":          strings.Repeat(" ", maxFileBytes+1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			root := makeRepository(t)
			if _, err := Bind(root, Binding{ProjectID: projectB}); err != nil {
				t.Fatal(err)
			}
			near := filepath.Join(root, "near")
			if err := os.Mkdir(near, 0755); err != nil {
				t.Fatal(err)
			}
			configPath := writeConfig(t, near, data)
			binding, path, err := Discover(near)
			if err == nil || errors.Is(err, ErrNoBinding) || binding != (Binding{}) || path != "" || !strings.Contains(err.Error(), configPath) {
				t.Fatalf("invalid binding did not fail closed: %+v %q %v", binding, path, err)
			}
			if strings.Contains(err.Error(), "do-not-echo-this-value") {
				t.Fatal("diagnostic exposed forbidden token content")
			}
			if _, err := Bind(near, Binding{ProjectID: projectA}); err == nil {
				t.Fatal("Bind overwrote invalid existing config")
			}
			unchanged, err := os.ReadFile(configPath)
			if err != nil || string(unchanged) != data {
				t.Fatalf("existing invalid file changed: %v", err)
			}
		})
	}
}

func TestDefaultsBoundsAndBindingIdempotency(t *testing.T) {
	for _, budget := range []int{0, MinMaxBytes, MaxMaxBytes} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			root := makeRepository(t)
			data := fmt.Sprintf("{\n  \"project_id\": %q", projectA)
			want := budget
			if budget == 0 {
				want = DefaultMaxBytes
			} else {
				data += fmt.Sprintf(",\n  \"max_bytes\": %d", budget)
			}
			data += "\n}\n"
			config := writeConfig(t, root, data)
			before, err := os.Stat(config)
			if err != nil {
				t.Fatal(err)
			}
			binding, path, err := Discover(root)
			if err != nil || path != config || binding.MaxBytes != want {
				t.Fatalf("valid budget: %+v %q %v", binding, path, err)
			}
			bound, err := Bind(root, Binding{ProjectID: projectA, MaxBytes: budget})
			if err != nil || bound != config {
				t.Fatalf("idempotent bind: %q %v", bound, err)
			}
			after, err := os.Stat(config)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("idempotent bind replaced the file: %v", err)
			}
			unchanged, err := os.ReadFile(config)
			if err != nil || string(unchanged) != data {
				t.Fatalf("idempotent bind rewrote formatting: %v", err)
			}
			for _, different := range []Binding{{ProjectID: projectB, MaxBytes: want}, {ProjectID: projectA, MaxBytes: want + 1}} {
				if different.MaxBytes > MaxMaxBytes {
					different.MaxBytes = MaxMaxBytes - 1
				}
				if _, err := Bind(root, different); !errors.Is(err, ErrBindingConflict) {
					t.Fatalf("different binding did not conflict: %v", err)
				}
			}
			unchanged, err = os.ReadFile(config)
			if err != nil || string(unchanged) != data {
				t.Fatalf("conflicting bind rewrote the file: %v", err)
			}
		})
	}
	root := makeRepository(t)
	data := fmt.Sprintf(`{"project_id":%q}`, projectA)
	writeConfig(t, root, data+strings.Repeat(" ", maxFileBytes-len(data)))
	if _, _, err := Discover(root); err != nil {
		t.Fatalf("exactly 64 KiB binding rejected: %v", err)
	}
}

func TestBindWritesOnlyNonsecretFieldsAndRejectsInvalidInput(t *testing.T) {
	root := makeRepository(t)
	for _, invalid := range []Binding{{}, {ProjectID: "invalid"}, {ProjectID: projectA, MaxBytes: -1}, {ProjectID: projectA, MaxBytes: MaxMaxBytes + 1}} {
		if _, err := Bind(root, invalid); err == nil {
			t.Fatalf("accepted invalid binding: %+v", invalid)
		}
		if _, err := os.Stat(filepath.Join(root, FileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid Bind created file: %v", err)
		}
	}
	path, err := Bind(root, Binding{ProjectID: projectA})
	if err != nil || !filepath.IsAbs(path) {
		t.Fatalf("new binding: %q %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	if len(object) != 2 || object["project_id"] == nil || string(object["max_bytes"]) != "12288" {
		t.Fatalf("unexpected binding fields: %s", data)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("temporary binding files left behind: %v", entries)
	}
}

func TestBindingsRejectSymlinksAndNonfiles(t *testing.T) {
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := makeRepository(t)
			path := filepath.Join(root, FileName)
			if kind == "directory" {
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				target := writeConfig(t, t.TempDir(), fmt.Sprintf(`{"project_id":%q}`, projectA))
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := Discover(root); err == nil || errors.Is(err, ErrNoBinding) {
				t.Fatalf("accepted %s binding: %v", kind, err)
			}
			if _, err := Bind(root, Binding{ProjectID: projectA}); err == nil {
				t.Fatalf("Bind accepted %s", kind)
			}
			info, err := os.Lstat(path)
			if err != nil || info.Mode().IsRegular() {
				t.Fatalf("Bind replaced existing %s: %v", kind, err)
			}
		})
	}
}

func TestConcurrentBindingsHaveOneWinnerWithoutPartialFiles(t *testing.T) {
	root := makeRepository(t)
	type result struct {
		requested Binding
		err       error
	}
	const count = 24
	results := make(chan result, count)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < count; i++ {
		requested := Binding{ProjectID: projectA, MaxBytes: DefaultMaxBytes}
		if i%2 == 1 {
			requested.ProjectID = projectB
		}
		workers.Add(1)
		go func(binding Binding) {
			defer workers.Done()
			<-start
			_, err := Bind(root, binding)
			results <- result{binding, err}
		}(requested)
	}
	close(start)
	workers.Wait()
	close(results)
	winning, _, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	succeeded, conflicted := 0, 0
	for result := range results {
		if result.requested == winning {
			if result.err != nil {
				t.Errorf("winning binding failed: %v", result.err)
			}
			succeeded++
		} else {
			if !errors.Is(result.err, ErrBindingConflict) {
				t.Errorf("losing binding did not conflict: %v", result.err)
			}
			conflicted++
		}
	}
	if succeeded != count/2 || conflicted != count/2 {
		t.Fatalf("successes=%d conflicts=%d", succeeded, conflicted)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary files survived concurrent binds: %v %v", entries, err)
	}
}

func TestInvalidSessionDirectoryDoesNotDiscoverAnotherProject(t *testing.T) {
	root := makeRepository(t)
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, []byte("fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", filepath.Join(root, "missing"), file} {
		if _, _, err := Discover(path); err == nil || errors.Is(err, ErrNoBinding) {
			t.Fatalf("invalid session directory %q: %v", path, err)
		}
		if _, err := Bind(path, Binding{ProjectID: projectA}); err == nil {
			t.Fatalf("bound invalid directory %q", path)
		}
	}
	before, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(before, []byte("fixture")) {
		t.Fatalf("existing regular file changed: %v", err)
	}
}
