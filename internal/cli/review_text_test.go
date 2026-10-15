package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func createdID(t *testing.T, args ...string) string {
	t.Helper()
	data, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("%v: %s %v", args, data, err)
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func TestReviewPrintsProposalsAsText(t *testing.T) {
	startServe(t)
	project := createdID(t, "create", "--kind", "project", "--title", "producerflow/mono")
	fact := createdID(t, "create", "--kind", "knowledge", "--project", project, "--title", "Spanner trap",
		"--body", "First line.\nSecond line.", "--sources", "https://example.com/a,memo:b")
	rule := createdID(t, "create", "--kind", "principle", "--title", "Global rule", "--body", "Do it.")
	data, err := runCLI(t, "review")
	if err != nil {
		t.Fatalf("%s %v", data, err)
	}
	got := string(data)
	for _, want := range []string{
		"herma review · 2 proposals\n",
		"\nPrinciples\n" + rule + " · global · by local-agent · ",
		"\nGlobal rule\n  Do it.\n",
		"\nKnowledge\n" + fact + " · producerflow/mono · by local-agent · ",
		"\nSpanner trap\n  First line.\n  Second line.\n  sources: https://example.com/a, memo:b\n",
		"Accept on the server: herma --identity owner accept ID [ID ...]\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("review lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"project_id"`) || strings.Index(got, "Principles") > strings.Index(got, "Knowledge") {
		t.Errorf("not text, or sections out of order:\n%s", got)
	}
}

func TestReviewTextSaysWhenNothingWaits(t *testing.T) {
	startServe(t)
	data, err := runCLI(t, "review")
	if err != nil {
		t.Fatalf("%s %v", data, err)
	}
	if string(data) != "Nothing to review.\n" {
		t.Fatalf("got %q", data)
	}
}

func TestReviewTextCountsWhatTheLimitLeftOut(t *testing.T) {
	startServe(t)
	for _, title := range []string{"One", "Two", "Three"} {
		createdID(t, "create", "--kind", "knowledge", "--title", title)
	}
	data, err := runCLI(t, "review", "--limit", "1")
	if err != nil {
		t.Fatalf("%s %v", data, err)
	}
	if !strings.Contains(string(data), "herma review · 3 proposals\n") || !strings.Contains(string(data), "2 more knowledge not shown: herma review --offset 1\n") {
		t.Fatalf("got:\n%s", data)
	}
}

func TestReviewRejectsAnUnknownFormat(t *testing.T) {
	startServe(t)
	if data, err := runCLI(t, "review", "--format", "yaml"); err == nil {
		t.Fatalf("accepted yaml: %s", data)
	}
}
