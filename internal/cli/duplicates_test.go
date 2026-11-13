package cli

import (
	"strings"
	"testing"
)

func acceptedPrinciple(t *testing.T, title string) string {
	t.Helper()
	return createAs(t, "owner", "--kind", "principle", "--title", title, "--status", "accepted", "--sources", "https://example.com/"+strings.ReplaceAll(title, " ", "-")).ID
}

func TestAcceptRefusesALikelyDuplicate(t *testing.T) {
	startServe(t)
	old := acceptedPrinciple(t, "Never add Claude attribution to commits or PRs")
	proposal := proposePrinciple(t, "Never add Claude attribution to commits, PRs or messages", "--sources", "https://example.com/new")
	out, err := runAccept(t, v1(proposal))
	if err == nil || len(out.Accepted) != 0 || len(out.Failed) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if !strings.Contains(out.Failed[0].Error, old) || !strings.Contains(out.Failed[0].Error, "--replaces") {
		t.Fatalf("failure should name the duplicate and the fix: %q", out.Failed[0].Error)
	}
	assertStatus(t, proposal, "proposed")
}

func TestAcceptReplacesTheRecordItDuplicates(t *testing.T) {
	startServe(t)
	old := acceptedPrinciple(t, "Write PR summaries as short prose, not bullets")
	proposal := proposePrinciple(t, "Write PR summaries as short prose, not bullets", "--sources", "https://example.com/new")
	out, err := runAccept(t, v1(proposal), "--replaces", old)
	if err != nil || len(out.Accepted) != 1 || len(out.Superseded) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	assertStatus(t, old, "superseded")
	assertStatus(t, proposal, "accepted")
}

func TestAcceptIgnoresRecordsThatMerelyShareWords(t *testing.T) {
	startServe(t)
	acceptedPrinciple(t, "Never add Claude attribution to commits or PRs")
	proposal := proposePrinciple(t, "Never commit specs, plans or process artifacts", "--sources", "https://example.com/new")
	if out, err := runAccept(t, v1(proposal)); err != nil || len(out.Accepted) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestInteractiveReviewOffersToSupersedeADuplicate(t *testing.T) {
	startServe(t)
	old := acceptedPrinciple(t, "Write PR summaries as short prose, not bullets")
	proposal := proposePrinciple(t, "Write PR summaries as short prose", "--sources", "https://example.com/new")
	out := runInteractiveReview(t, "a\ny\n")
	if !strings.Contains(out, "looks like a duplicate of accepted "+old) || !strings.Contains(out, "supersedes "+old) {
		t.Fatalf("output:\n%s", out)
	}
	assertStatus(t, old, "superseded")
	assertStatus(t, proposal, "accepted")
}

func TestInteractiveReviewCanKeepBothWhenAsked(t *testing.T) {
	startServe(t)
	old := acceptedPrinciple(t, "Write PR summaries as short prose, not bullets")
	proposal := proposePrinciple(t, "Write PR summaries as short prose", "--sources", "https://example.com/new")
	runInteractiveReview(t, "a\nn\n")
	assertStatus(t, old, "accepted")
	assertStatus(t, proposal, "accepted")
}

func TestTitleSimilarity(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		dup  bool
	}{
		{"Write PR summaries as short prose, not bullets", "Write PR summaries as short prose, not bullets", true},
		{"Never add Claude attribution to commits or PRs", "Never add Claude attribution to commits, PRs or messages", true},
		{"Never add Claude attribution to commits or PRs", "Never commit specs, plans or process artifacts", false},
		{"Ask before changing the environment", "Prefer simple, single-source solutions", false},
	} {
		if got := titleSimilarity(titleWords(tc.a), titleWords(tc.b)) >= duplicateThreshold; got != tc.dup {
			t.Errorf("%q vs %q: duplicate %v, want %v", tc.a, tc.b, got, tc.dup)
		}
	}
}
