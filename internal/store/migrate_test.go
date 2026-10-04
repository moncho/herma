package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// legacySchema is the unversioned schema written before migrations existed.
// Its search table is keyed by an UNINDEXED id column.
const legacySchema = `
CREATE TABLE records (
 id TEXT PRIMARY KEY,
 kind TEXT NOT NULL,
 project_id TEXT REFERENCES records(id),
 status TEXT NOT NULL,
 priority INTEGER NOT NULL,
 owner TEXT NOT NULL,
 archived INTEGER NOT NULL,
 updated_ns INTEGER NOT NULL,
 data TEXT NOT NULL
);
CREATE INDEX records_project ON records(project_id);
CREATE INDEX records_order ON records(priority DESC, updated_ns DESC, id);
CREATE INDEX records_project_recent ON records(project_id, kind, archived, updated_ns DESC, id);
CREATE VIRTUAL TABLE record_search USING fts5(id UNINDEXED, title, body);
CREATE TABLE revisions (
 record_id TEXT NOT NULL REFERENCES records(id),
 version INTEGER NOT NULL,
 data TEXT NOT NULL,
 PRIMARY KEY(record_id, version)
);
CREATE TABLE idempotency (
 actor TEXT NOT NULL,
 key TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 data TEXT NOT NULL,
 PRIMARY KEY(actor, key)
);`

func legacyDatabase(t *testing.T, records ...Record) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		revision, err := json.Marshal(Revision{Version: r.Version, Actor: r.CreatedBy, Action: "created", At: r.UpdatedAt, Record: r})
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{"INSERT INTO records(id, kind, project_id, status, priority, owner, archived, updated_ns, data) VALUES (?, ?, NULL, ?, ?, ?, 0, ?, ?)",
				[]any{r.ID, r.Kind, r.Status, r.Priority, r.Owner, r.UpdatedAt.UnixNano(), string(data)}},
			{"INSERT INTO record_search(id, title, body) VALUES (?, ?, ?)", []any{r.ID, r.Title, r.Body}},
			{"INSERT INTO revisions(record_id, version, data) VALUES (?, ?, ?)", []any{r.ID, r.Version, string(revision)}},
		} {
			if _, err := db.Exec(statement.query, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	return path
}

func legacyRecord(id, title, body string) Record {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return Record{
		ID: id, Kind: "task", Title: title, Body: body, Status: "open",
		Tags: []string{}, Links: []string{}, Sources: []string{},
		CreatedBy: "agent-a", UpdatedBy: "agent-a", CreatedAt: at, UpdatedAt: at, Version: 1,
	}
}

func userVersion(t *testing.T, s *Store) int {
	t.Helper()
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func searchTotal(t *testing.T, s *Store, query string) int {
	t.Helper()
	result, err := s.List(context.Background(), ListOptions{Query: query})
	if err != nil {
		t.Fatal(err)
	}
	return result.Total
}

func TestOpenUpgradesLegacyDatabaseAndKeepsSearchInSync(t *testing.T) {
	ctx := context.Background()
	first := legacyRecord("rec_"+strings.Repeat("a", 32), "Legacy coordination", "migrated body text")
	second := legacyRecord("rec_"+strings.Repeat("b", 32), "Another legacy item", "second body")
	s, err := Open(legacyDatabase(t, first, second))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := userVersion(t, s); got != schemaVersion {
		t.Fatalf("user_version = %d, want %d", got, schemaVersion)
	}
	for query, want := range map[string]int{"legacy": 2, "migrated": 1, "second": 1} {
		if got := searchTotal(t, s, query); got != want {
			t.Fatalf("search %q after upgrade = %d, want %d", query, got, want)
		}
	}
	if _, _, err := s.Update(ctx, first.ID, reviewer("agent-b"), "", UpdateInput{Version: 1, Title: pointer("Renamed coordination")}); err != nil {
		t.Fatal(err)
	}
	created := createRecord(t, s, CreateInput{Kind: "task", Title: "Fresh coordination"})
	for query, want := range map[string]int{"legacy": 1, "renamed": 1, "coordination": 2, "fresh": 1} {
		if got := searchTotal(t, s, query); got != want {
			t.Fatalf("search %q after writes = %d, want %d", query, got, want)
		}
	}
	var rows int
	if err := s.db.QueryRow("SELECT count(*) FROM record_search").Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("search rows = %d (%v), want one per record", rows, err)
	}
	if got, err := s.Get(ctx, created.ID); err != nil || got.Title != "Fresh coordination" {
		t.Fatalf("created record: %+v %v", got, err)
	}
}

func TestOpenIsIdempotentForCurrentSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knowledge.sqlite3")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	createRecord(t, s, CreateInput{Kind: "task", Title: "Survives reopen"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := userVersion(t, s); got != schemaVersion {
		t.Fatalf("user_version = %d, want %d", got, schemaVersion)
	}
	if got := searchTotal(t, s, "survives"); got != 1 {
		t.Fatalf("search after reopen = %d, want 1", got)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knowledge.sqlite3")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("expected a newer schema to be refused")
	} else if !strings.Contains(err.Error(), "newer") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFailedMigrationLeavesDatabaseUnchanged(t *testing.T) {
	path := legacyDatabase(t, legacyRecord("rec_"+strings.Repeat("c", 32), "Kept legacy row", ""))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// An object with the migration's index name makes step 2 fail midway.
	if _, err := db.Exec("CREATE TABLE records_search_rowid (x)"); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("expected the conflicting migration to fail")
	}
	var version int
	var id string
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("user_version = %d (%v), want 1 after step 1 only", version, err)
	}
	if err := db.QueryRow("SELECT id FROM record_search WHERE record_search MATCH 'kept'").Scan(&id); err != nil {
		t.Fatalf("legacy search table was not restored: %v", err)
	}
	if _, err := db.Exec("DROP TABLE records_search_rowid"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("retry after fixing the conflict: %v", err)
	}
	defer s.Close()
	if got := searchTotal(t, s, "kept"); got != 1 {
		t.Fatalf("search after retried migration = %d, want 1", got)
	}
}

