package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/rules"
	"github.com/moncho/herma/internal/store"
)

type refreshResult struct {
	Seen    string `json:"seen"`
	Context string `json:"context"`
}

func runRefresh(t *testing.T, dir, seen string) refreshResult {
	t.Helper()
	t.Chdir(dir)
	args := []string{"hook", "refresh"}
	if seen != "" {
		args = append(args, "--seen", seen)
	}
	var result refreshResult
	data := runInput(t, t.Context(), "", args...)
	if err := json.Unmarshal(data, &result); err != nil || result.Seen == "" {
		t.Fatalf("refresh output %s: %v", data, err)
	}
	return result
}

func TestRefreshTellsARunningSessionWhichPrinciplesChanged(t *testing.T) {
	db, p, root := claudeHookFixture(t, 0, nil)
	home := withClaudeHome(t)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Everywhere"})
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Here only", ProjectID: p.ID})

	baseline := runRefresh(t, root, "")
	if baseline.Context != "" {
		t.Fatalf("a baseline announces nothing: %+v", baseline)
	}
	if again := runRefresh(t, root, baseline.Seen); again.Seen != baseline.Seen || again.Context != "" {
		t.Fatalf("nothing changed: %+v", again)
	}

	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Accepted mid-session", ProjectID: p.ID})
	// Another session syncs the files first; this one must still hear of it.
	runSessionHook(t, root, "--client", "claude")
	changed := runRefresh(t, root, baseline.Seen)
	if changed.Seen == baseline.Seen || !strings.Contains(changed.Context, "## Project principles changed") || !strings.Contains(changed.Context, "Accepted mid-session") {
		t.Fatalf("project change: %+v", changed)
	}
	if strings.Contains(changed.Context, "Global principles") || strings.Contains(changed.Context, "Everywhere") {
		t.Fatalf("the global principles did not change: %s", changed.Context)
	}
	// Until the session takes the new state, every refresh repeats the change.
	if repeat := runRefresh(t, root, baseline.Seen); repeat.Context != changed.Context {
		t.Fatalf("repeat: %+v", repeat)
	}

	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Global addition"})
	global := runRefresh(t, root, changed.Seen)
	if !strings.Contains(global.Context, "## Global principles changed") || !strings.Contains(global.Context, "Global addition") || strings.Contains(global.Context, "Project principles") {
		t.Fatalf("global change: %+v", global)
	}
	file, err := os.ReadFile(filepath.Join(home, rules.GlobalPath))
	if err != nil || !strings.Contains(string(file), "Global addition") {
		t.Fatalf("global file %q %v", file, err)
	}
	file, err = os.ReadFile(filepath.Join(root, rules.Path))
	if err != nil || !strings.Contains(string(file), "Accepted mid-session") {
		t.Fatalf("project file %q %v", file, err)
	}
}

func TestRefreshFollowsOnlyGlobalPrinciplesInAnUnboundFolder(t *testing.T) {
	db, _, _ := claudeHookFixture(t, 0, nil)
	withClaudeHome(t)
	dir := unboundFolder(t)
	baseline := runRefresh(t, dir, "")
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Everywhere"})
	changed := runRefresh(t, dir, baseline.Seen)
	if !strings.Contains(changed.Context, "## Global principles changed") || !strings.Contains(changed.Context, "Everywhere") || strings.Contains(changed.Context, "Project") {
		t.Fatalf("%+v", changed)
	}
}
