package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func sameCommand(command string) func(string) (string, error) {
	return func(string) (string, error) { return command, nil }
}

func commandForTest(t *testing.T, identity string) string {
	t.Helper()
	command, err := QuoteCommand([]string{"/opt/knowledge base/bin/herma", "--url", "http://127.0.0.1:8765", "--credentials", "/private/credentials.json", "--identity", identity, "hook", "session-start"})
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func writeConfig(t *testing.T, root, relative, content string) string {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sessionGroups(t *testing.T, doc map[string]any) []any {
	t.Helper()
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatal("missing hooks object")
	}
	groups, ok := hooks["SessionStart"].([]any)
	if !ok {
		t.Fatal("missing SessionStart array")
	}
	return groups
}

func TestInstallBothPreservesSettingsAndUpdatesManagedHook(t *testing.T) {
	dir := t.TempDir()
	original := `{"permissions":{"allow":["Read"],"deny":["Bash(rm *)"]},"customNumber":9007199254740993,"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo stop"}]}],"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"echo existing","timeout":2}]}]}}`
	paths := []string{writeConfig(t, dir, ".claude/settings.local.json", original), writeConfig(t, dir, ".codex/hooks.json", original)}
	before := readJSON(t, paths[0])
	command := commandForTest(t, "owner")
	results, err := Install(dir, "both", sameCommand(command))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d installation results", len(results))
	}
	firstBytes := make([][]byte, len(results))
	for i, result := range results {
		if !result.Changed || result.Path != paths[i] || result.ID != HookID {
			t.Fatalf("bad result: %#v", result)
		}
		doc := readJSON(t, result.Path)
		if !reflect.DeepEqual(doc["permissions"], before["permissions"]) || doc["customNumber"] != before["customNumber"] || !reflect.DeepEqual(doc["hooks"].(map[string]any)["Stop"], before["hooks"].(map[string]any)["Stop"]) {
			t.Fatal("changed unrelated settings")
		}
		groups := sessionGroups(t, doc)
		if len(groups) != 2 || !reflect.DeepEqual(groups[0], sessionGroups(t, before)[0]) {
			t.Fatal("changed unrelated SessionStart hook")
		}
		group := groups[1].(map[string]any)
		handler := group["hooks"].([]any)[0].(map[string]any)
		if _, filtered := group["matcher"]; filtered {
			t.Fatal("managed hook must match every SessionStart source, including fork")
		}
		if handler["command"] != command+marker || handler["timeout"] != json.Number("6") || handler["async"] != false {
			t.Fatalf("incorrect managed hook: %#v", group)
		}
		if result.Client == "codex" && handler["additionalContextLimit"] != json.Number("70000") {
			t.Fatal("missing Codex additional context limit")
		}
		if result.Client == "claude" && handler["additionalContextLimit"] != nil {
			t.Fatal("added Codex-only field to Claude")
		}
		info, err := os.Stat(result.Path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("hook configuration permissions are not private")
		}
		firstBytes[i], err = os.ReadFile(result.Path)
		if err != nil {
			t.Fatal(err)
		}
	}
	results, err = Install(dir, "both", sameCommand(command))
	if err != nil {
		t.Fatal(err)
	}
	for i, result := range results {
		current, err := os.ReadFile(result.Path)
		if err != nil || result.Changed || !bytes.Equal(current, firstBytes[i]) {
			t.Fatal("repeat installation is not idempotent")
		}
	}
	updated := commandForTest(t, "worker")
	results, err = Install(dir, "both", sameCommand(updated))
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		groups := sessionGroups(t, readJSON(t, result.Path))
		if len(groups) != 2 || !result.Changed {
			t.Fatal("updating credentials duplicated the hook")
		}
		handler := groups[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)
		if handler["command"] != updated+marker {
			t.Fatal("managed hook command was not updated")
		}
	}
}

func TestReinstallRemovesLegacySessionStartSourceFilter(t *testing.T) {
	dir := t.TempDir()
	command := commandForTest(t, "owner")
	results, err := Install(dir, "both", sameCommand(command))
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		doc := readJSON(t, result.Path)
		group := sessionGroups(t, doc)[0].(map[string]any)
		group["matcher"] = "startup|resume|clear|compact"
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(result.Path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	results, err = Install(dir, "both", sameCommand(command))
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		groups := sessionGroups(t, readJSON(t, result.Path))
		if !result.Changed || len(groups) != 1 {
			t.Fatal("legacy hook was not updated in place")
		}
		if _, filtered := groups[0].(map[string]any)["matcher"]; filtered {
			t.Fatal("reinstalled hook still excludes fork or future SessionStart sources")
		}
	}
}

