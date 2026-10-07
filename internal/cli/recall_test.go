package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/project"
	"github.com/moncho/herma/internal/store"
)

type recallOutput struct {
	ProjectID string `json:"project_id"`
	Results   []struct {
		ID   string `json:"id"`
		Rank int    `json:"rank"`
	} `json:"results"`
}

func createAs(t *testing.T, identity string, args ...string) store.Record {
	t.Helper()
	data, err := runCLI(t, append([]string{"--identity", identity, "create"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	var r store.Record
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecallUsesBindingAndSearchesEverythingWithoutOne(t *testing.T) {
	startServe(t)
	here := createAs(t, "local-agent", "--kind", "project", "--title", "Here")
	elsewhere := createAs(t, "local-agent", "--kind", "project", "--title", "Elsewhere")
	mine := createAs(t, "owner", "--kind", "knowledge", "--title", "Sprocket tuning", "--project", here.ID, "--status", "accepted", "--sources", "https://example.com/review")
	theirs := createAs(t, "owner", "--kind", "knowledge", "--title", "Sprocket tuning", "--project", elsewhere.ID, "--status", "accepted", "--sources", "https://example.com/review")

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Bind(root, project.Binding{ProjectID: here.ID}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	data, err := runCLI(t, "recall", "sprocket", "--max-bytes", "4096")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 4096 || strings.Count(string(data), "\n") != 1 {
		t.Fatalf("output is not compact or exceeds budget: %d bytes", len(data))
	}
	var bound recallOutput
	if err := json.Unmarshal(data, &bound); err != nil || bound.ProjectID != here.ID || len(bound.Results) != 1 || bound.Results[0].ID != mine.ID {
		t.Fatalf("bound recall: %s %v", data, err)
	}

	t.Chdir(t.TempDir())
	data, err = runCLI(t, "recall", "sprocket")
	var all recallOutput
	if err != nil || json.Unmarshal(data, &all) != nil || all.ProjectID != "" || len(all.Results) != 2 {
		t.Fatalf("unbound recall: %s %v (theirs %s)", data, err, theirs.ID)
	}
}

func TestRecallCommandRejectsBadInput(t *testing.T) {
	startServe(t)
	for _, args := range [][]string{{"recall"}, {"recall", "--limit", "5"}, {"recall", "!!"}, {"recall", "x1", "--limit", "0"}, {"recall", "x1", "--max-bytes", "100"}} {
		if _, err := runCLI(t, args...); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}

func TestRecallCommandAsksToQuoteMultiWordQueries(t *testing.T) {
	startServe(t)
	_, err := runCLI(t, "recall", "snapshot", "pruning")
	if err == nil || err.Error() != `quote multi-word queries: herma recall "snapshot pruning"` {
		t.Errorf("unquoted words: %v", err)
	}
	// Flags after the query are still parsed as flags.
	if _, err := runCLI(t, "recall", "snapshot", "--limit", "0"); err == nil || strings.Contains(err.Error(), "quote") {
		t.Errorf("flag after query: %v", err)
	}
}

func TestCreateUsesBindingForKindsThatNeedAProject(t *testing.T) {
	startServe(t)
	here := createAs(t, "local-agent", "--kind", "project", "--title", "Here")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Bind(root, project.Binding{ProjectID: here.ID}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	for _, kind := range []string{"task", "feedback", "note"} {
		if r := createAs(t, "local-agent", "--kind", kind, "--title", "Bound "+kind); r.ProjectID != here.ID {
			t.Errorf("%s project = %q, want the binding's %q", kind, r.ProjectID, here.ID)
		}
	}
	for _, kind := range []string{"knowledge", "principle", "project"} {
		if r := createAs(t, "local-agent", "--kind", kind, "--title", "Global "+kind); r.ProjectID != "" {
			t.Errorf("%s project = %q, want none", kind, r.ProjectID)
		}
	}
	if r := createAs(t, "local-agent", "--kind", "note", "--title", "Opted out", "--project", ""); r.ProjectID != "" {
		t.Errorf("explicit empty --project kept %q", r.ProjectID)
	}

	t.Chdir(t.TempDir())
	if r := createAs(t, "local-agent", "--kind", "note", "--title", "Unbound"); r.ProjectID != "" {
		t.Errorf("unbound note project = %q, want none", r.ProjectID)
	}
}
