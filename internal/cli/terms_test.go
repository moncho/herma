package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTermsTravelFromProposalToRecall drives a real herma serve: an agent
// proposes with --terms, the reviewer sees them, and once the record is
// accepted a query sharing no word with its title or body finds it.
func TestTermsTravelFromProposalToRecall(t *testing.T) {
	startServe(t)
	proposal := createAs(t, "local-agent", "--kind", "principle", "--title", "Keep dates and ETAs out of status updates",
		"--body", "Say what is done and what is open.", "--sources", "session:test", "--terms", "Deadline, estimate,progress report")
	if strings.Join(proposal.Terms, "|") != "deadline|estimate|progress report" {
		t.Fatalf("terms %q", proposal.Terms)
	}
	review, err := runCLI(t, "review")
	if err != nil || !strings.Contains(string(review), "  terms: deadline, estimate, progress report\n") {
		t.Fatalf("review: %s %v", review, err)
	}
	text, err := runCLI(t, "get", proposal.ID, "--format", "text")
	if err != nil || !strings.Contains(string(text), "terms: deadline, estimate, progress report\n") {
		t.Fatalf("get: %s %v", text, err)
	}
	if data, err := runCLI(t, "--identity", "owner", "accept", proposal.ID); err != nil {
		t.Fatalf("accept: %s %v", data, err)
	}
	data, err := runCLI(t, "recall", "deadline in a progress report")
	var recall recallOutput
	if err != nil || json.Unmarshal(data, &recall) != nil || len(recall.Results) != 1 || recall.Results[0].ID != proposal.ID {
		t.Fatalf("recall: %s %v", data, err)
	}
	// The reviewer rewrites or clears the terms of an accepted record.
	if data, err := runCLI(t, "--identity", "owner", "update", proposal.ID, "--version", "2", "--terms", ""); err != nil {
		t.Fatalf("update: %s %v", data, err)
	}
	data, err = runCLI(t, "recall", "deadline in a progress report")
	if err != nil || json.Unmarshal(data, &recall) != nil || len(recall.Results) != 0 {
		t.Fatalf("recall after clearing: %s %v", data, err)
	}
}
