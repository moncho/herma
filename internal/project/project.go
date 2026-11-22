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
	DefaultMaxBytes = 10000
	MinMaxBytes     = 2048
	MaxMaxBytes     = 65536
	maxFileBytes    = 64 << 10
	maxPointerBytes = 4096
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

// Discover finds the nearest binding from the physical session directory and
// returns it with the directory it applies to. The binding at a Git checkout
// root is considered before its .git file or directory stops traversal. A
// linked worktree without a binding of its own inherits its main checkout's
// (see mainCheckout) and is the directory returned; no other .git entry is
// followed. An invalid or unreadable binding, nearer or inherited, always
// stops discovery with an error.
func Discover(cwd string) (Binding, string, error) {
	dir, err := resolveDirectory(cwd)
	if err != nil {
		return Binding{}, "", err
	}
	for {
		binding, err := readBinding(filepath.Join(dir, FileName))
		if err == nil {
			return binding, dir, nil
		}
		if !errors.Is(err, errBindingAbsent) {
			return Binding{}, "", err
		}
		// Any existing .git entry is a boundary.
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			if main, ok := mainCheckout(dir); ok {
				binding, err := readBinding(filepath.Join(main, FileName))
				if err == nil {
					return binding, dir, nil
				}
				if !errors.Is(err, errBindingAbsent) {
					return Binding{}, "", err
				}
			}
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

// mainCheckout returns the main checkout of the repository that dir is a
// linked worktree of. A .git file is only a claim, which any folder can make,
// so the repository must confirm it the way git does: the admin directory the
// file names has to sit in the repository's own .git/worktrees and name that
// same .git file back. Only the repository's owner can write there, so a
// folder cannot adopt the binding of a repository that did not create it.
func mainCheckout(dir string) (string, bool) {
	pointer := filepath.Join(dir, ".git")
	target, ok := readPointer(pointer, "gitdir: ")
	if !ok {
		return "", false
	}
	// The physical path, so the repository is derived from where the admin
	// directory really is and not from a symlink that names it.
	admin, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", false
	}
	worktrees := filepath.Dir(admin)
	gitDir := filepath.Dir(worktrees)
	if filepath.Base(worktrees) != "worktrees" || filepath.Base(gitDir) != ".git" {
		return "", false
	}
	back, ok := readPointer(filepath.Join(admin, "gitdir"), "")
	if !ok {
		return "", false
	}
	claimed, err := os.Lstat(pointer)
	if err != nil {
		return "", false
	}
	named, err := os.Lstat(back)
	if err != nil || !os.SameFile(claimed, named) {
		return "", false
	}
	return filepath.Dir(gitDir), true
}

// readPointer reads a one-line git file that holds a path after prefix, such
// as a worktree's .git file, and resolves a relative path against the file's
// directory. It refuses anything but a small regular file, so a planted pipe
// or huge file can neither block nor exhaust the session hook.
func readPointer(path, prefix string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPointerBytes {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimRight(string(data), "\r\n"), prefix)
	if !ok {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return filepath.Clean(target), true
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
