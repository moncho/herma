package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func runUsageJSON(t *testing.T, args ...string) store.UsageResult {
	t.Helper()
	data, err := runCLI(t, append([]string{"usage", "--format", "json"}, args...)...)
	if err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	var result store.UsageResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return result
}

func TestUsageShowsLeastUsedFirst(t *testing.T) {
	startServe(t)
	t.Chdir(t.TempDir())
	used := createAs(t, "owner", "--kind", "knowledge", "--title", "Sprocket tuning", "--status", "accepted", "--sources", "https://example.com/review")
	unused := createAs(t, "owner", "--kind", "principle", "--title", "Prefer simple", "--status", "accepted", "--sources", "https://example.com/review")
	if _, err := runCLI(t, "recall", "sprocket"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := runCLI(t, "get", used.ID); err != nil {
			t.Fatal(err)
		}
	}
	// The reviewer's own reads are not uses.
	if _, err := runCLI(t, "--identity", "owner", "get", unused.ID); err != nil {
		t.Fatal(err)
	}

	result := runUsageJSON(t)
	if result.Total != 2 || len(result.Items) != 2 || result.Items[0].ID != unused.ID || result.Items[1].ID != used.ID {
		t.Fatalf("usage: %+v, want %s then %s", result, unused.ID, used.ID)
	}
	if got := result.Items[1]; got.Recalled != 1 || got.Fetched != 2 || got.LastUsedAt == nil {
		t.Errorf("used record: %+v, want recalled 1 fetched 2", got)
	}
	if got := result.Items[0]; got.Recalled != 0 || got.Fetched != 0 || got.LastUsedAt != nil {
		t.Errorf("unused record: %+v", got)
	}

	data, err := runCLI(t, "usage")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("text output has %d lines, want a header and two records:\n%s", len(lines), data)
	}
	today := used.CreatedAt.UTC().Format("2006-01-02")
	for i, want := range [][]string{
		{"RECALLED", "FETCHED", "LAST USED", "ACCEPTED", "ID", "KIND", "TITLE"},
		{"0", "0", "never", today, unused.ID, "principle", "Prefer", "simple"},
		{"1", "2", today, today, used.ID, "knowledge", "Sprocket", "tuning"},
	} {
		if got := strings.Fields(lines[i]); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("line %d: %q, want fields %q", i, lines[i], want)
		}
	}
}

func TestUsageUsesBindingAndReportsTruncation(t *testing.T) {
	startServe(t)
	here := createAs(t, "local-agent", "--kind", "project", "--title", "Here")
	elsewhere := createAs(t, "local-agent", "--kind", "project", "--title", "Elsewhere")
	mine := createAs(t, "owner", "--kind", "knowledge", "--title", "Mine", "--project", here.ID, "--status", "accepted", "--sources", "https://example.com/review")
	createAs(t, "owner", "--kind", "knowledge", "--title", "Theirs", "--project", elsewhere.ID, "--status", "accepted", "--sources", "https://example.com/review")
	global := createAs(t, "owner", "--kind", "principle", "--title", "Global", "--status", "accepted", "--sources", "https://example.com/review")

	t.Chdir(boundCheckout(t, here.ID))
	bound := runUsageJSON(t)
	if bound.Total != 2 || len(bound.Items) != 2 {
		t.Fatalf("bound usage: %+v, want %s and %s", bound, mine.ID, global.ID)
	}
	if other := runUsageJSON(t, "--project", elsewhere.ID); other.Total != 2 || other.Items[0].ID == mine.ID || other.Items[1].ID == mine.ID {
		t.Errorf("--project did not override the binding: %+v", other)
	}

	t.Chdir(t.TempDir())
	if all := runUsageJSON(t); all.Total != 3 {
		t.Errorf("unbound usage lists %d records, want 3", all.Total)
	}
	data, err := runCLI(t, "usage", "--limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "1 of 3 records; raise --limit (up to 200) to see more\n") {
		t.Errorf("no truncation line:\n%s", data)
	}
}

func TestUsageWithNothingToReportAndBadInput(t *testing.T) {
	startServe(t)
	t.Chdir(t.TempDir())
	data, err := runCLI(t, "usage")
	if err != nil || string(data) != "No accepted records.\n" {
		t.Errorf("empty usage: %q %v", data, err)
	}
	for _, args := range [][]string{{"usage", "--format", "xml"}, {"usage", "--limit", "0"}, {"usage", "--limit", "201"}, {"usage", "extra"}} {
		if _, err := runCLI(t, args...); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}
