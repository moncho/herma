package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/project"
)

type statusOutput struct {
	Bound     bool   `json:"bound"`
	ProjectID string `json:"project_id"`
	Server    string `json:"server"`
	Identity  string `json:"identity"`
	Role      string `json:"role"`
	Review    struct {
		Knowledge          int `json:"knowledge"`
		Principles         int `json:"principles"`
		Kinds              int `json:"kinds"`
		PendingKindChanges int `json:"pending_kind_changes"`
		Total              int `json:"total"`
	} `json:"review"`
	Backup struct {
		Enabled       bool   `json:"enabled"`
		LastSuccessAt string `json:"last_success_at"`
		AgeSeconds    *int64 `json:"age_seconds"`
		Stale         bool   `json:"stale"`
	} `json:"backup"`
}

func boundCheckout(t *testing.T, projectID string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Bind(root, project.Binding{ProjectID: projectID}); err != nil {
		t.Fatal(err)
	}
	return root
}

func runStatus(t *testing.T, args ...string) statusOutput {
	t.Helper()
	data, err := runCLI(t, append(args, "status")...)
	if err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	var out statusOutput
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return out
}

func rawBackup(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := runCLI(t, "status")
	if err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	var raw struct {
		Backup map[string]json.RawMessage `json:"backup"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return raw.Backup
}

func TestStatusUnboundNeedsNoServer(t *testing.T) {
	cleanEnv(t)
	t.Chdir(t.TempDir())
	data, err := runCLI(t, "--url", "http://127.0.0.1:1", "status")
	if err != nil || string(data) != "{\n  \"bound\": false\n}\n" {
		t.Fatalf("%q %v", data, err)
	}
}

func TestStatusCountsReviewQueue(t *testing.T) {
	startServe(t)
	proj := createAs(t, "local-agent", "--kind", "project", "--title", "Status project")
	t.Chdir(boundCheckout(t, proj.ID))
	createAs(t, "local-agent", "--kind", "knowledge", "--title", "Proposed fact")
	createAs(t, "local-agent", "--kind", "principle", "--title", "Proposed rule")
	def := writeTempFile(t, "k.json", `{"schema":{"url":{"type":"url"}},"statuses":["a"],"policy":{}}`)
	cliRecord(t, "kind", "propose", "proposed_kind", "--file", def)
	accepted := cliRecord(t, "kind", "propose", "changing_kind", "--file", def)
	accepted = cliRecord(t, "--identity", "owner", "update", accepted.ID, "--version", "1", "--status", "accepted")
	next := writeTempFile(t, "k2.json", `{"schema":{"url":{"type":"url"},"note":{"type":"text"}},"statuses":["a"],"policy":{}}`)
	cliRecord(t, "kind", "change", accepted.ID, "--file", next)

	out := runStatus(t)
	if !out.Bound || out.ProjectID != proj.ID || out.Server != "ok" || out.Identity != "local-agent" || out.Role != "agent" {
		t.Fatalf("status: %+v", out)
	}
	r := out.Review
	if r.Knowledge != 1 || r.Principles != 1 || r.Kinds != 1 || r.PendingKindChanges != 1 || r.Total != 4 {
		t.Fatalf("review: %+v", r)
	}
	if out.Backup.Enabled {
		t.Fatalf("backup should be off without --backup-dir: %+v", out.Backup)
	}
	if raw := rawBackup(t); len(raw) != 1 || string(raw["enabled"]) != "false" {
		t.Fatalf("disabled backup should be exactly {\"enabled\": false}: %v", raw)
	}
}

func TestStatusReportsBackups(t *testing.T) {
	_, _, stop := startServeWith(t, []string{"--backup-dir", t.TempDir()})
	defer stop()
	proj := createAs(t, "local-agent", "--kind", "project", "--title", "Backup project")
	t.Chdir(boundCheckout(t, proj.ID))
	if out := runStatus(t); !out.Backup.Enabled {
		t.Fatalf("backup: %+v", out.Backup)
	}
	if raw := rawBackup(t); string(raw["stale"]) != "false" {
		t.Fatalf("enabled backup must report stale: %v", raw)
	}
}

func TestStatusFailsWhenServerIsDown(t *testing.T) {
	startServe(t)
	proj := createAs(t, "local-agent", "--kind", "project", "--title", "Down project")
	t.Chdir(boundCheckout(t, proj.ID))
	data, err := runCLI(t, "--url", "http://127.0.0.1:1", "status")
	if err == nil {
		t.Fatal("status succeeded against a closed port")
	}
	if !strings.Contains(err.Error(), "not running or reachable at http://127.0.0.1:1") {
		t.Fatalf("want a connection failure, got: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("stdout should be empty, got %q", data)
	}
}