func TestReceiptDoesNotDuplicateRecordBody(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	body := strings.Repeat("large handoff body ", 3000)
	input := CreateInput{Kind: "note", Title: "Large handoff", Body: body}
	r, _, err := s.Create(ctx, reviewer("agent-a"), "large-request", input)
	if err != nil {
		t.Fatal(err)
	}
	// Measure every column so the check does not depend on the receipt layout.
	rows, err := s.db.Query("SELECT * FROM idempotency")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		for _, value := range values {
			size += len(fmt.Sprint(value))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if size > 1024 {
		t.Fatalf("receipt stores %d bytes for a %d-byte body; it should reference the revision", size, len(body))
	}
	got, replay, err := s.Create(ctx, reviewer("agent-a"), "large-request", input)
	if err != nil || !replay || !reflect.DeepEqual(got, r) {
		t.Fatalf("replay after compact receipt: %t %v", replay, err)
	}
}

// versionTwoDatabase holds one record created with a receipt in the format
// written by schema version 2, which stored a full record copy.
func versionTwoDatabase(t *testing.T, r Record, key string, input CreateInput) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v2.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, migration := range migrations[:2] {
		if _, err := db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := json.Marshal(Revision{Version: r.Version, Actor: r.CreatedBy, Action: "created", At: r.UpdatedAt, Record: r})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := requestFingerprint("create", "", input)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"PRAGMA user_version = 2", nil},
		{"INSERT INTO record_search(rowid, title, body) VALUES (1, ?, ?)", []any{r.Title, r.Body}},
		{"INSERT INTO records(id, kind, project_id, status, priority, owner, archived, updated_ns, data, search_rowid) VALUES (?, ?, NULL, ?, ?, ?, 0, ?, ?, 1)",
			[]any{r.ID, r.Kind, r.Status, r.Priority, r.Owner, r.UpdatedAt.UnixNano(), string(data)}},
		{"INSERT INTO revisions(record_id, version, data) VALUES (?, ?, ?)", []any{r.ID, r.Version, string(revision)}},
		{"INSERT INTO idempotency(actor, key, fingerprint, data) VALUES (?, ?, ?, ?)", []any{r.CreatedBy, key, fingerprint, string(data)}},
	} {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestOpenUpgradesVersionTwoReceipts(t *testing.T) {
	ctx := context.Background()
	input := CreateInput{Kind: "task", Title: "Receipt before upgrade", Body: "kept"}
	r := legacyRecord("rec_"+strings.Repeat("d", 32), input.Title, input.Body)
	s, err := Open(versionTwoDatabase(t, r, "old-request", input))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, replay, err := s.Create(ctx, reviewer("agent-a"), "old-request", input)
	want := r
	want.Fields = map[string]any{} // snapshots from before typed fields decode with none
	if err != nil || !replay || !reflect.DeepEqual(got, want) {
		t.Fatalf("replay of upgraded receipt: %+v %t %v", got, replay, err)
	}
	other := input
	other.Title = "Different input"
	if _, _, err := s.Create(ctx, reviewer("agent-a"), "old-request", other); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("misuse of upgraded receipt: %v", err)
	}
}

func TestSearchFindsFieldValuesAfterUpgrade(t *testing.T) {
	path := legacyDatabase(t, legacyRecord("rec_"+strings.Repeat("a", 32), "Old title", "old body"))
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if userVersion(t, s) != 4 {
		t.Fatalf("user_version = %d", userVersion(t, s))
	}
	if searchTotal(t, s, "old") != 1 {
		t.Fatal("legacy record lost from search")
	}
	acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	createRecord(t, s, CreateInput{Kind: "bookmark", Title: "Untitled", Fields: map[string]any{"url": "https://zebra.example/x", "authors": "Hipp"}})
	if searchTotal(t, s, "zebra") != 1 || searchTotal(t, s, "hipp") != 1 {
		t.Fatal("field values are not searchable")
	}
}
