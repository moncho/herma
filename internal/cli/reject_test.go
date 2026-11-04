package cli

import (
	"encoding/json"
	"testing"
)

type rejectOutput struct {
	Rejected []acceptedRecord `json:"rejected"`
	Failed   []acceptFailure  `json:"failed"`
}

func runReject(t *testing.T, args ...string) (rejectOutput, error) {
	t.Helper()
	data, err := runCLI(t, append([]string{"--identity", "owner", "reject"}, args...)...)
	var out rejectOutput
	if jsonErr := json.Unmarshal(data, &out); jsonErr != nil {
		t.Fatalf("reject output %s: %v (command error %v)", data, jsonErr, err)
	}
	return out, err
}

func TestRejectSeveralProposalsWithOneReason(t *testing.T) {
	startServe(t)
	a := proposePrinciple(t, "First rule")
	b := proposePrinciple(t, "Second rule")
	out, err := runReject(t, a, b, "--reason", "Too specific to one repository")
	if err != nil || len(out.Rejected) != 2 || len(out.Failed) != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	for _, id := range []string{a, b} {
		r := cliRecord(t, "get", id)
		if r.Status != "rejected" || r.ReviewReason != "Too specific to one repository" {
			t.Fatalf("%s status %q reason %q", id, r.Status, r.ReviewReason)
		}
	}
}

func TestRejectWithoutReasonAndAcceptedRecords(t *testing.T) {
	startServe(t)
	accepted := createAs(t, "owner", "--kind", "knowledge", "--title", "Stale fact", "--status", "accepted", "--sources", "https://example.com/old").ID
	out, err := runReject(t, accepted)
	if err != nil || len(out.Rejected) != 1 || out.Rejected[0].ID != accepted {
		t.Fatalf("%+v %v", out, err)
	}
	if r := cliRecord(t, "get", accepted); r.Status != "rejected" || r.ReviewReason != "" {
		t.Fatalf("status %q reason %q", r.Status, r.ReviewReason)
	}
}

func TestRejectReportsEachFailureAndRejectsTheRest(t *testing.T) {
	startServe(t)
	good := proposePrinciple(t, "Rule")
	out, err := runReject(t, "rec_missing", good)
	if err == nil || len(out.Rejected) != 1 || out.Rejected[0].ID != good || len(out.Failed) != 1 || out.Failed[0].ID != "rec_missing" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestRejectUsageErrors(t *testing.T) {
	startServe(t)
	id := proposePrinciple(t, "Rule")
	for name, args := range map[string][]string{
		"nothing":      {},
		"empty reason": {id, "--reason", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := runCLI(t, append([]string{"--identity", "owner", "reject"}, args...)...); err == nil {
				t.Fatal("expected a usage error")
			}
		})
	}
	if r := cliRecord(t, "get", id); r.Status != "proposed" {
		t.Fatalf("status %q", r.Status)
	}
}

func TestRejectNeedsTheReviewer(t *testing.T) {
	startServe(t)
	id := proposePrinciple(t, "Rule")
	data, err := runCLI(t, "--identity", "local-agent", "reject", id)
	if err == nil {
		t.Fatalf("agent rejected: %s", data)
	}
	if r := cliRecord(t, "get", id); r.Status != "proposed" {
		t.Fatalf("status %q", r.Status)
	}
}
