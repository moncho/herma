package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

type acceptOutput struct {
	Accepted   []acceptedRecord `json:"accepted"`
	Superseded []acceptedRecord `json:"superseded"`
	Failed     []acceptFailure  `json:"failed"`
}

func runAccept(t *testing.T, args ...string) (acceptOutput, error) {
	t.Helper()
	data, err := runCLI(t, append([]string{"--identity", "owner", "accept"}, args...)...)
	var out acceptOutput
	if jsonErr := json.Unmarshal(data, &out); jsonErr != nil {
		t.Fatalf("accept output %s: %v (command error %v)", data, jsonErr, err)
	}
	return out, err
}

func proposePrinciple(t *testing.T, title string, extra ...string) string {
	t.Helper()
	args := append([]string{"--kind", "principle", "--title", title}, extra...)
	return createAs(t, "local-agent", args...).ID
}

func TestAcceptSeveralProposalsAtOnce(t *testing.T) {
	startServe(t)
	a := proposePrinciple(t, "First rule", "--sources", "https://example.com/a")
	b := proposePrinciple(t, "Second rule", "--sources", "https://example.com/b")
	out, err := runAccept(t, a, b)
	if err != nil || len(out.Accepted) != 2 || len(out.Failed) != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	for _, id := range []string{a, b} {
		if r := cliRecord(t, "get", id); r.Status != "accepted" {
			t.Fatalf("%s status %q", id, r.Status)
		}
	}
}

func TestAcceptReplacesSupersedesOnlyAfterAccepting(t *testing.T) {
	startServe(t)
	old := createAs(t, "owner", "--kind", "principle", "--title", "Old rule", "--status", "accepted", "--sources", "https://example.com/old").ID
	replacement := proposePrinciple(t, "New rule", "--sources", "https://example.com/new")
	out, err := runAccept(t, replacement, "--replaces", old)
	if err != nil || len(out.Accepted) != 1 || len(out.Superseded) != 1 || out.Superseded[0].ID != old {
		t.Fatalf("%+v %v", out, err)
	}
	if r := cliRecord(t, "get", old); r.Status != "superseded" {
		t.Fatalf("old status %q", r.Status)
	}
	if r := cliRecord(t, "get", replacement); r.Status != "accepted" {
		t.Fatalf("new status %q", r.Status)
	}
}

func TestAcceptKeepsTheOldRecordWhenTheReplacementFails(t *testing.T) {
	startServe(t)
	old := createAs(t, "owner", "--kind", "principle", "--title", "Old rule", "--status", "accepted", "--sources", "https://example.com/old").ID
	unsourced := proposePrinciple(t, "New rule without sources")
	out, err := runAccept(t, unsourced, "--replaces", old)
	if err == nil || len(out.Failed) != 1 || len(out.Superseded) != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	if !strings.Contains(out.Failed[0].Error, "--sources") {
		t.Fatalf("failure should say how to add sources: %q", out.Failed[0].Error)
	}
	if r := cliRecord(t, "get", old); r.Status != "accepted" {
		t.Fatalf("old status %q", r.Status)
	}
}

func TestAcceptReportsEachFailureAndAcceptsTheRest(t *testing.T) {
	startServe(t)
	good := proposePrinciple(t, "Good rule", "--sources", "https://example.com/good")
	bad := proposePrinciple(t, "Rule without sources")
	out, err := runAccept(t, bad, good)
	if err == nil || len(out.Accepted) != 1 || out.Accepted[0].ID != good || len(out.Failed) != 1 || out.Failed[0].ID != bad {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestAcceptAllTakesTheReviewQueue(t *testing.T) {
	startServe(t)
	principle := proposePrinciple(t, "Queued rule", "--sources", "https://example.com/p")
	knowledge := createAs(t, "local-agent", "--kind", "knowledge", "--title", "Queued fact", "--sources", "https://example.com/k").ID
	out, err := runAccept(t, "--all")
	if err != nil || len(out.Accepted) != 2 {
		t.Fatalf("%+v %v", out, err)
	}
	for _, id := range []string{principle, knowledge} {
		if r := cliRecord(t, "get", id); r.Status != "accepted" {
			t.Fatalf("%s status %q", id, r.Status)
		}
	}
}

func TestAcceptRejectsAmbiguousArguments(t *testing.T) {
	startServe(t)
	a := proposePrinciple(t, "A", "--sources", "https://example.com/a")
	b := proposePrinciple(t, "B", "--sources", "https://example.com/b")
	for name, args := range map[string][]string{
		"nothing":            {},
		"all with ids":       {"--all", a},
		"replaces with many": {a, b, "--replaces", a},
		"replaces with all":  {"--all", "--replaces", a},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := runCLI(t, append([]string{"--identity", "owner", "accept"}, args...)...); err == nil {
				t.Fatal("expected a usage error")
			}
		})
	}
}

func TestAcceptNeedsTheReviewer(t *testing.T) {
	startServe(t)
	id := proposePrinciple(t, "Rule", "--sources", "https://example.com/r")
	data, err := runCLI(t, "--identity", "local-agent", "accept", id)
	if err == nil {
		t.Fatalf("agent accepted: %s", data)
	}
	if r := cliRecord(t, "get", id); r.Status != "proposed" {
		t.Fatalf("status %q", r.Status)
	}
}
