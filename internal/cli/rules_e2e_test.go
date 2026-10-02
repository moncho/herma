package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/project"
	"github.com/moncho/herma/internal/rules"
)

func TestServedPrinciplesReachClaudeThroughTheRulesFile(t *testing.T) {
	startServe(t)
	proj := createAs(t, "local-agent", "--kind", "project", "--title", "Rules e2e")
	createAs(t, "owner", "--kind", "principle", "--project", proj.ID, "--title", "Run the race detector", "--status", "accepted", "--sources", "https://example.com/review")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Bind(root, project.Binding{ProjectID: proj.ID}); err != nil {
		t.Fatal(err)
	}
	hook := func() string {
		var out struct {
			Hook struct {
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		data := runInput(t, context.Background(), hookEvent(t, root), "hook", "session-start", "--client", "claude")
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("hook output: %s", data)
		}
		return out.Hook.Context
	}
	if first := hook(); !strings.Contains(first, "## Principles changed") || !strings.Contains(first, "Run the race detector") {
		t.Fatalf("first session:\n%s", first)
	}
	file, err := os.ReadFile(filepath.Join(root, rules.Path))
	if err != nil || !strings.Contains(string(file), "## Run the race detector") {
		t.Fatalf("rules file: %s %v", file, err)
	}
	if second := hook(); strings.Contains(second, "## Principles") || !strings.Contains(second, "herma recall") {
		t.Fatalf("second session:\n%s", second)
	}
}
