package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/api"
	"github.com/moncho/herma/internal/project"
	"github.com/moncho/herma/internal/rules"
	"github.com/moncho/herma/internal/store"
)

type hookResult struct{ Context, Message string }

func claudeHookFixture(t *testing.T, maxBytes int, wrap func(http.Handler) http.Handler) (*store.Store, store.Record, string) {
	t.Helper()
	cleanEnv(t)
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var handler http.Handler = api.NewHandler(db, []api.Identity{{Name: "worker", Token: "test-token", Role: store.RoleAgent}})
	if wrap != nil {
		handler = wrap(handler)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("HERMA_URL", server.URL)
	t.Setenv("HERMA_TOKEN", "test-token")
	p := reviewed(t, db, store.CreateInput{Kind: "project", Title: "Rules project"})
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Bind(root, project.Binding{ProjectID: p.ID, MaxBytes: maxBytes}); err != nil {
		t.Fatal(err)
	}
	return db, p, root
}

func reviewed(t *testing.T, db *store.Store, in store.CreateInput) store.Record {
	t.Helper()
	if in.Kind == "principle" {
		in.Status, in.Sources = "accepted", []string{"https://example.com/review"}
	}
	r, _, err := db.Create(context.Background(), store.Author{Name: "owner", Role: store.RoleReviewer}, "", in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func runSessionHook(t *testing.T, root string, args ...string) hookResult {
	t.Helper()
	var out struct {
		Hook struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
		Message string `json:"systemMessage"`
	}
	data := runInput(t, context.Background(), hookEvent(t, root), append([]string{"hook", "session-start"}, args...)...)
	if len(data) == 0 {
		return hookResult{}
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("hook output %s: %v", data, err)
	}
	return hookResult{out.Hook.Context, out.Message}
}

func TestClaudeHookWritesPrinciplesAndAnnouncesOnlyChanges(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Never commit to main", ProjectID: p.ID})
	first := runSessionHook(t, root, "--client", "claude")
	file, err := os.ReadFile(filepath.Join(root, rules.Path))
	if err != nil || !strings.Contains(string(file), "## Never commit to main") {
		t.Fatalf("rules file: %s %v", file, err)
	}
	resolved, _ := filepath.EvalSymlinks(root)
	if !strings.HasPrefix(first.Context, "## Project principles changed\nherma project principles changed after this session loaded them; this version replaces "+filepath.Join(resolved, rules.Path)+".\n") || !strings.Contains(first.Context, "Never commit to main") {
		t.Fatalf("first packet does not announce the change:\n%s", first.Context)
	}
	before, _ := os.Stat(filepath.Join(root, rules.Path))
	second := runSessionHook(t, root, "--client", "claude")
	after, _ := os.Stat(filepath.Join(root, rules.Path))
	if second != (hookResult{}) || !os.SameFile(before, after) {
		t.Fatalf("unchanged principles were resent or rewritten:\n%s", second.Context)
	}
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Open PRs as drafts", ProjectID: p.ID})
	if third := runSessionHook(t, root, "--client", "claude"); !strings.Contains(third.Context, "Open PRs as drafts") || !strings.Contains(third.Context, "## Project principles changed") {
		t.Fatalf("new principle not announced:\n%s", third.Context)
	}
}

func TestClaudeHookFallsBackWhenTheRulesPathIsUnsafe(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Keep rules safe", ProjectID: p.ID})
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(root, ".claude")); err != nil {
		t.Fatal(err)
	}
	got := runSessionHook(t, root, "--client", "claude")
	if !strings.HasPrefix(got.Context, "## Project principles\n") || !strings.Contains(got.Context, "Keep rules safe") || !strings.Contains(got.Message, "could not write "+rules.Path) {
		t.Fatalf("no fallback: %+v", got)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("wrote through the symlink: %v", entries)
	}
}

func TestClaudeHookReplacesAStaleRulesFileItCannotUpdate(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Old rule", ProjectID: p.ID})
	runSessionHook(t, root, "--client", "claude")
	path := filepath.Join(root, rules.Path)
	old, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(old), "## Old rule") {
		t.Fatalf("rules file: %s %v", old, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "rules", "herma", ".gitignore"), []byte("custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "New rule", ProjectID: p.ID})
	got := runSessionHook(t, root, "--client", "claude")
	resolved, _ := filepath.EvalSymlinks(path)
	if !strings.HasPrefix(got.Context, "## Project principles\nherma could not update "+resolved+"; these principles replace it.\n") || !strings.Contains(got.Context, "New rule") || !strings.Contains(got.Message, "could not write "+rules.Path) {
		t.Fatalf("stale rules file not replaced in context: %+v", got)
	}
	if now, _ := os.ReadFile(path); string(now) != string(old) {
		t.Fatalf("rules file changed:\n%s", now)
	}
}

func TestClaudeHookDisownsAStaleRulesFileWhenNoPrinciplesRemain(t *testing.T) {
	_, _, root := claudeHookFixture(t, 0, nil)
	if err := os.Symlink(t.TempDir(), filepath.Join(root, ".claude")); err != nil {
		t.Fatal(err)
	}
	got := runSessionHook(t, root, "--client", "claude")
	want := "; no herma project principles apply now, so disregard the ones loaded from it."
	if !strings.HasPrefix(got.Context, "## Project principles\nherma could not update ") || !strings.Contains(got.Context, want) || strings.Contains(got.Context, "replace it") || !strings.Contains(got.Message, "could not write "+rules.Path) {
		t.Fatalf("empty fallback: %+v", got)
	}
}

func TestClaudeHookWarnsAboutLongRulesFiles(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Long rule", Body: strings.Repeat("line\n", 250), ProjectID: p.ID})
	if got := runSessionHook(t, root, "--client", "claude"); !strings.Contains(got.Message, "under 200 lines") {
		t.Fatalf("message = %q", got.Message)
	}
}

func TestClaudeHookLeavesRulesFileWhenServerLacksPrinciples(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		// An older herma server without the route answers from its default route.
		{"older server", http.StatusNotFound, `{"error":{"code":"not_found","message":"endpoint not found"}}` + "\n"},
		{"stale token", http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"a valid bearer token is required"}}` + "\n"},
		{"unavailable", http.StatusServiceUnavailable, "unavailable\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, root := claudeHookFixture(t, 0, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/principles" {
						if strings.HasPrefix(tc.body, "{") {
							w.Header().Set("Content-Type", "application/json; charset=utf-8")
						}
						w.WriteHeader(tc.status)
						_, _ = w.Write([]byte(tc.body))
						return
					}
					next.ServeHTTP(w, r)
				})
			})
			if _, err := rules.Sync(root, []byte(rules.Header+"old rules\n")); err != nil {
				t.Fatal(err)
			}
			got := runSessionHook(t, root, "--client", "claude")
			if got.Context != "" || !strings.Contains(got.Message, "herma context unavailable") {
				t.Fatalf("failure not reported: %+v", got)
			}
			if data, _ := os.ReadFile(filepath.Join(root, rules.Path)); string(data) != rules.Header+"old rules\n" {
				t.Fatalf("rules file changed: %q", data)
			}
		})
	}
}

func TestClaudeHookRemovesTheRulesFileOfAnArchivedProject(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Archived rule", ProjectID: p.ID})
	runSessionHook(t, root, "--client", "claude")
	path := filepath.Join(root, rules.Path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("rules file not written: %v", err)
	}
	archived := true
	if _, _, err := db.Update(context.Background(), p.ID, store.Author{Name: "owner", Role: store.RoleReviewer}, "", store.UpdateInput{Version: p.Version, Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	got := runSessionHook(t, root, "--client", "claude")
	if got.Context != "" || !strings.Contains(got.Message, "herma context unavailable") {
		t.Fatalf("archived project not reported: %+v", got)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("rules file of an archived project kept: %v", err)
	}
}

func TestCodexHookIncludesPrinciplesWithoutWritingFiles(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Codex rule", ProjectID: p.ID})
	got := runSessionHook(t, root, "--client", "codex")
	if !strings.HasPrefix(got.Context, "## Project principles\n"+rules.Header) || !strings.Contains(got.Context, "Codex rule") || strings.Contains(got.Context, "changed") {
		t.Fatalf("codex packet:\n%s", got.Context)
	}
	if _, err := os.Lstat(filepath.Join(root, ".claude")); !os.IsNotExist(err) {
		t.Fatal("codex hook wrote Claude rules")
	}
}

func TestLegacyHookKeepsJSONAndAsksForReinstall(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Legacy rule", ProjectID: p.ID})
	got := runSessionHook(t, root)
	if !json.Valid([]byte(got.Context)) || !strings.Contains(got.Context, "Legacy rule") || !strings.Contains(got.Message, "herma hook install") {
		t.Fatalf("legacy hook: %+v", got)
	}
	if _, err := os.Lstat(filepath.Join(root, ".claude")); !os.IsNotExist(err) {
		t.Fatal("legacy hook wrote Claude rules")
	}
}

func TestSessionStartRejectsUnknownClients(t *testing.T) {
	cleanEnv(t)
	if err := Run(context.Background(), []string{"hook", "session-start", "--client", "cursor"}, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("accepted an unknown client")
	}
}

func TestClaudeHookKeepsPrinciplesInContextForAHomeDirectoryBinding(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	t.Setenv("HOME", root)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Home rule", ProjectID: p.ID})
	got := runSessionHook(t, root, "--client", "claude")
	if _, err := os.Lstat(filepath.Join(root, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("wrote user-wide Claude rules: %v", err)
	}
	if !strings.HasPrefix(got.Context, "## Project principles\n"+rules.Header) || !strings.Contains(got.Context, "Home rule") || !strings.Contains(got.Message, "home directory") {
		t.Fatalf("home binding: %+v", got)
	}
}

func TestLegacyHookClampsTheOldDefault(t *testing.T) {
	db, p, root := claudeHookFixture(t, legacyDefaultMaxBytes, nil)
	for i := 0; i < 60; i++ {
		reviewed(t, db, store.CreateInput{Kind: "note", Title: "Handoff", Body: strings.Repeat("detail ", 100), ProjectID: p.ID})
	}
	got := runSessionHook(t, root)
	if len(got.Context) > claudeContextLimit || !json.Valid([]byte(got.Context)) {
		t.Fatalf("legacy context %d bytes, want at most %d", len(got.Context), claudeContextLimit)
	}
}

// Inline project principles that do not fit must not point Claude at a rules
// file herma never wrote (home binding) or could not update (unsafe path).
func TestClaudeHookPointsOversizedInlinePrinciplesAtHermaPrinciples(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, root string){
		"home directory": func(t *testing.T, root string) { t.Setenv("HOME", root) },
		"unwritable rules file": func(t *testing.T, root string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(root, ".claude")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			db, p, root := claudeHookFixture(t, 0, nil)
			setup(t, root)
			for i := 0; i < 30; i++ {
				reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Rule " + strings.Repeat("x", i+1), Body: strings.Repeat("long ", 120), ProjectID: p.ID})
			}
			got := runSessionHook(t, root, "--client", "claude")
			if len(got.Context) > claudeContextLimit {
				t.Fatalf("context %d chars, over the cap", len(got.Context))
			}
			if !strings.Contains(got.Context, "## Project principles\nherma project principles are too long to repeat here; read them with: herma principles --project "+p.ID+" before relying on them.") || strings.Contains(got.Context, rules.Path) {
				t.Fatalf("context:\n%s", got.Context)
			}
		})
	}
}
