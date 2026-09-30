// Package hooks installs project-local session hooks without changing unrelated
// settings. Every destination is validated before any configuration is replaced.
package hooks

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
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
	HookID         = "herma-session-start-v1"
	marker         = " # herma-managed:session-start:v1"
	maxConfigBytes = 2 << 20
)

type Installation struct {
	Client  string `json:"client"`
	Path    string `json:"path"`
	ID      string `json:"id"`
	Changed bool   `json:"changed"`
}

// QuoteCommand quotes an absolute executable and its arguments for a POSIX
// shell. No shell expansion occurs, including for dollars, backticks or quotes.
// Callers supply credential paths, never tokens, in this persisted command.
func QuoteCommand(args []string) (string, error) {
	if len(args) == 0 || !filepath.IsAbs(args[0]) {
		return "", errors.New("hook executable must be an absolute path")
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		if strings.ContainsAny(arg, "\x00\r\n") || !utf8.ValidString(arg) {
			return "", errors.New("hook arguments must be valid UTF-8 without NUL or line breaks")
		}
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " "), nil
}

type plan struct {
	client string
	path   string
	before []byte
	info   os.FileInfo
	after  []byte
	staged string
}

// Install creates or updates the managed SessionStart hook for claude, codex,
// or both. command must already be safely quoted (normally with QuoteCommand).
// The stable shell-comment marker identifies only this installer's own entry.
// Existing malformed JSON, duplicate keys, and symlink destinations are errors.
// File replacements are individually atomic; a rare commit failure may return
// already-completed installations alongside its error.
func Install(dir, client, command string) ([]Installation, error) {
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, "\x00\r\n") || !utf8.ValidString(command) || strings.Contains(command, "herma-managed:session-start:") {
		return nil, errors.New("hook command must be nonempty, single-line, and contain no managed hook marker")
	}
	var plans []plan
	switch client {
	case "claude":
		plans = []plan{{client: "claude", path: filepath.Join(".claude", "settings.local.json")}}
	case "codex":
		plans = []plan{{client: "codex", path: filepath.Join(".codex", "hooks.json")}}
	case "both":
		plans = []plan{{client: "claude", path: filepath.Join(".claude", "settings.local.json")}, {client: "codex", path: filepath.Join(".codex", "hooks.json")}}
	default:
		return nil, errors.New("hook client must be claude, codex, or both")
	}
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("project directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve project directory: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("inspect project directory: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("project directory must be an existing directory, not a symlink")
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open project directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("project directory changed while opening; retry installation")
	}
	for i := range plans {
		p := &plans[i]
		p.before, p.info, err = readConfig(root, p.path)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", filepath.Join(abs, p.path), err)
		}
		p.after, err = merge(p.before, p.client, command+marker)
		if err != nil {
			return nil, fmt.Errorf("invalid hook configuration %s: %w", filepath.Join(abs, p.path), err)
		}
	}
	// Stage all files first, so malformed or unwritable second destinations do
	// not cause an otherwise valid first destination to be overwritten.
	defer func() {
		for _, p := range plans {
			if p.staged != "" {
				_ = root.Remove(p.staged)
			}
		}
	}()
	for i := range plans {
		p := &plans[i]
		if bytes.Equal(p.before, p.after) {
			continue
		}
		parent := filepath.Dir(p.path)
		if err := root.Mkdir(parent, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create hook directory %s: %w", filepath.Join(abs, parent), err)
		}
		if err := checkDirectory(root, parent); err != nil {
			return nil, err
		}
		p.staged, err = stage(root, parent, p.after)
		if err != nil {
			return nil, fmt.Errorf("prepare %s: %w", filepath.Join(abs, p.path), err)
		}
	}
	// Refuse a changed destination instead of overwriting another editor's work.
	for _, p := range plans {
		current, info, err := readConfig(root, p.path)
		if err != nil {
			return nil, fmt.Errorf("recheck %s: %w", filepath.Join(abs, p.path), err)
		}
		if !bytes.Equal(current, p.before) || (info == nil) != (p.info == nil) || (info != nil && !os.SameFile(info, p.info)) {
			return nil, fmt.Errorf("hook configuration %s changed during installation; retry", filepath.Join(abs, p.path))
		}
	}
	installed := make([]Installation, 0, len(plans))
	for i := range plans {
		p := &plans[i]
		changed := p.staged != ""
		if changed {
			if err := root.Rename(p.staged, p.path); err != nil {
				return installed, fmt.Errorf("replace %s (%d earlier configurations completed): %w", filepath.Join(abs, p.path), len(installed), err)
			}
			p.staged = ""
		}
		installed = append(installed, Installation{Client: p.client, Path: filepath.Join(abs, p.path), ID: HookID, Changed: changed})
	}
	return installed, nil
}

func checkDirectory(root *os.Root, path string) error {
	info, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("hook directory %s must be a directory, not a symlink", path)
	}
	return nil
}

func readConfig(root *os.Root, path string) ([]byte, os.FileInfo, error) {
	if err := checkDirectory(root, filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	info, err := root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.New("hook configuration must be a regular file, not a symlink")
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, nil, errors.New("hook configuration changed while opening; retry")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, nil, errors.New("hook configuration exceeds 2 MiB")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil, errors.New("existing hook configuration is empty")
	}
	return data, info, nil
}

func stage(root *os.Root, parent string, data []byte) (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	name := filepath.Join(parent, ".herma-hooks-"+hex.EncodeToString(entropy[:])+".tmp")
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = root.Remove(name)
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	keep = true
	return name, nil
}

