package cli

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

// TestReviewWorkflowAcrossRoles drives a real herma serve through the CLI: an
// agent proposes, a read-only session finds the proposal, the reviewer accepts
// it over the socket, and only then does it reach durable context.
func TestReviewWorkflowAcrossRoles(t *testing.T) {
	startServe(t, identitySpec{name: "reader", role: store.RoleReadOnly})
	decode := func(data []byte, err error) store.Record {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var record store.Record
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		return record
	}
	project := decode(runCLI(t, "create", "--kind", "project", "--title", "Memory project", "--status", "active"))
	proposal := decode(runCLI(t, "create", "--kind", "knowledge", "--project", project.ID, "--title", "SQLite runs in WAL mode", "--sources", "https://example.com/wal"))
	decode(runCLI(t, "create", "--kind", "knowledge", "--project", project.ID, "--title", "Unreviewed guess"))
	if proposal.Status != "proposed" || proposal.CreatedBy != "local-agent" {
		t.Fatalf("proposal: %+v", proposal)
	}

	data, err := runCLI(t, "--identity", "reader", "list", "--kind", "knowledge", "--q", "wal")
	var found store.ListResult
	if err != nil || json.Unmarshal(data, &found) != nil || found.Total != 1 || found.Items[0].Status != "proposed" {
		t.Fatalf("read-only search: %s %v", data, err)
	}
	if _, err := runCLI(t, "--identity", "reader", "create", "--kind", "note", "--title", "Read-only write"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("read-only write: %v", err)
	}

	accepted := decode(runCLI(t, "--identity", "owner", "update", proposal.ID, "--version", strconv.FormatInt(proposal.Version, 10), "--status", "accepted"))
	if accepted.ReviewedBy != "owner" || accepted.ReviewedAt == nil {
		t.Fatalf("accepted: %+v", accepted)
	}
	if _, err := runCLI(t, "update", proposal.ID, "--version", strconv.FormatInt(accepted.Version, 10), "--body", "Agent rewrite"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("agent edit of accepted record: %v", err)
	}

	data, err = runCLI(t, "context", "--project", project.ID, "--include-durable", "--format", "json")
	var packet struct {
		Sections []contextSection `json:"sections"`
	}
	if err != nil || json.Unmarshal(data, &packet) != nil || len(sectionRecords(packet.Sections, "knowledge")) != 1 || sectionRecords(packet.Sections, "knowledge")[0].ID != proposal.ID {
		t.Fatalf("durable context: %s %v", data, err)
	}
}