func TestMalformedSecondDestinationDoesNotChangeFirst(t *testing.T) {
	for name, malformed := range map[string]string{
		"invalid UTF-8": "{\"name\":\"\xff\"}",
		"empty":         "", "syntax": "{", "array": "[]", "null": "null", "duplicate": "{\"permissions\":{},\"permissions\":{}}", "trailing": "{} {}", "null hooks": "{\"hooks\":null}", "wrong hooks": "{\"hooks\":[]}", "null groups": "{\"hooks\":{\"SessionStart\":null}}", "wrong group": "{\"hooks\":{\"SessionStart\":[7]}}", "missing handlers": "{\"hooks\":{\"SessionStart\":[{}]}}", "bad handler": "{\"hooks\":{\"SessionStart\":[{\"hooks\":[{}]}]}}",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			first := writeConfig(t, dir, ".claude/settings.local.json", `{ "permissions": { "allow": ["Read"] } }`)
			before, err := os.ReadFile(first)
			if err != nil {
				t.Fatal(err)
			}
			second := writeConfig(t, dir, ".codex/hooks.json", malformed)
			if _, err := Install(dir, "both", sameCommand(commandForTest(t, "owner"))); err == nil {
				t.Fatal("accepted malformed second destination")
			}
			after, err := os.ReadFile(first)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("changed first file despite malformed second file")
			}
			afterSecond, err := os.ReadFile(second)
			if err != nil || string(afterSecond) != malformed {
				t.Fatal("changed malformed file")
			}
		})
	}
}

func TestInstallCreatesOnlyRequestedClientAndRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	if _, err := Install(dir, "claude", sameCommand(commandForTest(t, "owner"))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".codex")); !os.IsNotExist(err) {
		t.Fatal("created unrequested Codex configuration")
	}
	info, err := os.Stat(filepath.Join(dir, ".claude"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("new settings directory is not private")
	}
	for _, target := range []string{"directory", "file", "project"} {
		t.Run(target, func(t *testing.T) {
			project, outside := t.TempDir(), t.TempDir()
			outsideConfig := writeConfig(t, outside, "hooks.json", `{"description":"outside"}`)
			switch target {
			case "directory":
				if err := os.Symlink(outside, filepath.Join(project, ".codex")); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.Mkdir(filepath.Join(project, ".codex"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outsideConfig, filepath.Join(project, ".codex/hooks.json")); err != nil {
					t.Fatal(err)
				}
			case "project":
				link := filepath.Join(project, "linked-project")
				if err := os.Symlink(outside, link); err != nil {
					t.Fatal(err)
				}
				project = link
			}
			if _, err := Install(project, "codex", sameCommand(commandForTest(t, "owner"))); err == nil {
				t.Fatal("accepted symlink destination")
			}
			data, err := os.ReadFile(outsideConfig)
			if err != nil || string(data) != `{"description":"outside"}` {
				t.Fatal("changed data outside project")
			}
		})
	}
}

func TestManagedHookWithCustomExecutionFieldsIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	command := commandForTest(t, "owner")
	results, err := Install(dir, "codex", sameCommand(command))
	if err != nil {
		t.Fatal(err)
	}
	doc := readJSON(t, results[0].Path)
	handler := sessionGroups(t, doc)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	handler["commandWindows"] = "custom launcher"
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(results[0].Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir, "codex", sameCommand(command)); err == nil || !strings.Contains(err.Error(), "custom fields") {
		t.Fatalf("overwrote a customized managed hook: %v", err)
	}
	after, err := os.ReadFile(results[0].Path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("changed a conflicting managed hook")
	}
}

func TestQuoteCommandPassesEveryArgumentLiterally(t *testing.T) {
	args := []string{"/usr/bin/printf", "%s\\n", "a'b", "two words", "$(touch should-not-exist)", "`printf injected`", "$HOME", "semi;colon", "", "back\\slash"}
	command, err := QuoteCommand(args)
	if err != nil {
		t.Fatal(err)
	}
	run := exec.Command("/bin/sh", "-c", command+marker)
	run.Dir = t.TempDir()
	got, err := run.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(args[2:], "\n") + "\n"
	if string(got) != want {
		t.Fatalf("shell quoting changed arguments: got %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(run.Dir, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("shell executed an argument")
	}
	for _, args := range [][]string{nil, {"herma"}, {"/bin/herma", "bad\nargument"}, {"/bin/herma", "bad\x00argument"}} {
		if _, err := QuoteCommand(args); err == nil {
			t.Fatalf("accepted unsafe arguments: %q", args)
		}
	}
}

func TestInstallKeepsKeyOrderAndLiteralCharacters(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, filepath.Join(".claude", "settings.local.json"), `{
  "permissions": {"allow": ["Bash(make && go test)"]},
  "env": {"Z_VAR": "<value>", "A_VAR": "1"},
  "model": "custom"
}
`)
	if _, err := Install(dir, "claude", sameCommand(commandForTest(t, "owner"))); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, literal := range []string{`"Bash(make && go test)"`, `"<value>"`} {
		if !strings.Contains(text, literal) {
			t.Fatalf("installed settings escaped %s:\n%s", literal, text)
		}
	}
	order := []string{`"permissions"`, `"env"`, `"Z_VAR"`, `"A_VAR"`, `"model"`, `"hooks"`}
	previous := -1
	for _, key := range order {
		index := strings.Index(text, key)
		if index <= previous {
			t.Fatalf("keys were reordered; want %v:\n%s", order, text)
		}
		previous = index
	}
	results, err := Install(dir, "claude", sameCommand(commandForTest(t, "owner")))
	if err != nil || len(results) != 1 || results[0].Changed {
		t.Fatalf("reinstall changed settings: %+v %v", results, err)
	}
}

