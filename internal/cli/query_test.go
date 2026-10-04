package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func TestListWhereAndSort(t *testing.T) {
	startServe(t)
	def := writeTempFile(t, "bookmark.json", `{"schema":{"rating":{"type":"integer"},"read_at":{"type":"datetime"}},"statuses":["unread"],"policy":{}}`)
	kind := cliRecord(t, "kind", "propose", "bookmark", "--file", def)
	cliRecord(t, "--identity", "owner", "update", kind.ID, "--version", "1", "--status", "accepted")
	cliRecord(t, "create", "--kind", "bookmark", "--title", "low", "--field", "rating=2", "--field", "read_at=2026-10-01T09:00:00Z")
	cliRecord(t, "create", "--kind", "bookmark", "--title", "mid", "--field", "rating=4", "--field", "read_at=2026-10-02T09:00:00Z")
	cliRecord(t, "create", "--kind", "bookmark", "--title", "high", "--field", "rating=5", "--field", "read_at=2026-10-03T09:00:00Z")
	// The +02:00 offset must survive URL encoding: 2026-10-03T11:00+02:00 is 09:00Z.
	data, err := runCLI(t, "list", "--kind", "bookmark", "--where", "rating>=4", "--where", "read_at<=2026-10-03T11:00:00+02:00", "--sort", "-rating")
	if err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	var page store.ListResult
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, r := range page.Items {
		titles = append(titles, r.Title)
	}
	if strings.Join(titles, ",") != "high,mid" {
		t.Fatalf("titles = %v", titles)
	}
	if _, err := runCLI(t, "list", "--kind", "bookmark", "--where", "rating>=four"); err == nil || !strings.Contains(err.Error(), `fields.rating: must be an integer`) {
		t.Fatalf("bad where: %v", err)
	}
}
