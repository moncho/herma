package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moncho/herma/internal/api"
	"github.com/moncho/herma/internal/backup"
	"github.com/moncho/herma/internal/hooks"
	"github.com/moncho/herma/internal/project"
	"github.com/moncho/herma/internal/store"
)

func runInput(t *testing.T, ctx context.Context, input string, args ...string) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := RunWithInput(ctx, args, strings.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatalf("%v: %v (%s)", args, err, stderr.String())
	}
	return stdout.Bytes()
}

func hookEvent(t *testing.T, cwd string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{"cwd": cwd, "hook_event_name": "SessionStart", "source": "startup", "session_id": "test-session"})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBoundContextAndSessionStartLoadFreshCoordination(t *testing.T) {
	cleanEnv(t)
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(api.NewHandler(db, []api.Identity{{Name: "worker", Token: "test-token", Role: store.RoleAgent}}))
	defer server.Close()
	t.Setenv("HERMA_URL", server.URL)
	t.Setenv("HERMA_TOKEN", "test-token")
	create := func(in store.CreateInput) store.Record {
		t.Helper()
		r, _, err := db.Create(context.Background(), store.Author{Name: "worker", Role: store.RoleReviewer}, "", in)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	p := create(store.CreateInput{Kind: "project", Title: "Session coordination"})
	create(store.CreateInput{Kind: "knowledge", Title: "Durable wiki fact", ProjectID: p.ID, Status: "accepted", Sources: []string{"https://example.com/review"}})
	create(store.CreateInput{Kind: "task", Title: "Session A edits the API", ProjectID: p.ID, Status: "in_progress", Owner: "session-a", Sources: []string{"https://issues.example/EX-123"}})
	note := create(store.CreateInput{Kind: "note", Title: "Latest handoff", Body: strings.Repeat("引き継ぎ🙂", 500), ProjectID: p.ID})
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "src")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	runInput(t, context.Background(), "", "project", "bind", "--project", p.ID, "--dir", root, "--max-bytes", "4096")
	t.Chdir(nested)
	check := func(data []byte) {
		t.Helper()
		if len(data) > 4096 || !json.Valid(data) {
			t.Fatalf("unbounded/invalid context: %d bytes", len(data))
		}
		var packet struct {
			Project  store.Record     `json:"project"`
			Sections []contextSection `json:"sections"`
			Scope    string           `json:"scope"`
		}
		if err := json.Unmarshal(data, &packet); err != nil {
			t.Fatal(err)
		}
		if packet.Project.ID != p.ID || len(sectionRecords(packet.Sections, "task")) != 1 || len(sectionRecords(packet.Sections, "knowledge")) != 0 || len(sectionRecords(packet.Sections, "note")) != 1 || sectionRecords(packet.Sections, "note")[0].ID != note.ID {
			t.Fatalf("unexpected coordination packet: %s", data)
		}
		if !strings.Contains(packet.Scope, "untrusted") {
			t.Fatal("context must distinguish record data from instructions")
		}
	}
	check(runInput(t, context.Background(), "", "context", "--format", "json"))
	// Session start loads only counts; the records stay behind herma context.
	for _, client := range []string{"claude", "codex"} {
		got := runSessionHook(t, nested, "--client", client)
		if !strings.HasPrefix(got.Context, "herma coordination: Tasks 1 · Notes 1 (latest ") || strings.Contains(got.Context, "Session A") || strings.Contains(got.Context, "handoff") || strings.Contains(got.Context, "Durable") {
			t.Fatalf("%s session start: %+v", client, got)
		}
	}
	create(store.CreateInput{Kind: "note", Title: "Fresh handoff after first startup", ProjectID: p.ID})
	if got := runSessionHook(t, nested, "--client", "codex"); !strings.Contains(got.Context, "Notes 2") {
		t.Fatalf("hook reused stale counts: %+v", got)
	}
	records, err := db.List(context.Background(), store.ListOptions{Archived: true, Limit: 200})
	if err != nil || records.Total != 5 {
		t.Fatalf("read-only startup changed records: %+v, %v", records, err)
	}
}

func TestContextCLIRespectsBudgetAndDurableOptIn(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HERMA_TOKEN", "token")
	id := "rec_" + strings.Repeat("a", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("include_durable") != "true" || r.URL.Query().Get("max_bytes") != "3072" || r.URL.Query().Has("format") || r.URL.Query().Has("principles") {
			t.Errorf("wrong context query: %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"project":{"id":"`+id+`"},"max_bytes":3072,"body":"`+strings.Repeat("x", 2800)+`"}`)
	}))
	defer server.Close()
	data := runInput(t, context.Background(), "", "--url", server.URL, "context", "--project", id, "--max-bytes", "3072", "--include-durable", "--format", "json")
	if len(data) > 3072 || bytes.Count(data, []byte("\n")) != 1 {
		t.Fatal("CLI expanded the server's bounded JSON")
	}
	for _, budget := range []int{0, 2047, 65537} {
		var out, errs bytes.Buffer
		err := Run(context.Background(), []string{"context", "--project", id, "--max-bytes", strconv.Itoa(budget)}, &out, &errs)
		if err == nil || !strings.Contains(err.Error(), "max-bytes") || out.Len() > 0 {
			t.Fatalf("accepted invalid context budget %d: %v", budget, err)
		}
	}
}

