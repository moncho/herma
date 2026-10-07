package hooks

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestWritePluginWritesReplacesAndKeepsIdenticalFiles(t *testing.T) {
	home := t.TempDir()
	v1 := fstest.MapFS{
		".claude-plugin/plugin.json": {Data: []byte(`{"name":"herma"}`)},
		"hooks/register.ts":          {Data: []byte("v1")},
	}
	dir, err := WritePlugin(home, v1)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "herma", "plugins", "claude"); dir != want {
		t.Fatalf("dir %q, want %q", dir, want)
	}
	before, err := os.Stat(filepath.Join(dir, "hooks", "register.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WritePlugin(home, v1); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(dir, "hooks", "register.ts"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("identical plugin was rewritten: %v", err)
	}

	v2 := fstest.MapFS{
		".claude-plugin/plugin.json": {Data: []byte(`{"name":"herma"}`)},
		"hooks/status.ts":            {Data: []byte("v2")},
	}
	if _, err := WritePlugin(home, v2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hooks", "register.ts")); !os.IsNotExist(err) {
		t.Fatalf("file from the previous version remains: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "hooks", "status.ts")); err != nil || string(data) != "v2" {
		t.Fatalf("new file: %q %v", data, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) != 1 {
		t.Fatalf("leftovers next to the plugin: %v", entries)
	}
}

func TestWritePluginRefusesASymlinkedFolder(t *testing.T) {
	home := t.TempDir()
	plugins := filepath.Join(home, ".config", "herma", "plugins")
	if err := os.MkdirAll(plugins, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(plugins, "claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := WritePlugin(home, fstest.MapFS{"hooks/register.ts": {Data: []byte("v1")}}); err == nil {
		t.Fatal("wrote through a symlinked plugin folder")
	}
}
