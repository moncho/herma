package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRecallFindsARecordByItsSearchTerms(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	other := accepted(t, s, "knowledge", "Progress notes", "A weekly progress report goes to the team.", "")
	r, _, err := s.Create(ctx, reviewer("owner"), "", CreateInput{
		Kind: "principle", Title: "Keep dates and ETAs out of status updates", Body: "Say what is done and what is open.",
		Status: "accepted", Sources: recallSources, Terms: []string{" Deadline ", "estimate", "progress report", "deadline"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"deadline", "estimate", "progress report"}; !reflect.DeepEqual(r.Terms, want) {
		t.Fatalf("terms %q, want %q", r.Terms, want)
	}
	if got := recallIDs(t, s, RecallOptions{Query: "deadline estimate in progress report"}); len(got) != 2 || got[0] != r.ID || got[1] != other.ID {
		t.Fatalf("recall %v, want %s first", got, r.ID)
	}

	none := []string{}
	updated, _, err := s.Update(ctx, r.ID, reviewer("owner"), "", UpdateInput{Version: r.Version, Terms: &none})
	if err != nil {
		t.Fatal(err)
	}
	if got := recallIDs(t, s, RecallOptions{Query: "deadline"}); len(got) != 0 {
		t.Fatalf("cleared terms still match: %v", got)
	}
	data, err := json.Marshal(updated)
	if err != nil || strings.Contains(string(data), `"terms"`) {
		t.Fatalf("a record without terms carries none: %s %v", data, err)
	}

	if _, _, err := s.Create(ctx, reviewer("owner"), "", CreateInput{Kind: "note", Title: "Too many", Terms: distinctWords(maxTerms + 1)}); err == nil {
		t.Fatal("more than the maximum number of terms accepted")
	}
}

func TestAgentsSetTermsOnlyWhileARecordIsProposed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p, _, err := s.Create(ctx, agent("worker"), "", CreateInput{Kind: "knowledge", Title: "Cache keys", Body: "Hash the tenant.", Sources: recallSources, Terms: []string{"memoize"}})
	if err != nil {
		t.Fatal(err)
	}
	terms := []string{"memoize", "lookup"}
	if p, _, err = s.Update(ctx, p.ID, agent("worker"), "", UpdateInput{Version: p.Version, Terms: &terms}); err != nil {
		t.Fatal(err)
	}
	status := "accepted"
	if p, _, err = s.Update(ctx, p.ID, reviewer("owner"), "", UpdateInput{Version: p.Version, Status: &status}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Terms, terms) {
		t.Fatalf("accepting dropped the terms: %q", p.Terms)
	}
	more := []string{"anything"}
	if _, _, err := s.Update(ctx, p.ID, agent("worker"), "", UpdateInput{Version: p.Version, Terms: &more}); err == nil {
		t.Fatal("an agent changed the terms of an accepted record")
	}
}

func TestSearchKeepsItsColumnsWhenTermsAreAdded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herma.sqlite3")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	bookmark := createRecord(t, s, CreateInput{Kind: "bookmark", Title: "Untitled", Body: "quagga", Fields: map[string]any{"url": "https://zebra.example/x"}})
	// Put the database back to the schema before search terms.
	if _, err := s.db.Exec(`CREATE VIRTUAL TABLE old_search USING fts5(title, body, fields);
INSERT INTO old_search(rowid, title, body, fields) SELECT rowid, title, body, fields FROM record_search;
DROP TABLE record_search;
ALTER TABLE old_search RENAME TO record_search;
PRAGMA user_version = 5;`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if userVersion(t, s) != schemaVersion {
		t.Fatalf("user_version = %d", userVersion(t, s))
	}
	if searchTotal(t, s, "untitled") != 1 || searchTotal(t, s, "quagga") != 1 || searchTotal(t, s, "zebra") != 1 {
		t.Fatal("the upgrade lost a title, body or field value from search")
	}
	terms := []string{"okapi"}
	if _, _, err := s.Update(context.Background(), bookmark.ID, reviewer("owner"), "", UpdateInput{Version: bookmark.Version, Terms: &terms}); err != nil {
		t.Fatal(err)
	}
	if searchTotal(t, s, "okapi") != 1 || searchTotal(t, s, "zebra") != 1 {
		t.Fatal("terms are not searchable after the upgrade")
	}
}
