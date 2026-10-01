package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/project"
)

// TestSessionsGetPrinciplesAndRecallKnowledge drives a real herma serve: accepted
// principles reach the session-start hook with the recall hint, knowledge does
// not, and herma recall finds the knowledge first.
func TestSessionsGetPrinciplesAndRecallKnowledge(t *testing.T) {
	startServe(t)
	proj := createAs(t, "local-agent", "--kind", "project", "--title", "Retrieval project")
	createAs(t, "owner", "--kind", "principle", "--project", proj.ID, "--title", "Always run the race detector", "--status", "accepted", "--sources", "https://example.com/review")
	knowledge := createAs(t, "owner", "--kind", "knowledge", "--project", proj.ID, "--title", "Snapshot pruning keeps fourteen files", "--status", "accepted", "--sources", "https://example.com/review")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Bind(root, project.Binding{ProjectID: proj.ID}); err != nil {
		t.Fatal(err)
	}
	var hook struct {
		Output struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	out := runInput(t, context.Background(), hookEvent(t, root), "hook", "session-start")
	if err := json.Unmarshal(out, &hook); err != nil {
		t.Fatalf("hook output: %s", out)
	}
	if !strings.Contains(hook.Output.Context, "Always run the race detector") || !strings.Contains(hook.Output.Context, "herma recall") || strings.Contains(hook.Output.Context, "Snapshot pruning") {
		t.Fatalf("session context: %s", hook.Output.Context)
	}
	t.Chdir(root)
	data, err := runCLI(t, "recall", "pruning snapshots")
	var recall recallOutput
	if err != nil || json.Unmarshal(data, &recall) != nil || len(recall.Results) == 0 || recall.Results[0].ID != knowledge.ID || recall.Results[0].Rank != 1 {
		t.Fatalf("recall: %s %v", data, err)
	}
}
