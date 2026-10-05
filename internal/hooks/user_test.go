package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testPlugin = UserPlugin{Dir: "/repo/herma/plugins/claude", Binary: "/repo/herma/bin/herma", Credentials: "/repo/herma/.herma/credentials.json", Identity: "local-agent", URL: "http://127.0.0.1:8765"}

func decodeSettings(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return v
}

func TestMergeUserPluginIntoEmptySettings(t *testing.T) {
	data, err := mergeUserPlugin(nil, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	v := decodeSettings(t, data)
	if v["env"].(map[string]any)["CLAUDE_CODE_PLUGIN_DIRS"] != testPlugin.Dir {
		t.Fatalf("env: %s", data)
	}
	opts := v["pluginConfigs"].(map[string]any)["herma"].(map[string]any)["options"].(map[string]any)
	if opts["herma"] != testPlugin.Binary || opts["credentials"] != testPlugin.Credentials || opts["identity"] != "local-agent" || opts["url"] != testPlugin.URL {
		t.Fatalf("options: %s", data)
	}
	allow := v["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 2 || allow[0] != "mcp__herma__recall" || allow[1] != "mcp__herma__get" {
		t.Fatalf("allow: %s", data)
	}
}

func TestMergeUserPluginKeepsOtherDirs(t *testing.T) {
	sep := string(os.PathListSeparator)
	for name, existing := range map[string]string{
		"other dirs": "/a" + sep + "/b",
		"empty":      "",
		"already":    "/a" + sep + testPlugin.Dir,
	} {
		t.Run(name, func(t *testing.T) {
			in := `{"env":{"CLAUDE_CODE_PLUGIN_DIRS":` + strconvQuote(existing) + `,"OTHER":"1"}}`
			data, err := mergeUserPlugin([]byte(in), testPlugin)
			if err != nil {
				t.Fatal(err)
			}
			env := decodeSettings(t, data)["env"].(map[string]any)
			got := strings.Split(env["CLAUDE_CODE_PLUGIN_DIRS"].(string), sep)
			count := 0
			for _, d := range got {
				if d == testPlugin.Dir {
					count++
				}
				if d == "" {
					t.Fatalf("empty entry in %q", got)
				}
			}
			if count != 1 || env["OTHER"] != "1" {
				t.Fatalf("dirs %q env %v", got, env)
			}
			if name == "other dirs" && (got[0] != "/a" || got[1] != "/b") {
				t.Fatalf("order changed: %q", got)
			}
		})
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestMergeUserPluginPreservesUnrelatedSettingsAndIsIdempotent(t *testing.T) {
	in := `{"model":"opus","permissions":{"allow":["Bash(ls)"],"deny":["Read(//x)"]},"pluginConfigs":{"other":{"options":{"a":1}}},"hooks":{"Stop":[]}}`
	once, err := mergeUserPlugin([]byte(in), testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := mergeUserPlugin(once, testPlugin)
	if err != nil || string(once) != string(twice) {
		t.Fatalf("not idempotent:\n%s\n%s", once, twice)
	}
	v := decodeSettings(t, once)
	perms := v["permissions"].(map[string]any)
	if v["model"] != "opus" || perms["deny"].([]any)[0] != "Read(//x)" || perms["allow"].([]any)[0] != "Bash(ls)" || len(perms["allow"].([]any)) != 3 {
		t.Fatalf("unrelated settings changed: %s", once)
	}
	if v["pluginConfigs"].(map[string]any)["other"] == nil || v["hooks"] == nil {
		t.Fatalf("lost keys: %s", once)
	}
	if !strings.HasPrefix(string(once), "{\n  \"model\"") {
		t.Fatalf("key order changed: %s", once)
	}
}

func TestMergeUserPluginRejectsWrongTypes(t *testing.T) {
	for name, in := range map[string]string{
		"env not object":     `{"env":[]}`,
		"dirs not string":    `{"env":{"CLAUDE_CODE_PLUGIN_DIRS":1}}`,
		"allow not array":    `{"permissions":{"allow":"x"}}`,
		"configs not object": `{"pluginConfigs":true}`,
		"not json":           `{`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := mergeUserPlugin([]byte(in), testPlugin); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestInstallUserPluginWritesAtomicallyAndKeepsMode(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(path, []byte(`{"model":"opus"}`), 0644); err != nil {
		t.Fatal(err)
	}
	first, err := InstallUserPlugin(home, testPlugin)
	if err != nil || !first.Changed || first.Path != path {
		t.Fatalf("%+v %v", first, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0644 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	second, err := InstallUserPlugin(home, testPlugin)
	if err != nil || second.Changed {
		t.Fatalf("second: %+v %v", second, err)
	}
	empty := t.TempDir()
	created, err := InstallUserPlugin(empty, testPlugin)
	if err != nil || !created.Changed {
		t.Fatalf("create: %+v %v", created, err)
	}
	if info, err := os.Stat(filepath.Join(empty, ".claude", "settings.json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("new file: %v %v", info, err)
	}
	if err := os.WriteFile(path, []byte(`{"env":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallUserPlugin(home, testPlugin); err == nil {
		t.Fatal("wrong type accepted")
	}
	if data, _ := os.ReadFile(path); string(data) != `{"env":[]}` {
		t.Fatalf("file changed on failure: %s", data)
	}
}
