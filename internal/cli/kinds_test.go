package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// cliRecord runs a herma command and decodes the record it prints.
func cliRecord(t *testing.T, args ...string) store.Record {
	t.Helper()
	data, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	var r store.Record
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return r
}

// pendingKindChange reports whether herma review lists the kind under
// pending_kind_changes.
func pendingKindChange(t *testing.T, id string) bool {
	t.Helper()
	data, err := runCLI(t, "review")
	if err != nil {
		t.Fatalf("review: %s %v", data, err)
	}
	var queue struct {
		Pending []store.Record `json:"pending_kind_changes"`
	}
	if err := json.Unmarshal(data, &queue); err != nil {
		t.Fatal(err)
	}
	for _, r := range queue.Pending {
		if r.ID == id {
			return true
		}
	}
	return false
}

func TestCustomKindWorkflow(t *testing.T) {
	startServe(t)
	def := writeTempFile(t, "bookmark.json", `{"schema":{"url":{"type":"url","required":true,"unique":true},"authors":{"type":"string-list"},"note":{"type":"string"}},"statuses":["unread","done"],"policy":{"recall":true}}`)
	kind := cliRecord(t, "kind", "propose", "bookmark", "--file", def, "--body", "Things to read")
	if kind.Status != "proposed" || kind.Kind != "kind" {
		t.Fatalf("proposed: %+v", kind)
	}
	queue, err := runCLI(t, "review")
	if err != nil || !strings.Contains(string(queue), kind.ID) {
		t.Fatalf("review queue: %s %v", queue, err)
	}
	cliRecord(t, "--identity", "owner", "update", kind.ID, "--version", "1", "--status", "accepted")
	b := cliRecord(t, "create", "--kind", "bookmark", "--title", "WAL internals", "--field", "url=https://sqlite.org/wal.html", "--field", "authors=Hipp, Kennedy", "--field", "note=one, two")
	if b.Fields["note"] != "one, two" || len(b.Fields["authors"].([]any)) != 2 {
		t.Fatalf("fields: %+v", b.Fields)
	}
	if out, err := runCLI(t, "create", "--kind", "bookmark", "--title", "Again", "--field", "url=https://sqlite.org/wal.html"); err == nil || !strings.Contains(err.Error(), "already used by "+b.ID) {
		t.Fatalf("duplicate: %s %v", out, err)
	}
	updated := cliRecord(t, "update", b.ID, "--version", "1", "--field", "note=", "--status", "done")
	if _, ok := updated.Fields["note"]; ok || updated.Status != "done" {
		t.Fatalf("remove field: %+v", updated)
	}
	recall, err := runCLI(t, "recall", "kennedy")
	if err != nil || !strings.Contains(string(recall), b.ID) || !strings.Contains(string(recall), `"reviewed":false`) {
		t.Fatalf("recall: %s %v", recall, err)
	}
	next := writeTempFile(t, "v2.json", `{"schema":{"url":{"type":"url","required":true,"unique":true},"authors":{"type":"string-list"},"note":{"type":"string"},"rating":{"type":"integer","min":1,"max":5}},"statuses":["unread","done"],"policy":{"recall":true}}`)
	changed := cliRecord(t, "kind", "change", kind.ID, "--file", next)
	if changed.Fields["pending"] == nil {
		t.Fatalf("pending: %+v", changed)
	}
	if !pendingKindChange(t, kind.ID) {
		t.Fatal("kind with a pending change is missing from pending_kind_changes")
	}
	accepted := cliRecord(t, "--identity", "owner", "update", kind.ID, "--version", strconv.FormatInt(changed.Version, 10), "--accept-pending")
	if accepted.Fields["pending"] != nil {
		t.Fatalf("accept pending: %+v", accepted)
	}
	if pendingKindChange(t, kind.ID) {
		t.Fatal("accepted kind is still in pending_kind_changes")
	}
	cliRecord(t, "update", b.ID, "--version", "2", "--field", "rating=4")
	list, err := runCLI(t, "kind", "list")
	if err != nil || !strings.Contains(string(list), `"name": "bookmark"`) || !strings.Contains(string(list), `"status": "accepted"`) {
		t.Fatalf("kind list: %s %v", list, err)
	}
	fieldsFile := writeTempFile(t, "fields.json", `{"url":"https://other.example","authors":["A"]}`)
	cliRecord(t, "create", "--kind", "bookmark", "--title", "From file", "--fields-file", fieldsFile)
	if _, err := runCLI(t, "create", "--kind", "bookmark", "--title", "Both", "--fields-file", fieldsFile, "--field", "note=x"); err == nil {
		t.Fatal("--field and --fields-file together were accepted")
	}
}

func TestReviewListsPendingChangesOfRetiredKinds(t *testing.T) {
	startServe(t)
	def := writeTempFile(t, "paper.json", `{"schema":{"doi":{"type":"string"}},"statuses":["unread"],"policy":{}}`)
	kind := cliRecord(t, "kind", "propose", "paper", "--file", def)
	accepted := cliRecord(t, "--identity", "owner", "update", kind.ID, "--version", "1", "--status", "accepted")
	retired := cliRecord(t, "--identity", "owner", "update", kind.ID, "--version", strconv.FormatInt(accepted.Version, 10), "--status", "retired")
	next := writeTempFile(t, "paper2.json", `{"schema":{"doi":{"type":"string"},"year":{"type":"integer"}},"statuses":["unread"],"policy":{}}`)
	if changed := cliRecord(t, "kind", "change", kind.ID, "--file", next); changed.Fields["pending"] == nil || retired.Status != "retired" {
		t.Fatalf("pending on retired kind: %+v", changed)
	}
	if !pendingKindChange(t, kind.ID) {
		t.Fatal("retired kind with a pending change is missing from pending_kind_changes")
	}
}
