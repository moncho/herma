package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func TestBoundHookLoadsTheIndex(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	withClaudeHome(t)
	reviewed(t, db, store.CreateInput{Kind: "task", Title: "Events API v2", ProjectID: p.ID})
	reviewed(t, db, store.CreateInput{Kind: "note", Title: "Secret handoff", ProjectID: p.ID})
	for _, client := range []string{"claude", "codex"} {
		got := runSessionHook(t, root, "--client", client)
		if !strings.Contains(got.Context, "herma index · ") || !strings.Contains(got.Context, "Events API v2") ||
			!strings.Contains(got.Context, "Also in herma: Notes 1") || strings.Contains(got.Context, "Secret handoff") {
			t.Fatalf("%s context:\n%s", client, got.Context)
		}
	}
}

func rejectFormat(format string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("format") == format {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"format must be json, text or summary"}}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func TestHookFallsBackToTheSummaryOnAnOldServer(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, rejectFormat("index"))
	withClaudeHome(t)
	reviewed(t, db, store.CreateInput{Kind: "task", Title: "Secret task", ProjectID: p.ID})
	got := runSessionHook(t, root, "--client", "claude")
	if !strings.Contains(got.Context, "herma coordination: Tasks 1") || strings.Contains(got.Context, "Secret task") {
		t.Fatalf("context:\n%s", got.Context)
	}
}

func TestHookIgnoresAnIndexOfTheWrongShape(t *testing.T) {
	packet := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("format") == "index" {
				q := r.URL.Query()
				q.Set("format", "text")
				r.URL.RawQuery = q.Encode()
			}
			next.ServeHTTP(w, r)
		})
	}
	db, p, root := claudeHookFixture(t, 0, packet)
	withClaudeHome(t)
	reviewed(t, db, store.CreateInput{Kind: "task", Title: "Task title", Body: "Secret body text", ProjectID: p.ID})
	got := runSessionHook(t, root, "--client", "claude")
	if strings.Contains(got.Context, "Secret body text") || !strings.Contains(got.Context, "herma coordination: Tasks 1") {
		t.Fatalf("context:\n%s", got.Context)
	}
}

func TestIndexGivesWayToChangedPrinciplesInClaude(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	withClaudeHome(t)
	for i := 0; i < 14; i++ {
		reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Rule " + strings.Repeat("r", i+1), Body: strings.Repeat("word ", 90)})
	}
	for i := 0; i < 60; i++ {
		reviewed(t, db, store.CreateInput{Kind: "task", Title: "Open effort " + strings.Repeat("t", 40), ProjectID: p.ID})
	}
	got := runSessionHook(t, root, "--client", "claude")
	if len(got.Context) > claudeContextLimit {
		t.Fatalf("context %d chars, over the cap", len(got.Context))
	}
	if !strings.Contains(got.Context, "## Global principles changed") || strings.Contains(got.Context, "too long to repeat") {
		t.Fatalf("principles gave way to the index:\n%s", got.Context[:min(300, len(got.Context))])
	}
	if !strings.Contains(got.Context, "herma coordination: Tasks 60") || strings.Contains(got.Context, "herma index · ") {
		t.Fatalf("index did not fall back to the count line:\n%s", got.Context[len(got.Context)-min(300, len(got.Context)):])
	}
}
