package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/rules"
	"github.com/moncho/herma/internal/store"
)

func unboundFolder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func withClaudeHome(t *testing.T) string {
	t.Helper()
	home := os.Getenv("HOME")
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestClaudeHookWritesGlobalPrinciplesInAnUnboundFolder(t *testing.T) {
	db, _, _ := claudeHookFixture(t, 0, nil)
	home := withClaudeHome(t)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Everywhere"})
	first := runSessionHook(t, unboundFolder(t), "--client", "claude")
	file, err := os.ReadFile(filepath.Join(home, rules.GlobalPath))
	if err != nil || !strings.Contains(string(file), "## Everywhere") {
		t.Fatalf("global file %q %v", file, err)
	}
	if !strings.Contains(first.Context, "## Global principles changed") || first.Message != "" {
		t.Fatalf("first: %+v", first)
	}
	data := runInput(t, t.Context(), hookEvent(t, unboundFolder(t)), "hook", "session-start", "--client", "claude")
	if strings.TrimSpace(string(data)) != "" {
		t.Fatalf("unchanged unbound run should print nothing: %s", data)
	}
}

func TestCodexHookWritesTheManagedBlockInAnUnboundFolder(t *testing.T) {
	db, _, _ := claudeHookFixture(t, 0, nil)
	codex := filepath.Join(os.Getenv("HOME"), ".codex")
	if err := os.Mkdir(codex, 0o755); err != nil {
		t.Fatal(err)
	}
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Everywhere"})
	runSessionHook(t, unboundFolder(t), "--client", "codex")
	data, err := os.ReadFile(filepath.Join(codex, "AGENTS.md"))
	if err != nil || !strings.Contains(string(data), rules.CodexStart) || !strings.Contains(string(data), "## Everywhere") {
		t.Fatalf("AGENTS.md %q %v", data, err)
	}
}

func TestBoundHookLoadsPrinciplesAndCountsButNoRecords(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	home := withClaudeHome(t)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Everywhere"})
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Here only", ProjectID: p.ID})
	reviewed(t, db, store.CreateInput{Kind: "task", Title: "Secret task text", ProjectID: p.ID})
	reviewed(t, db, store.CreateInput{Kind: "note", Title: "Secret handoff", ProjectID: p.ID})
	got := runSessionHook(t, root, "--client", "claude")
	project, _ := os.ReadFile(filepath.Join(root, rules.Path))
	global, _ := os.ReadFile(filepath.Join(home, rules.GlobalPath))
	if !strings.Contains(string(project), "Here only") || strings.Contains(string(project), "Everywhere") || !strings.Contains(string(global), "Everywhere") {
		t.Fatalf("project:\n%s\nglobal:\n%s", project, global)
	}
	if !strings.Contains(got.Context, "herma coordination: Tasks 1 · Notes 1") || strings.Contains(got.Context, "Secret") {
		t.Fatalf("context:\n%s", got.Context)
	}
	codex := runSessionHook(t, root, "--client", "codex")
	if !strings.Contains(codex.Context, "## Project principles\n") || !strings.Contains(codex.Context, "Here only") || strings.Contains(codex.Context, "Secret") {
		t.Fatalf("codex context:\n%s", codex.Context)
	}
}

func TestClaudeHookPointsToTheFileWhenChangedGlobalsDoNotFit(t *testing.T) {
	db, _, root := claudeHookFixture(t, 0, nil)
	home := withClaudeHome(t)
	for i := 0; i < 30; i++ {
		reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Rule " + strings.Repeat("x", i+1), Body: strings.Repeat("long ", 120)})
	}
	got := runSessionHook(t, root, "--client", "claude")
	if len(got.Context) > claudeContextLimit {
		t.Fatalf("context %d chars, over the cap", len(got.Context))
	}
	if !strings.Contains(got.Context, "are too long to repeat here; read "+filepath.Join(home, rules.GlobalPath)) {
		t.Fatalf("context:\n%s", got.Context[:min(400, len(got.Context))])
	}
}

func TestOldServerLeavesGlobalFilesAndWarnsOnlyWhenBound(t *testing.T) {
	old := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if (r.URL.Path == "/v1/principles" && q.Get("project_id") == "") || q.Get("format") == "summary" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"project_id is required"}}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	_, _, root := claudeHookFixture(t, 0, old)
	home := withClaudeHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "rules", "herma"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := rules.Header + " -->\n# Global principles (reviewed in herma)\n\n## Kept\n"
	if err := os.WriteFile(filepath.Join(home, rules.GlobalPath), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	data := runInput(t, t.Context(), hookEvent(t, unboundFolder(t)), "hook", "session-start", "--client", "claude")
	if strings.TrimSpace(string(data)) != "" {
		t.Fatalf("unbound must stay silent: %s", data)
	}
	bound := runSessionHook(t, root, "--client", "claude")
	if !strings.Contains(bound.Message, "update the herma service") || strings.Contains(bound.Context, "herma coordination") {
		t.Fatalf("bound: %+v", bound)
	}
	if got, _ := os.ReadFile(filepath.Join(home, rules.GlobalPath)); string(got) != existing {
		t.Fatal("global file changed")
	}
}

func TestUnboundHookIsSilentWhenTheServerIsDown(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HERMA_URL", "http://127.0.0.1:1")
	t.Setenv("HERMA_TOKEN", "test-token")
	data := runInput(t, t.Context(), hookEvent(t, unboundFolder(t)), "hook", "session-start", "--client", "claude")
	if strings.TrimSpace(string(data)) != "" {
		t.Fatalf("output %s", data)
	}
}

func TestUnwritableGlobalFileWarnsOnlyWhenBound(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	home := withClaudeHome(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(home, ".claude", "rules")); err != nil {
		t.Fatal(err)
	}
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Everywhere"})
	reviewed(t, db, store.CreateInput{Kind: "task", Title: "Open task", ProjectID: p.ID})
	data := runInput(t, t.Context(), hookEvent(t, unboundFolder(t)), "hook", "session-start", "--client", "claude")
	if strings.TrimSpace(string(data)) != "" {
		t.Fatalf("unbound must stay silent: %s", data)
	}
	bound := runSessionHook(t, root, "--client", "claude")
	if !strings.Contains(bound.Message, "could not update "+filepath.Join(home, rules.GlobalPath)) || !strings.Contains(bound.Context, "herma coordination: Tasks 1") || strings.Contains(bound.Context, "Global principles") {
		t.Fatalf("bound: %+v", bound)
	}
}