// Before the service restarts on a new binary it rejects unknown query
// parameters, so JSON requests must look exactly like they did before.
func TestJSONContextWorksAgainstAServerThatRejectsNewParameters(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HERMA_TOKEN", "token")
	id := "rec_" + strings.Repeat("c", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name := range r.URL.Query() {
			if name != "project_id" && name != "max_bytes" && name != "include_durable" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"unknown query parameter `+name+`"}}`)
				return
			}
		}
		_, _ = io.WriteString(w, `{"project":{"id":"`+id+`"},"max_bytes":`+r.URL.Query().Get("max_bytes")+`,"body":"old server"}`)
	}))
	defer server.Close()
	t.Setenv("HERMA_URL", server.URL)
	data := runInput(t, context.Background(), "", "context", "--format", "json", "--project", id)
	if !bytes.Contains(data, []byte("old server")) {
		t.Fatalf("context failed against an older server: %s", data)
	}
	root := t.TempDir()
	if _, err := project.Bind(root, project.Binding{ProjectID: id}); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Hook struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	out := runInput(t, context.Background(), hookEvent(t, root), "hook", "session-start")
	if err := json.Unmarshal(out, &envelope); err != nil || !strings.Contains(envelope.Hook.Context, "old server") {
		t.Fatalf("hook lost context against an older server: %s", out)
	}
}

func TestSessionStartFailuresAreBoundedAndDoNotBlock(t *testing.T) {
	cleanEnv(t)
	id := "rec_" + strings.Repeat("b", 32)
	root := t.TempDir()
	if _, err := project.Bind(root, project.Binding{ProjectID: id}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMA_TOKEN", "private-test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"private-test-token `+strings.Repeat("remote failure ", 1000)+`"}}`)
	}))
	defer server.Close()
	t.Setenv("HERMA_URL", server.URL)
	unbound := t.TempDir()
	if err := os.Mkdir(filepath.Join(unbound, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, client := range [][]string{nil, {"--client", "claude"}, {"--client", "codex"}} {
		args := append([]string{"hook", "session-start"}, client...)
		for _, input := range []string{hookEvent(t, root), `{`, `{ "hook_event_name":"SessionStart", "cwd":"relative" }`, strings.Repeat("x", maxHookInput+1)} {
			data := runInput(t, context.Background(), input, args...)
			if !json.Valid(data) || !bytes.Contains(data, []byte("systemMessage")) || bytes.Contains(data, []byte("additionalContext")) || bytes.Contains(data, []byte("private-test-token")) || len(data) > 512 {
				t.Fatalf("%v: bad fail-open warning: %s", client, data)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		data := runInput(t, ctx, hookEvent(t, root), args...)
		if !bytes.Contains(data, []byte("systemMessage")) {
			t.Fatalf("%v: canceled request did not fail open", client)
		}
		data = runInput(t, context.Background(), hookEvent(t, unbound), args...)
		if len(data) != 0 {
			t.Fatalf("%v: unbound projects should be a quiet no-op: %s", client, data)
		}
	}
}

func TestHookInstallUsesAbsoluteCredentialsWithoutTokens(t *testing.T) {
	cleanEnv(t)
	root := t.TempDir()
	if _, err := project.Bind(root, project.Binding{ProjectID: "rec_" + strings.Repeat("c", 32)}); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentials, []byte(`{"owner":{"token":"do-not-embed-this-reviewer-token-in-hooks","role":"reviewer"},"session-a":{"token":"do-not-embed-this-token-in-hook-configuration","role":"agent"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	runInput(t, context.Background(), "", "--credentials", credentials, "--identity", "session-a", "hook", "install", "--client", "both", "--dir", root)
	for relative, client := range map[string]string{".claude/settings.json": "claude", ".codex/hooks.json": "codex"} {
		data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), relative))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("do-not-embed-this-token")) || !bytes.Contains(data, []byte(credentials)) || !bytes.Contains(data, []byte("session-a")) || !bytes.Contains(data, []byte("'session-start' '--client' '"+client+"'")) {
			t.Fatalf("wrong generated command: %s", data)
		}
	}
}

func TestHookInstallIsUserLevelAndRemovesProjectHooks(t *testing.T) {
	cleanEnv(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentials, []byte(`{"session-a":{"token":"do-not-embed-this-token-in-hook-configuration","role":"agent"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	if _, err := hooks.Install(checkout, "both", func(string) (string, error) { return "'/bin/herma' hook session-start", nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "--credentials", credentials, "--identity", "session-a", "hook", "install", "--client", "both", "--dir", checkout); err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("HOME")
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "hooks.json")); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{".claude/settings.local.json", ".codex/hooks.json"} {
		data, _ := os.ReadFile(filepath.Join(checkout, relative))
		if strings.Contains(string(data), "herma-managed:session-start") {
			t.Fatalf("project hook left behind in %s: %s", relative, data)
		}
	}
}

func TestHookInstallInHomeKeepsUserCodexHook(t *testing.T) {
	cleanEnv(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentials, []byte(`{"session-a":{"token":"agent-token-that-is-long-enough-for-herma","role":"agent"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("HOME")
	if _, err := runCLI(t, "--credentials", credentials, "--identity", "session-a", "hook", "install", "--client", "both", "--dir", home); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if err != nil || !strings.Contains(string(data), "herma-managed:session-start") {
		t.Fatalf("user-level Codex hook removed: %s %v", data, err)
	}
}

func TestHookRefusesOverBudgetServerResponse(t *testing.T) {
	cleanEnv(t)
	id := "rec_" + strings.Repeat("d", 32)
	root := t.TempDir()
	if _, err := project.Bind(root, project.Binding{ProjectID: id, MaxBytes: 2048}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMA_TOKEN", "token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"project":{"id":"`+id+`"},"max_bytes":2048,"body":"`+strings.Repeat("x", 5000)+`"}`)
	}))
	defer server.Close()
	t.Setenv("HERMA_URL", server.URL)
	data := runInput(t, context.Background(), hookEvent(t, root), "hook", "session-start")
	if !bytes.Contains(data, []byte("systemMessage")) || len(data) > 512 {
		t.Fatal("over-budget response was injected")
	}
}

func TestTokenHookNeverFallsBackToCredentialFile(t *testing.T) {
	cleanEnv(t)
	root := t.TempDir()
	if _, err := project.Bind(root, project.Binding{ProjectID: "rec_" + strings.Repeat("e", 32)}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMA_TOKEN", "private-token-which-must-not-be-written")
	runInput(t, context.Background(), "", "hook", "install", "--client", "claude", "--dir", root)
	config, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(config, []byte("private-token")) || !bytes.Contains(config, []byte("--require-token")) {
		t.Fatalf("token mode was not pinned: %s", config)
	}
	t.Setenv("HERMA_TOKEN", "")
	data := runInput(t, context.Background(), hookEvent(t, root), "hook", "session-start", "--require-token")
	if !bytes.Contains(data, []byte("requires HERMA_TOKEN")) || bytes.Contains(data, []byte("additionalContext")) {
		t.Fatalf("missing token fell back to a different identity: %s", data)
	}
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// sessionStartWithBackups runs the hook against a server whose backup status
// comes from runner; failStatus makes /v1/backup return 500.
func sessionStartWithBackups(t *testing.T, runner *backup.Runner, db *store.Store, failStatus bool) map[string]any {
	t.Helper()
	handler := api.NewHandler(db, []api.Identity{{Name: "worker", Token: "test-token", Role: store.RoleAgent}})
	if runner != nil {
		handler.SetBackupReporter(runner)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failStatus && r.URL.Path == "/v1/backup" {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("HERMA_URL", server.URL)
	t.Setenv("HERMA_TOKEN", "test-token")
	p, _, err := db.Create(context.Background(), store.Author{Name: "worker", Role: store.RoleAgent}, "", store.CreateInput{Kind: "project", Title: "Backed up"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Create(context.Background(), store.Author{Name: "worker", Role: store.RoleAgent}, "", store.CreateInput{Kind: "note", Title: "Handoff", ProjectID: p.ID}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Bind(root, project.Binding{ProjectID: p.ID}); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	out := runInput(t, context.Background(), hookEvent(t, root), "hook", "session-start", "--client", "codex")
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("hook output: %s", out)
	}
	if result["hookSpecificOutput"] == nil {
		t.Fatalf("context missing: %s", out)
	}
	return result
}

func TestSessionStartWarnsOnlyWhenBackupsAreStale(t *testing.T) {
	cleanEnv(t)
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := &testClock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	runner, err := backup.New(db, backup.Config{Dir: t.TempDir(), Every: time.Hour, Keep: 3}, io.Discard, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result := sessionStartWithBackups(t, runner, db, false); result["systemMessage"] != nil {
		t.Fatalf("fresh backups warned: %v", result["systemMessage"])
	}
	clock.Advance(3 * time.Hour)
	result := sessionStartWithBackups(t, runner, db, false)
	message, _ := result["systemMessage"].(string)
	if !strings.Contains(message, "no successful backup since 2026-09-30") {
		t.Fatalf("stale warning: %q", message)
	}
	if result := sessionStartWithBackups(t, nil, db, false); result["systemMessage"] != nil {
		t.Fatalf("disabled backups warned: %v", result["systemMessage"])
	}
	if result := sessionStartWithBackups(t, runner, db, true); result["systemMessage"] != nil {
		t.Fatalf("failed status request warned: %v", result["systemMessage"])
	}
}

func TestContextCLIPrintsTextByDefault(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HERMA_TOKEN", "token")
	id := "rec_" + strings.Repeat("b", 32)
	packet := "herma context · project Demo (" + id + ") · 2026-10-02T09:00Z\nbody\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("format") != "text" || q.Get("principles") != "omit" || q.Get("max_bytes") != "10000" {
			t.Errorf("wrong context query: %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, packet)
	}))
	defer server.Close()
	data := runInput(t, context.Background(), "", "--url", server.URL, "context", "--project", id, "--principles", "omit")
	if string(data) != packet {
		t.Fatalf("CLI output = %q", data)
	}
	for _, args := range [][]string{{"--format", "xml"}, {"--principles", "changed"}} {
		var out, errs bytes.Buffer
		err := Run(context.Background(), append([]string{"--url", server.URL, "context", "--project", id}, args...), &out, &errs)
		if err == nil || out.Len() > 0 {
			t.Fatalf("accepted %v: %v", args, err)
		}
	}
}

func TestContextCLIRejectsIncompatibleTextResponses(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HERMA_TOKEN", "token")
	id := "rec_" + strings.Repeat("e", 32)
	for name, body := range map[string]string{
		"other project": "herma context · project Demo (rec_" + strings.Repeat("f", 32) + ") · now\n",
		"over budget":   "herma context · project Demo (" + id + ") · now\n" + strings.Repeat("x", 3000) + "\n",
		"not context":   "hello\n",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		var out, errs bytes.Buffer
		err := Run(context.Background(), []string{"--url", server.URL, "context", "--project", id, "--max-bytes", "2048"}, &out, &errs)
		server.Close()
		if err == nil || out.Len() > 0 {
			t.Fatalf("%s accepted: %q", name, out.String())
		}
	}
}

func TestHookInstallRegistersClaudePluginUserWide(t *testing.T) {
	startServe(t)
	proj := createAs(t, "local-agent", "--kind", "project", "--title", "Plugin project")
	root := boundCheckout(t, proj.ID)
	if _, err := runCLI(t, "hook", "install", "--client", "claude", "--dir", root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(data), os.Getenv("HERMA_CLAUDE_PLUGIN_DIR")) || !strings.Contains(string(data), "mcp__herma__recall") || !strings.Contains(string(data), "herma-managed:session-start") {
		t.Fatalf("%s %v", data, err)
	}
	var settings struct {
		PluginConfigs map[string]struct {
			Options map[string]string `json:"options"`
		} `json:"pluginConfigs"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if got := settings.PluginConfigs["herma"].Options["url"]; got == "" || got != os.Getenv("HERMA_URL") {
		t.Fatalf("plugin url %q, want %q: %s", got, os.Getenv("HERMA_URL"), data)
	}
	override := os.Getenv("HERMA_CLAUDE_PLUGIN_DIR")
	t.Setenv("HERMA_CLAUDE_PLUGIN_DIR", "")
	if _, err := runCLI(t, "hook", "install", "--client", "claude", "--dir", root); err != nil {
		t.Fatalf("install with the built-in plugin: %v", err)
	}
	builtin := filepath.Join(os.Getenv("HOME"), ".config", "herma", "plugins", "claude")
	data, err = os.ReadFile(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(data), builtin) || strings.Contains(string(data), override) {
		t.Fatalf("built-in plugin not registered: %s %v", data, err)
	}
	for _, file := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.ts"} {
		if _, err := os.Stat(filepath.Join(builtin, file)); err != nil {
			t.Errorf("built-in plugin lacks %s: %v", file, err)
		}
	}
	t.Setenv("HERMA_CLAUDE_PLUGIN_DIR", filepath.Join(t.TempDir(), "missing"))
	if _, err := runCLI(t, "hook", "install", "--client", "claude", "--dir", root); err == nil || !strings.Contains(err.Error(), "Claude Code plugin not found") {
		t.Fatalf("missing plugin: %v", err)
	}
	if _, err := runCLI(t, "hook", "install", "--client", "codex", "--dir", root); err != nil {
		t.Fatalf("codex install must not need the plugin: %v", err)
	}
}
