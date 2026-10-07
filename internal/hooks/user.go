package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// UserPlugin is what Claude Code needs in the user's settings to load the herma
// plugin and let it run herma.
type UserPlugin struct {
	Dir         string // absolute path of the plugin folder
	Binary      string // absolute path of the herma binary
	Credentials string // absolute path of the agent credentials file
	Identity    string
	URL         string // herma server URL the shell hook uses
}

const (
	pluginName    = "herma"
	pluginDirsKey = "CLAUDE_CODE_PLUGIN_DIRS"
)

var pluginTools = []string{"mcp__herma__recall", "mcp__herma__get"}

// InstallUserPlugin merges the herma plugin into home/.claude/settings.json,
// creating it if absent. Other settings are kept; the write is atomic and keeps
// an existing file's permissions.
func InstallUserPlugin(home string, p UserPlugin) (Installation, error) {
	root, abs, err := openDirectory(home, "home directory")
	if err != nil {
		return Installation{}, err
	}
	defer func() { _ = root.Close() }()
	path := filepath.Join(".claude", "settings.json")
	result := Installation{Client: "claude-plugin", Path: filepath.Join(abs, path), ID: "herma-plugin"}
	before, info, err := readConfig(root, path)
	if err != nil {
		return result, fmt.Errorf("inspect %s: %w", result.Path, err)
	}
	after, err := mergeUserPlugin(before, p, isHermaPlugin)
	if err != nil {
		return result, fmt.Errorf("invalid settings %s: %w", result.Path, err)
	}
	if bytes.Equal(before, after) {
		return result, nil
	}
	if err := root.Mkdir(".claude", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return result, err
	}
	if err := checkDirectory(root, ".claude"); err != nil {
		return result, err
	}
	staged, err := stage(root, ".claude", after)
	if err != nil {
		return result, fmt.Errorf("prepare %s: %w", result.Path, err)
	}
	defer func() { _ = root.Remove(staged) }()
	if info != nil {
		if err := root.Chmod(staged, info.Mode().Perm()); err != nil {
			return result, err
		}
	}
	current, currentInfo, err := readConfig(root, path)
	if err != nil {
		return result, fmt.Errorf("recheck %s: %w", result.Path, err)
	}
	if !bytes.Equal(current, before) || (currentInfo == nil) != (info == nil) || (info != nil && !os.SameFile(info, currentInfo)) {
		return result, fmt.Errorf("%s changed during installation; retry", result.Path)
	}
	if err := root.Rename(staged, path); err != nil {
		return result, fmt.Errorf("replace %s: %w", result.Path, err)
	}
	result.Changed = true
	return result, nil
}

// mergeUserPlugin adds p to the settings in data. Plugin folders for which
// replaced reports true, other copies of the herma plugin, are dropped so only
// one loads.
func mergeUserPlugin(data []byte, p UserPlugin, replaced func(dir string) bool) ([]byte, error) {
	settings, err := parseSettings(data)
	if err != nil {
		return nil, err
	}
	env, err := childObject(settings, "env")
	if err != nil {
		return nil, err
	}
	var dirs []string
	if raw, ok := env.get(pluginDirsKey); ok {
		value, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("env.%s must be a string", pluginDirsKey)
		}
		for _, d := range filepath.SplitList(value) {
			if d != "" && (d == p.Dir || !replaced(d)) {
				dirs = append(dirs, d)
			}
		}
	}
	if !slices.Contains(dirs, p.Dir) {
		dirs = append(dirs, p.Dir)
	}
	env.set(pluginDirsKey, strings.Join(dirs, string(os.PathListSeparator)))
	settings.set("env", env)

	configs, err := childObject(settings, "pluginConfigs")
	if err != nil {
		return nil, err
	}
	entry, err := childObject(configs, pluginName)
	if err != nil {
		return nil, fmt.Errorf("pluginConfigs.%w", err)
	}
	options := newObject()
	options.set("herma", p.Binary)
	options.set("credentials", p.Credentials)
	options.set("identity", p.Identity)
	options.set("url", p.URL)
	entry.set("options", options)
	configs.set(pluginName, entry)
	settings.set("pluginConfigs", configs)

	permissions, err := childObject(settings, "permissions")
	if err != nil {
		return nil, err
	}
	allow := []any{}
	if raw, ok := permissions.get("allow"); ok {
		var valid bool
		if allow, valid = raw.([]any); !valid {
			return nil, errors.New("permissions.allow must be an array")
		}
	}
	for _, tool := range pluginTools {
		if !slices.ContainsFunc(allow, func(v any) bool { s, ok := v.(string); return ok && s == tool }) {
			allow = append(allow, tool)
		}
	}
	permissions.set("allow", allow)
	settings.set("permissions", permissions)
	return encodeSettings(settings)
}

// childObject returns parent[key] as an object, or a new one when absent.
func childObject(parent *object, key string) (*object, error) {
	raw, ok := parent.get(key)
	if !ok {
		return newObject(), nil
	}
	child, ok := raw.(*object)
	if !ok {
		return nil, fmt.Errorf("%s must be a JSON object", key)
	}
	return child, nil
}
