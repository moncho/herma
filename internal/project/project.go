// Package project stores nonsecret repository-to-knowledge-project bindings.
package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	FileName        = ".herma-project.json"
	DefaultMaxBytes = 12288
	MinMaxBytes     = 2048
	MaxMaxBytes     = 65536
	maxFileBytes    = 64 << 10
)

var (
	ErrNoBinding       = errors.New("no " + FileName + " binding found")
	ErrBindingConflict = errors.New("directory is already bound to a different project or context budget")
	errBindingAbsent   = errors.New("project binding does not exist")
)

// Binding is safe to share in source control. Credentials and endpoints belong
// in the session's own configuration, never in this file.
type Binding struct {
	ProjectID string `json:"project_id"`
	MaxBytes  int    `json:"max_bytes"`
}

// Discover finds the nearest binding from the physical session directory.
// The binding at a Git checkout root is considered before its .git file or
// directory stops traversal. Linked worktrees therefore find their own tracked
// binding without following gitdir pointers into a different checkout.
// An invalid or unreadable nearer binding always stops discovery with an error.
func Discover(cwd string) (Binding, string, error) {
	dir, err := resolveDirectory(cwd)
	if err != nil {
		return Binding{}, "", err
	}
	for {
		path := filepath.Join(dir, FileName)
		binding, err := readBinding(path)
		if err == nil {
			return binding, path, nil
		}
		if !errors.Is(err, errBindingAbsent) {
			return Binding{}, "", err
		}
		// Any existing .git entry is a boundary. Do not follow symlinks or read
		// worktree metadata; either could lead outside the session's checkout.
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return Binding{}, "", fmt.Errorf("%w at or below Git boundary %s", ErrNoBinding, dir)
		} else if !errors.Is(err, os.ErrNotExist) {
			return Binding{}, "", fmt.Errorf("inspect Git boundary in %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Binding{}, "", ErrNoBinding
		}
		dir = parent
	}
}

// Bind atomically writes a binding in dir. MaxBytes == 0 selects the default.
// A semantically identical existing binding is left unchanged, including its
// formatting. A different, malformed, or nonregular existing file is never
// overwritten, including when another process creates it concurrently.
func Bind(dir string, binding Binding) (string, error) {
	if binding.MaxBytes == 0 {
		binding.MaxBytes = DefaultMaxBytes
	}
	if err := validate(binding); err != nil {
		return "", err
	}
	dir, err := resolveDirectory(dir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, FileName)
	if existing, err := readBinding(path); err == nil {
		return compareExisting(path, existing, binding)
	} else if !errors.Is(err, errBindingAbsent) {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".herma-project-*")
	if err != nil {
		return "", fmt.Errorf("prepare project binding in %s: %w", dir, err)
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err := f.Chmod(0644); err != nil {
		return "", fmt.Errorf("set project binding permissions: %w", err)
	}
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(binding); err != nil {
		return "", fmt.Errorf("write project binding: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("save project binding: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close project binding: %w", err)
	}
	// Publish the complete file atomically, without replacing an existing path.
	if err := os.Link(f.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := readBinding(path)
			if readErr != nil {
				return "", readErr
			}
			return compareExisting(path, existing, binding)
		}
		return "", fmt.Errorf("create project binding %s without overwriting an existing file: %w", path, err)
	}
	return path, nil
}

func compareExisting(path string, existing, requested Binding) (string, error) {
	if existing != requested {
		return "", fmt.Errorf("%w: %s; review the existing binding before changing it", ErrBindingConflict, path)
	}
	return path, nil
}

func resolveDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("session directory is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve session directory: %w", err)
	}
	physical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve session directory %s: %w", absolute, err)
	}
	info, err := os.Stat(physical)
	if err != nil {
		return "", fmt.Errorf("inspect session directory %s: %w", physical, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("session directory %s must be a directory", physical)
	}
	return physical, nil
}

func readBinding(path string) (Binding, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Binding{}, fmt.Errorf("%w: %s", errBindingAbsent, path)
		}
		return Binding{}, fmt.Errorf("inspect project binding %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Binding{}, fmt.Errorf("project binding %s must be a regular file, not a symlink or directory", path)
	}
	if info.Size() > maxFileBytes {
		return Binding{}, fmt.Errorf("project binding %s exceeds 64 KiB", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return Binding{}, fmt.Errorf("open project binding %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return Binding{}, fmt.Errorf("inspect opened project binding %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return Binding{}, fmt.Errorf("project binding %s changed while being opened; retry discovery", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return Binding{}, fmt.Errorf("read project binding %s: %w", path, err)
	}
	if len(data) > maxFileBytes {
		return Binding{}, fmt.Errorf("project binding %s exceeds 64 KiB", path)
	}
	binding, err := parseBinding(data)
	if err != nil {
		return Binding{}, fmt.Errorf("invalid project binding %s: %w", path, err)
	}
	return binding, nil
}

func parseBinding(data []byte) (Binding, error) {
	if !utf8.Valid(data) {
		return Binding{}, errors.New("file must contain valid UTF-8 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return Binding{}, errors.New("file must contain one JSON object")
	}
	binding := Binding{MaxBytes: DefaultMaxBytes}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Binding{}, errors.New("file must contain valid JSON")
		}
		field, ok := token.(string)
		if !ok {
			return Binding{}, errors.New("object field names must be strings")
		}
		if seen[field] {
			return Binding{}, fmt.Errorf("duplicate field %q", field)
		}
		seen[field] = true
		switch field {
		case "project_id":
			if err := decoder.Decode(&binding.ProjectID); err != nil {
				return Binding{}, errors.New("project_id must be a string")
			}
		case "max_bytes":
			var budget json.RawMessage
			if err := decoder.Decode(&budget); err != nil || bytes.Equal(bytes.TrimSpace(budget), []byte("null")) {
				return Binding{}, errors.New("max_bytes must be an integer")
			}
			if err := json.Unmarshal(budget, &binding.MaxBytes); err != nil {
				return Binding{}, errors.New("max_bytes must be an integer")
			}
		default:
			return Binding{}, fmt.Errorf("unknown field %q; only project_id and max_bytes are allowed", field)
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return Binding{}, errors.New("file must contain valid JSON")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return Binding{}, errors.New("file must contain exactly one JSON object")
	}
	if err := validate(binding); err != nil {
		return Binding{}, err
	}
	return binding, nil
}

func validate(binding Binding) error {
	if !validProjectID(binding.ProjectID) {
		return errors.New("project_id must be a record ID: rec_ followed by 32 lowercase hexadecimal digits")
	}
	if binding.MaxBytes < MinMaxBytes || binding.MaxBytes > MaxMaxBytes {
		return fmt.Errorf("max_bytes must be between %d and %d", MinMaxBytes, MaxMaxBytes)
	}
	return nil
}

func validProjectID(id string) bool {
	if len(id) != 36 || !strings.HasPrefix(id, "rec_") {
		return false
	}
	for _, r := range id[4:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