func TestInstallWritesEachClientsCommandAndUpgradesV1(t *testing.T) {
	dir := t.TempDir()
	v1 := commandForTest(t, "worker") + " # herma-managed:session-start:v1"
	legacy := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":` + strconv.Quote(v1) + `,"timeout":6,"async":false}]}]}}`
	writeConfig(t, dir, ".claude/settings.local.json", legacy)
	perClient := func(client string) (string, error) {
		return QuoteCommand([]string{"/opt/herma/bin/herma", "hook", "session-start", "--client", client})
	}
	if _, err := Install(dir, "both", perClient); err != nil {
		t.Fatal(err)
	}
	for client, relative := range map[string]string{"claude": ".claude/settings.local.json", "codex": ".codex/hooks.json"} {
		groups := sessionGroups(t, readJSON(t, filepath.Join(dir, relative)))
		if len(groups) != 1 {
			t.Fatalf("%s: %d groups, want the v1 hook replaced in place", client, len(groups))
		}
		command := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
		want, _ := perClient(client)
		if command != want+marker || !strings.HasSuffix(command, "herma-managed:session-start:v2") {
			t.Fatalf("%s command = %q", client, command)
		}
	}
}

func TestInstallUserWritesUserLevelFiles(t *testing.T) {
	home := t.TempDir()
	command := commandForTest(t, "claude-agent")
	installed, err := InstallUser(home, "both", sameCommand(command))
	if err != nil || len(installed) != 2 {
		t.Fatalf("%v %v", installed, err)
	}
	claude := readJSON(t, filepath.Join(home, ".claude", "settings.json"))
	codex := readJSON(t, filepath.Join(home, ".codex", "hooks.json"))
	for name, settings := range map[string]map[string]any{"claude": claude, "codex": codex} {
		groups := settings["hooks"].(map[string]any)["SessionStart"].([]any)
		cmd := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
		if !strings.HasSuffix(cmd, marker) {
			t.Fatalf("%s command %q", name, cmd)
		}
	}
	again, err := InstallUser(home, "both", sameCommand(command))
	if err != nil || again[0].Changed || again[1].Changed {
		t.Fatalf("reinstall changed files: %v %v", again, err)
	}
}

func TestRemoveProjectDeletesOnlyHermasEntry(t *testing.T) {
	dir := t.TempDir()
	if _, err := Install(dir, "both", sameCommand(commandForTest(t, "worker"))); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, ".claude", "settings.local.json")
	settings := readJSON(t, other)
	groups := settings["hooks"].(map[string]any)["SessionStart"].([]any)
	groups = append(groups, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/usr/bin/true"}}})
	settings["hooks"].(map[string]any)["SessionStart"] = groups
	settings["permissions"] = map[string]any{"allow": []any{"Bash(ls)"}}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(other, data, 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveProject(dir, "both")
	if err != nil || !removed[0].Changed || !removed[1].Changed {
		t.Fatalf("%v %v", removed, err)
	}
	after := readJSON(t, other)
	left := after["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(left) != 1 || after["permissions"] == nil {
		t.Fatalf("claude settings after removal: %v", after)
	}
	codex := readJSON(t, filepath.Join(dir, ".codex", "hooks.json"))
	if _, ok := codex["hooks"]; ok {
		t.Fatalf("empty codex hooks should be dropped: %v", codex)
	}
	again, err := RemoveProject(dir, "both")
	if err != nil || again[0].Changed || again[1].Changed {
		t.Fatalf("second removal: %v %v", again, err)
	}
}

func TestRemoveProjectWithoutFilesIsANoOp(t *testing.T) {
	dir := t.TempDir()
	removed, err := RemoveProject(dir, "both")
	if err != nil || removed[0].Changed || removed[1].Changed {
		t.Fatalf("%v %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude")); !os.IsNotExist(err) {
		t.Fatal("created .claude")
	}
}