func merge(data []byte, client, command string) ([]byte, error) {
	settings := newObject()
	if data != nil {
		if !utf8.Valid(data) {
			return nil, errors.New("hook configuration must be valid UTF-8")
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		value, err := parseJSON(dec, 0)
		if err != nil {
			return nil, err
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return nil, errors.New("configuration must contain exactly one JSON object")
		}
		var ok bool
		settings, ok = value.(*object)
		if !ok {
			return nil, errors.New("configuration must be a JSON object")
		}
	}
	hookSettings := newObject()
	if raw, ok := settings.get("hooks"); ok {
		var valid bool
		hookSettings, valid = raw.(*object)
		if !valid {
			return nil, errors.New("hooks must be a JSON object")
		}
	}
	groups := []any{}
	if raw, ok := hookSettings.get("SessionStart"); ok {
		var valid bool
		groups, valid = raw.([]any)
		if !valid {
			return nil, errors.New("SessionStart hooks must be an array")
		}
	}
	handler := map[string]any{"type": "command", "command": command, "timeout": 6, "async": false}
	if client == "codex" {
		handler["additionalContextLimit"] = 70000
	}
	// An omitted matcher covers every SessionStart source, including Claude's
	// fork events and future lifecycle sources, without client-specific filters.
	desired := map[string]any{"hooks": []any{handler}}
	managed := -1
	for index, raw := range groups {
		group, ok := raw.(*object)
		if !ok {
			return nil, errors.New("each SessionStart matcher group must be an object")
		}
		if m, ok := group.get("matcher"); ok {
			if _, valid := m.(string); !valid {
				return nil, errors.New("SessionStart matcher must be a string")
			}
		}
		rawHandlers, _ := group.get("hooks")
		handlers, ok := rawHandlers.([]any)
		if !ok {
			return nil, errors.New("each SessionStart matcher group must contain a hooks array")
		}
		for _, raw := range handlers {
			h, ok := raw.(*object)
			if !ok {
				return nil, errors.New("each SessionStart handler must be an object")
			}
			rawKind, _ := h.get("type")
			kind, ok := rawKind.(string)
			if !ok || kind == "" {
				return nil, errors.New("each SessionStart handler must have a string type")
			}
			rawCommand, _ := h.get("command")
			cmd, _ := rawCommand.(string)
			if kind == "command" && strings.TrimSpace(cmd) == "" {
				return nil, errors.New("each command handler must have a nonempty command")
			}
			if !strings.Contains(cmd, "herma-managed:session-start:") {
				continue
			}
			if !strings.HasSuffix(cmd, marker) || kind != "command" || len(handlers) != 1 || managed >= 0 {
				return nil, errors.New("managed herma hook has a conflicting marker or shares a group; separate or remove that entry before reinstalling")
			}
			for _, key := range group.keys {
				if key != "matcher" && key != "hooks" {
					return nil, errors.New("managed herma matcher group has custom fields; remove its managed marker to preserve it separately")
				}
			}
			for _, key := range h.keys {
				if key != "type" && key != "command" && key != "timeout" && key != "async" && key != "additionalContextLimit" {
					return nil, errors.New("managed herma hook has custom fields; remove its managed marker to preserve it separately")
				}
			}
			managed = index
		}
	}
	if managed >= 0 {
		groups[managed] = desired
	} else {
		groups = append(groups, desired)
	}
	hookSettings.set("SessionStart", groups)
	settings.set("hooks", hookSettings)
	var result bytes.Buffer
	encoder := json.NewEncoder(&result)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(settings); err != nil {
		return nil, err
	}
	if result.Len() > maxConfigBytes {
		return nil, errors.New("updated hook configuration would exceed 2 MiB")
	}
	return result.Bytes(), nil
}

// object is a parsed JSON object that keeps its key order, so rewriting a
// settings file changes only the entries this installer manages.
type object struct {
	keys   []string
	values map[string]any
}

func newObject() *object { return &object{values: map[string]any{}} }

func (o *object) get(key string) (any, bool) {
	value, ok := o.values[key]
	return value, ok
}

func (o *object) set(key string, value any) {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

func (o *object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		for j, value := range []any{key, o.values[key]} {
			if j > 0 {
				buf.WriteByte(':')
			}
			data, err := marshalLiteral(value)
			if err != nil {
				return nil, err
			}
			buf.Write(data)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalLiteral keeps <, > and & as written. The top-level encoder cannot
// undo HTML escaping that a nested MarshalJSON has already applied.
func marshalLiteral(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Decode without losing large numeric settings or silently dropping duplicate
// keys. Both would otherwise corrupt unrelated settings during a merge.
func parseJSON(dec *json.Decoder, depth int) (any, error) {
	if depth > 100 {
		return nil, errors.New("hook configuration JSON is nested too deeply")
	}
	token, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("parse hook JSON: %w", err)
	}
	switch token {
	case json.Delim('{'):
		value := newObject()
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, errors.New("JSON object key must be a string")
			}
			if _, exists := value.get(name); exists {
				return nil, errors.New("hook configuration contains a duplicate JSON object key")
			}
			item, err := parseJSON(dec, depth+1)
			if err != nil {
				return nil, err
			}
			value.set(name, item)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return value, nil
	case json.Delim('['):
		value := []any{}
		for dec.More() {
			item, err := parseJSON(dec, depth+1)
			if err != nil {
				return nil, err
			}
			value = append(value, item)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return token, nil
	}
}
