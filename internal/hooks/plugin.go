package hooks

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// pluginDir is where WritePlugin puts the Claude Code plugin, under herma's
// data folder.
var pluginDir = filepath.Join("plugins", "claude")

// WritePlugin writes the plugin files to plugins/claude in herma's data folder,
// creating that folder if needed, and returns the plugin folder. An identical
// copy is left alone; otherwise the folder is replaced whole, so no file of an
// older version remains.
func WritePlugin(dataDir string, files fs.FS) (string, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return "", fmt.Errorf("create %s: %w", dataDir, err)
	}
	root, abs, err := openDirectory(dataDir, "herma data folder")
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	dir := filepath.Join(abs, pluginDir)
	parent := filepath.Dir(pluginDir)
	if err := root.MkdirAll(parent, 0700); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(dir), err)
	}
	if err := checkDirectory(root, parent); err != nil {
		return "", err
	}
	want, err := readTree(files, ".")
	if err != nil {
		return "", fmt.Errorf("read built-in plugin: %w", err)
	}
	if info, err := root.Lstat(pluginDir); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("plugin folder %s must be a directory, not a symlink", dir)
		}
		if have, err := readTree(root.FS(), pluginDir); err == nil && sameTree(have, want) {
			return dir, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	suffix := hex.EncodeToString(entropy[:])
	staged := filepath.Join(parent, ".claude-"+suffix+".tmp")
	if err := root.Mkdir(staged, 0700); err != nil {
		return "", fmt.Errorf("prepare plugin: %w", err)
	}
	defer func() { _ = root.RemoveAll(staged) }()
	for name, data := range want {
		path := filepath.Join(staged, filepath.FromSlash(name))
		if err := root.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", fmt.Errorf("prepare plugin: %w", err)
		}
		if err := root.WriteFile(path, data, 0600); err != nil {
			return "", fmt.Errorf("prepare plugin: %w", err)
		}
	}
	old := filepath.Join(parent, ".claude-"+suffix+".old")
	if err := root.Rename(pluginDir, old); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("replace plugin: %w", err)
	}
	if err := root.Rename(staged, pluginDir); err != nil {
		_ = root.Rename(old, pluginDir)
		return "", fmt.Errorf("replace plugin: %w", err)
	}
	_ = root.RemoveAll(old)
	return dir, nil
}

// readTree returns the regular files under dir in fsys, keyed by slash path
// relative to dir. Anything else, such as a symlink, is an error.
func readTree(fsys fs.FS, dir string) (map[string][]byte, error) {
	tree := map[string][]byte{}
	err := fs.WalkDir(fsys, dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		rel := path
		if dir != "." {
			rel = path[len(dir)+1:]
		}
		tree[rel] = data
		return nil
	})
	return tree, err
}

func sameTree(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for name, data := range a {
		if other, ok := b[name]; !ok || !bytes.Equal(data, other) {
			return false
		}
	}
	return true
}

// isHermaPlugin reports whether dir holds a Claude Code plugin named herma, such
// as one registered from an older checkout.
func isHermaPlugin(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		return false
	}
	var manifest struct {
		Name string `json:"name"`
	}
	return json.Unmarshal(data, &manifest) == nil && manifest.Name == pluginName
}
