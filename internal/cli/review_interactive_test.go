package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func runCLIWithInput(t *testing.T, input io.Reader, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := RunWithInput(context.Background(), args, input, &stdout, &stderr)
	return stdout.String(), err
}

func runInteractiveReview(t *testing.T, input string) string {
	t.Helper()
	out, err := runCLIWithInput(t, strings.NewReader(input), "--identity", "owner", "review", "-i")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return out
}

// queueOrder returns the proposed principles in the order review shows them.
func queueOrder(t *testing.T) []string {
	t.Helper()
	data, err := runCLI(t, "review", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range reviewQueue(t, data)["principles"].Items {
		ids = append(ids, r.ID)
	}
	return ids
}

func assertStatus(t *testing.T, id, status string) {
	t.Helper()
	if r := cliRecord(t, "get", id); r.Status != status {
		t.Fatalf("%s status %q, want %q", id, r.Status, status)
	}
}

func TestInteractiveReviewAcceptsRejectsAndSkips(t *testing.T) {
	startServe(t)
	proposePrinciple(t, "First rule", "--sources", "https://example.com/a")
	proposePrinciple(t, "Second rule", "--sources", "https://example.com/b")
	proposePrinciple(t, "Third rule", "--sources", "https://example.com/c")
	order := queueOrder(t)
	a, b, c := order[0], order[1], order[2]
	out := runInteractiveReview(t, "a\nr\ntoo specific\ns\n")
	assertStatus(t, a, "accepted")
	if r := cliRecord(t, "get", b); r.Status != "rejected" || r.ReviewReason != "too specific" {
		t.Fatalf("%+v", r)
	}
	assertStatus(t, c, "proposed")
	for _, want := range []string{"First rule", "Second rule", "Third rule", "accepted 1 · rejected 1 · skipped 1 · failed 0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestInteractiveReviewQuitAndEOFKeepDecisions(t *testing.T) {
	startServe(t)
	proposePrinciple(t, "First rule", "--sources", "https://example.com/a")
	proposePrinciple(t, "Second rule", "--sources", "https://example.com/b")
	order := queueOrder(t)
	runInteractiveReview(t, "a\nq\n")
	assertStatus(t, order[0], "accepted")
	assertStatus(t, order[1], "proposed")
	proposePrinciple(t, "Third rule", "--sources", "https://example.com/c")
	order = queueOrder(t)
	out := runInteractiveReview(t, "s\n")
	assertStatus(t, order[0], "proposed")
	assertStatus(t, order[1], "proposed")
	if !strings.Contains(out, "accepted 0 · rejected 0 · skipped 1 · failed 0 · left 1") {
		t.Fatalf("summary after EOF:\n%s", out)
	}
}

func TestInteractiveReviewAsksAgainForEmptyReasonAndUnknownKey(t *testing.T) {
	startServe(t)
	id := proposePrinciple(t, "Only rule")
	out := runInteractiveReview(t, "x\nr\n\nno\n")
	if r := cliRecord(t, "get", id); r.Status != "rejected" || r.ReviewReason != "no" {
		t.Fatalf("%+v", r)
	}
	if strings.Count(out, "[a]ccept [r]eject [s]kip [q]uit: ") != 2 || strings.Count(out, "reason: ") != 2 {
		t.Fatalf("prompts not repeated:\n%s", out)
	}
}

// editingReader lets an agent edit a record when the review first reads
// input, after the record was shown.
type editingReader struct {
	t     *testing.T
	id    string
	input io.Reader
	done  bool
}

func (r *editingReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		cliRecord(r.t, "update", r.id, "--version", "1", "--title", "Changed rule")
	}
	return r.input.Read(p)
}

func TestInteractiveReviewReportsConflictAndContinues(t *testing.T) {
	startServe(t)
	proposePrinciple(t, "First rule", "--sources", "https://example.com/a")
	proposePrinciple(t, "Second rule", "--sources", "https://example.com/b")
	// An agent edits the second item while the reviewer reads the first.
	order := queueOrder(t)
	second, first := order[0], order[1]
	title := cliRecord(t, "get", first).Title
	out, err := runCLIWithInput(t, &editingReader{t: t, id: first, input: strings.NewReader("a\na\n")}, "--identity", "owner", "review", "-i")
	if err == nil {
		t.Fatalf("a failed decision exited zero:\n%s", out)
	}
	assertStatus(t, first, "proposed")
	assertStatus(t, second, "accepted")
	if !strings.Contains(out, first+" · "+title+": changed since it was shown") || !strings.Contains(out, "failed 1") {
		t.Fatalf("conflict not reported:\n%s", out)
	}
}

func TestInteractiveReviewHandlesKindsAndPendingChanges(t *testing.T) {
	startServe(t)
	def := writeTempFile(t, "k.json", `{"schema":{"url":{"type":"url"}},"statuses":["a"],"policy":{}}`)
	proposed := cliRecord(t, "kind", "propose", "proposed_kind", "--file", def)
	changing := cliRecord(t, "kind", "propose", "changing_kind", "--file", def)
	cliRecord(t, "--identity", "owner", "update", changing.ID, "--version", "1", "--status", "accepted")
	next := writeTempFile(t, "k2.json", `{"schema":{"url":{"type":"url"},"note":{"type":"text"}},"statuses":["a"],"policy":{}}`)
	cliRecord(t, "kind", "change", changing.ID, "--file", next)
	out := runInteractiveReview(t, "r\nnot needed\nr\n")
	if r := cliRecord(t, "get", proposed.ID); r.Status != "rejected" || r.ReviewReason != "not needed" {
		t.Fatalf("%+v", r)
	}
	r := cliRecord(t, "get", changing.ID)
	if _, ok := r.Fields["pending"]; ok || r.Status != "accepted" {
		t.Fatalf("pending change not discarded: %+v", r)
	}
	if !strings.Contains(out, `"note"`) {
		t.Fatalf("pending definition not shown:\n%s", out)
	}
}

func TestInteractiveReviewWithEmptyQueue(t *testing.T) {
	startServe(t)
	if out := runInteractiveReview(t, ""); out != "Nothing to review.\n" {
		t.Fatalf("%q", out)
	}
}

func TestInteractiveReviewRefusesJSONAndOffset(t *testing.T) {
	startServe(t)
	for _, extra := range [][]string{{"--format", "json"}, {"--offset", "1"}} {
		if _, err := runCLIWithInput(t, strings.NewReader(""), append([]string{"--identity", "owner", "review", "-i"}, extra...)...); err == nil || !strings.Contains(err.Error(), "review -i takes no --format or --offset") {
			t.Fatalf("review -i %v: %v", extra, err)
		}
	}
}
