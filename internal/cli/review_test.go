package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/project"
	"github.com/moncho/herma/internal/store"
)

// reviewQueue decodes the paged lists in the review output, leaving out
// pending_kind_changes, which is a plain array.
func reviewQueue(t *testing.T, data []byte) map[string]store.ListResult {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "pending_kind_changes")
	queue := map[string]store.ListResult{}
	for key, value := range raw {
		var page store.ListResult
		if err := json.Unmarshal(value, &page); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		queue[key] = page
	}
	return queue
}

func TestReviewListsProposedDurableRecordsWithPagination(t *testing.T) {
	startServe(t)
	for _, args := range [][]string{
		{"create", "--kind", "knowledge", "--title", "Proposed fact one"},
		{"create", "--kind", "knowledge", "--title", "Proposed fact two"},
		{"create", "--kind", "principle", "--title", "Proposed principle"},
		{"--identity", "owner", "create", "--kind", "knowledge", "--title", "Accepted fact", "--status", "accepted", "--sources", "https://example.com/review"},
	} {
		if _, err := runCLI(t, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	data, err := runCLI(t, "review")
	if err != nil {
		t.Fatal(err)
	}
	queue := reviewQueue(t, data)
	if queue["knowledge"].Total != 2 || queue["principles"].Total != 1 {
		t.Fatalf("queue totals: %s", data)
	}
	for _, item := range append(queue["knowledge"].Items, queue["principles"].Items...) {
		if item.Status != "proposed" {
			t.Fatalf("queue contains %s record %s", item.Status, item.Title)
		}
	}
	data, err = runCLI(t, "review", "--limit", "1", "--offset", "1")
	if err != nil {
		t.Fatalf("paged review: %s %v", data, err)
	}
	queue = reviewQueue(t, data)
	if len(queue["knowledge"].Items) != 1 || queue["knowledge"].Total != 2 {
		t.Fatalf("paged review: %s", data)
	}
	if _, err := runCLI(t, "review", "--limit", "0"); err == nil {
		t.Fatal("accepted limit 0")
	}
}

func TestHookInstallRefusesReviewerAndAdvisesPermissions(t *testing.T) {
	cleanEnv(t)
	root := t.TempDir()
	if _, err := project.Bind(root, project.Binding{ProjectID: "rec_" + strings.Repeat("c", 32)}); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := initCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, "--credentials", credentials, "--identity", "owner", "hook", "install", "--client", "claude", "--dir", root)
	if err == nil || !strings.Contains(err.Error(), "agent identity") {
		t.Fatalf("reviewer hook install: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".claude")); !os.IsNotExist(statErr) {
		t.Fatal("refused install still wrote hook configuration")
	}
	data, err := runCLI(t, "--credentials", credentials, "--identity", "local-agent", "hook", "install", "--client", "claude", "--dir", root)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("install output: %s %v", data, err)
	}
	if advice, _ := result["permission_advice"].(string); !strings.Contains(advice, "Read(/"+credentials+")") {
		t.Fatalf("permission advice: %s %v", data, err)
	}
	// Once the reviewer lives in its own file, the agent file needs no rule.
	if _, err := revokeIdentity(credentials, "owner"); err != nil {
		t.Fatal(err)
	}
	data, err = runCLI(t, "--credentials", credentials, "--identity", "local-agent", "hook", "install", "--client", "claude", "--dir", root)
	if err != nil {
		t.Fatal(err)
	}
	result = nil
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("install output: %s %v", data, err)
	}
	if _, ok := result["permission_advice"]; ok {
		t.Fatalf("advised a deny rule for a file without a reviewer: %s", data)
	}
}

func TestSessionStartWithReviewerWarnsAndContinues(t *testing.T) {
	cleanEnv(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Bind(root, project.Binding{ProjectID: "rec_" + strings.Repeat("c", 32)}); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := initCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	out := runInput(t, context.Background(), hookEvent(t, root), "--credentials", credentials, "--identity", "owner", "hook", "session-start")
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("hook output is not JSON: %s", out)
	}
	message, _ := result["systemMessage"].(string)
	if !strings.Contains(message, "Reinstall") || result["hookSpecificOutput"] != nil {
		t.Fatalf("reviewer session start: %s", out)
	}
}
