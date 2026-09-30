package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func createRecord(t *testing.T, s *Store, input CreateInput) Record {
	t.Helper()
	r, replay, err := s.Create(context.Background(), reviewer("agent-a"), "", input)
	if err != nil {
		t.Fatal(err)
	}
	if replay {
		t.Fatal("unexpected replay")
	}
	return r
}

func pointer[T any](value T) *T { return &value }

func assertValidation(t *testing.T, err error) {
	t.Helper()
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("wanted ValidationError, got %v", err)
	}
}

func TestPersistenceHistoryAndAttribution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "knowledge.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Create(ctx, reviewer("agent-a"), "persist-create", CreateInput{Kind: "knowledge", Title: "  Search decision  ", Body: "Use SQLite", Tags: []string{" Storage ", "storage", "MVP"}, Sources: []string{"https://example.com/spec"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Title != "Search decision" || r.Status != "proposed" || r.Version != 1 || r.CreatedBy != "agent-a" || r.UpdatedBy != "agent-a" || r.CreatedAt.IsZero() || r.CreatedAt.Location().String() != "UTC" {
		t.Fatalf("incorrect created record: %+v", r)
	}
	if !reflect.DeepEqual(r.Tags, []string{"storage", "mvp"}) || r.Links == nil {
		t.Fatalf("normalization: %+v", r)
	}
	updated, _, err := s.Update(ctx, r.ID, reviewer("agent-b"), "persist-update", UpdateInput{Version: 1, Body: pointer("Use SQLite with FTS5"), Status: pointer("accepted")})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || updated.CreatedBy != "agent-a" || updated.UpdatedBy != "agent-b" || !updated.CreatedAt.Equal(r.CreatedAt) {
		t.Fatalf("incorrect update: %+v", updated)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Get(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(got, updated) {
		t.Fatalf("reopened record: %+v, %v", got, err)
	}
	history, err := s.History(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Version != 1 || history[1].Version != 2 || history[0].Actor != "agent-a" || history[1].Actor != "agent-b" || history[0].Action != "created" || history[1].Action != "updated" || !reflect.DeepEqual(history[0].Record, r) || !reflect.DeepEqual(history[1].Record, updated) {
		t.Fatalf("incorrect history: %+v", history)
	}
	replayed, replay, err := s.Update(ctx, r.ID, reviewer("agent-b"), "persist-update", UpdateInput{Version: 1, Body: pointer("Use SQLite with FTS5"), Status: pointer("accepted")})
	if err != nil || !replay || !reflect.DeepEqual(replayed, updated) {
		t.Fatalf("persisted replay: %+v, %t, %v", replayed, replay, err)
	}
	results, err := s.List(ctx, ListOptions{Query: "FTS5"})
	if err != nil || results.Total != 1 || results.Items[0].ID != r.ID {
		t.Fatalf("persisted FTS: %+v, %v", results, err)
	}
}

func TestConcurrentUpdatesHaveOneWinner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := createRecord(t, s, CreateInput{Kind: "task", Title: "Ship the first milestone"})
	const count = 12
	start := make(chan struct{})
	errorsReceived := make(chan error, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, _, err := s.Update(ctx, r.ID, reviewer("agent-b"), "", UpdateInput{Version: 1, Status: pointer("in_progress")})
			errorsReceived <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsReceived)
	winners, conflicts := 0, 0
	for err := range errorsReceived {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Errorf("unexpected update error: %v", err)
		}
	}
	if winners != 1 || conflicts != count-1 {
		t.Fatalf("winners=%d conflicts=%d", winners, conflicts)
	}
	history, err := s.History(ctx, r.ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("history=%+v error=%v", history, err)
	}
}

func TestIdempotencyReplaySnapshotAndMisuse(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	input := CreateInput{Kind: "task", Title: "Original task"}
	r, replay, err := s.Create(ctx, reviewer("agent-a"), "request-1", input)
	if err != nil || replay {
		t.Fatalf("create: %t %v", replay, err)
	}
	updated, _, err := s.Update(ctx, r.ID, reviewer("agent-b"), "request-2", UpdateInput{Version: 1, Title: pointer("Changed task")})
	if err != nil {
		t.Fatal(err)
	}
	got, replay, err := s.Create(ctx, reviewer("agent-a"), "request-1", input)
	if err != nil || !replay || !reflect.DeepEqual(got, r) {
		t.Fatalf("original snapshot replay: %+v %t %v", got, replay, err)
	}
	current, err := s.Get(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(current, updated) {
		t.Fatal("replay changed the current record")
	}
	other := input
	other.Title = "Different input"
	_, _, err = s.Create(ctx, reviewer("agent-a"), "request-1", other)
	if !errors.Is(err, ErrIdempotency) {
		t.Fatalf("misuse returned %v", err)
	}
	_, _, err = s.Update(ctx, r.ID, reviewer("agent-a"), "request-1", UpdateInput{Version: 2, Title: pointer("No")})
	if !errors.Is(err, ErrIdempotency) {
		t.Fatalf("cross-operation reuse: %v", err)
	}
	otherActor, replay, err := s.Create(ctx, reviewer("agent-b"), "request-1", input)
	if err != nil || replay || otherActor.ID == r.ID {
		t.Fatalf("actor scope: %+v %t %v", otherActor, replay, err)
	}
	_, _, err = s.Update(ctx, otherActor.ID, reviewer("agent-b"), "request-2", UpdateInput{Version: 1, Title: pointer("Changed task")})
	if !errors.Is(err, ErrIdempotency) {
		t.Fatalf("cross-record reuse: %v", err)
	}
	history, err := s.History(ctx, r.ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("replays/misuse created revisions: %+v %v", history, err)
	}
}

func TestConcurrentIdempotentCreate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const count = 8
	results := make(chan Record, count)
	errCh := make(chan error, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			r, _, err := s.Create(ctx, reviewer("agent-a"), "shared-request", CreateInput{Kind: "knowledge", Title: "Same request"})
			results <- r
			errCh <- err
		}()
	}
	group.Wait()
	close(results)
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for r := range results {
		if id != "" && id != r.ID {
			t.Fatalf("created two IDs: %s %s", id, r.ID)
		}
		id = r.ID
	}
	listed, err := s.List(ctx, ListOptions{})
	if err != nil || listed.Total != 1 {
		t.Fatalf("duplicate records: %+v %v", listed, err)
	}
}

func TestFiltersSearchAndPagination(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	project := createRecord(t, s, CreateInput{Kind: "project", Title: "Project Mercury"})
	otherProject := createRecord(t, s, CreateInput{Kind: "project", Title: "Project Venus"})
	wanted := createRecord(t, s, CreateInput{Kind: "task", Title: "SQLite Search", Body: "Design durable persistence", ProjectID: project.ID, Status: "in_progress", Priority: 5, Owner: "agent-a", Tags: []string{" Storage "}})
	createRecord(t, s, CreateInput{Kind: "task", Title: "SQLite Search", Body: "Design durable persistence", ProjectID: otherProject.ID, Status: "in_progress", Priority: 4, Owner: "agent-a", Tags: []string{"storage"}})
	createRecord(t, s, CreateInput{Kind: "task", Title: "SQLite Search", Body: "Design durable persistence", ProjectID: project.ID, Status: "open", Priority: 3, Owner: "agent-a", Tags: []string{"storage"}})
	createRecord(t, s, CreateInput{Kind: "knowledge", Title: "SQLite Search", Body: "Design durable persistence", ProjectID: project.ID, Priority: 2, Owner: "agent-b", Tags: []string{"storage"}})
	global := createRecord(t, s, CreateInput{Kind: "principle", Title: "Prefer durable SQLite storage", Priority: 1, Tags: []string{"storage"}})
	archived := createRecord(t, s, CreateInput{Kind: "task", Title: "Archived SQLite work", Priority: 5})
	if _, _, err := s.Update(ctx, archived.ID, reviewer("agent-a"), "", UpdateInput{Version: 1, Archived: pointer(true)}); err != nil {
		t.Fatal(err)
	}
	options := ListOptions{Kind: "task", ProjectID: project.ID, Status: "in_progress", Owner: "agent-a", Tag: "STORAGE", Query: "sqlite durable"}
	got, err := s.List(ctx, options)
	if err != nil || got.Total != 1 || len(got.Items) != 1 || got.Items[0].ID != wanted.ID {
		t.Fatalf("conjunctive filters: %+v %v", got, err)
	}
	got, err = s.List(ctx, ListOptions{Global: true, Tag: "storage"})
	if err != nil || got.Total != 1 || got.Items[0].ID != global.ID {
		t.Fatalf("global filter: %+v %v", got, err)
	}
	got, err = s.List(ctx, ListOptions{Query: "sqlite", Limit: 2, Offset: 1})
	if err != nil || got.Total != 5 || len(got.Items) != 2 || got.Items[0].Priority != 4 || got.Items[1].Priority != 3 {
		t.Fatalf("pagination: %+v %v", got, err)
	}
	got, err = s.List(ctx, ListOptions{Query: "sqlite", Archived: true})
	if err != nil || got.Total != 6 {
		t.Fatalf("include archived: %+v %v", got, err)
	}
	for _, query := range []string{`sqlite OR missing`, `" OR *`, `title:sqlite`, `()`, `NEAR(sqlite, 2)`} {
		got, err := s.List(ctx, ListOptions{Query: query})
		if err != nil || got.Total != 0 {
			t.Errorf("unsafe/literal query %q produced %+v %v", query, got, err)
		}
	}
	got, err = s.List(ctx, ListOptions{Query: `"sqlite"`})
	if err != nil || got.Total != 5 {
		t.Fatalf("quoted plaintext: %+v %v", got, err)
	}
	if _, _, err := s.Update(ctx, wanted.ID, reviewer("agent-b"), "", UpdateInput{Version: 1, Title: pointer("Changed title"), Body: pointer("Removed the old search terms")}); err != nil {
		t.Fatal(err)
	}
	got, err = s.List(ctx, ListOptions{Query: "sqlite durable", Kind: "task", Status: "in_progress", ProjectID: project.ID})
	if err != nil || got.Total != 0 {
		t.Fatalf("stale search index: %+v %v", got, err)
	}
}

func TestReferenceValidationAndArchivedProjects(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	project := createRecord(t, s, CreateInput{Kind: "project", Title: "First project"})
	task := createRecord(t, s, CreateInput{Kind: "task", Title: "First task", ProjectID: project.ID, Links: []string{project.ID}})
	if _, _, err := s.Update(ctx, project.ID, reviewer("agent-a"), "", UpdateInput{Version: 1, Archived: pointer(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Update(ctx, task.ID, reviewer("agent-a"), "", UpdateInput{Version: 1, Title: pointer("Still editable"), ProjectID: &project.ID}); err != nil {
		t.Fatalf("cannot retain old archived project: %v", err)
	}
	for _, input := range []CreateInput{
		{Kind: "task", Title: "Archived project", ProjectID: project.ID},
		{Kind: "task", Title: "Missing project", ProjectID: "missing"},
		{Kind: "task", Title: "Non-project reference", ProjectID: task.ID},
		{Kind: "project", Title: "Nested project", ProjectID: project.ID},
		{Kind: "knowledge", Title: "Missing link", Links: []string{"missing"}},
	} {
		_, _, err := s.Create(ctx, reviewer("agent-a"), "", input)
		assertValidation(t, err)
	}
	linked := createRecord(t, s, CreateInput{Kind: "knowledge", Title: "An archived record may still be linked", Links: []string{project.ID}})
	if len(linked.Links) != 1 {
		t.Fatal("missing link")
	}
	_, _, err := s.Update(ctx, task.ID, reviewer("agent-a"), "", UpdateInput{Version: 2, Links: pointer([]string{"missing"})})
	assertValidation(t, err)
	history, err := s.History(ctx, task.ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("invalid reference created revision: %+v %v", history, err)
	}
}

func TestNotesAreAppendOnlyButArchivable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	note := createRecord(t, s, CreateInput{Kind: "note", Title: "Handover", Body: "Next agent should review the decision."})
	_, _, err := s.Update(ctx, note.ID, reviewer("agent-b"), "", UpdateInput{Version: 1, Body: pointer("Changed")})
	assertValidation(t, err)
	_, _, err = s.Update(ctx, note.ID, reviewer("agent-b"), "", UpdateInput{Version: 1, Tags: pointer([]string{"changed"})})
	assertValidation(t, err)
	archived, _, err := s.Update(ctx, note.ID, reviewer("agent-b"), "", UpdateInput{Version: 1, Archived: pointer(true)})
	if err != nil || !archived.Archived || archived.Version != 2 || archived.Body != note.Body {
		t.Fatalf("archive: %+v %v", archived, err)
	}
	restored, _, err := s.Update(ctx, note.ID, reviewer("agent-b"), "", UpdateInput{Version: 2, Archived: pointer(false)})
	if err != nil || restored.Archived || restored.Version != 3 {
		t.Fatalf("restore: %+v %v", restored, err)
	}
}

func TestValidationAndDefaults(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for kind, status := range defaultStatus {
		r := createRecord(t, s, CreateInput{Kind: kind, Title: "Default status"})
		if r.Status != status {
			t.Errorf("%s default=%q, want %q", kind, r.Status, status)
		}
	}
	for name, input := range map[string]CreateInput{
		"kind":          {Kind: "unsupported", Title: "Title"},
		"blank title":   {Kind: "task", Title: " \n "},
		"long title":    {Kind: "task", Title: strings.Repeat("a", 301)},
		"body":          {Kind: "task", Title: "Title", Body: strings.Repeat("a", 64*1024+1)},
		"status":        {Kind: "task", Title: "Title", Status: "accepted"},
		"priority high": {Kind: "task", Title: "Title", Priority: 6},
		"priority low":  {Kind: "task", Title: "Title", Priority: -1},
		"tags":          {Kind: "task", Title: "Title", Tags: []string{""}},
		"tag count":     {Kind: "task", Title: "Title", Tags: make([]string, 33)},
		"source":        {Kind: "task", Title: "Title", Sources: []string{strings.Repeat("a", 2049)}},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := s.Create(ctx, reviewer("agent-a"), "", input)
			assertValidation(t, err)
		})
	}
	for _, actor := range []string{"", "  ", strings.Repeat("a", 201)} {
		_, _, err := s.Create(ctx, reviewer(actor), "", CreateInput{Kind: "task", Title: "Test"})
		assertValidation(t, err)
	}
	r := createRecord(t, s, CreateInput{Kind: "task", Title: "Versions"})
	for _, input := range []UpdateInput{{Version: 0, Title: pointer("New")}, {Version: -1, Title: pointer("New")}, {Version: 1}} {
		_, _, err := s.Update(ctx, r.ID, reviewer("agent-a"), "", input)
		assertValidation(t, err)
	}
	_, _, err := s.Update(ctx, r.ID, reviewer("agent-a"), "", UpdateInput{Version: 99, Title: pointer("Future")})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("future version: %v", err)
	}
	for _, options := range []ListOptions{
		{Kind: "unknown"}, {Status: "unknown"}, {Kind: "task", Status: "accepted"},
		{ProjectID: "some-project", Global: true}, {Limit: -1}, {Limit: 201}, {Offset: -1},
		{Query: strings.Repeat("a", 501)},
	} {
		_, err := s.List(ctx, options)
		assertValidation(t, err)
	}
	if _, err := s.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	if _, err := s.History(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("history missing: %v", err)
	}
	if _, _, err := s.Update(ctx, "missing", reviewer("agent-a"), "", UpdateInput{Version: 1, Title: pointer("Missing")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

func TestRejectedMutationDoesNotReserveIdempotencyKey(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, _, err := s.Create(ctx, reviewer("agent-a"), "retry-after-fix", CreateInput{Kind: "task", Title: ""})
	assertValidation(t, err)
	r, replay, err := s.Create(ctx, reviewer("agent-a"), "retry-after-fix", CreateInput{Kind: "task", Title: "Now valid"})
	if err != nil || replay || r.Version != 1 {
		t.Fatalf("valid retry: %+v %t %v", r, replay, err)
	}
}

func TestWriteFailureRollsBackRecordSearchHistoryAndReceipt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := createRecord(t, s, CreateInput{Kind: "knowledge", Title: "Original searchable text"})
	// Simulate a storage failure after the record and its search index have been
	// written, so this checks rollback across every part of the mutation.
	_, err := s.db.Exec(`CREATE TRIGGER fail_revision BEFORE INSERT ON revisions
WHEN NEW.version = 2 BEGIN SELECT RAISE(ABORT, 'simulated revision failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	input := UpdateInput{Version: 1, Title: pointer("Replacement searchable text")}
	if _, _, err := s.Update(ctx, r.ID, reviewer("agent-b"), "retry-write", input); err == nil {
		t.Fatal("expected simulated storage failure")
	}
	got, err := s.Get(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("record partially committed: %+v %v", got, err)
	}
	for query, count := range map[string]int{"original": 1, "replacement": 0} {
		result, err := s.List(ctx, ListOptions{Query: query})
		if err != nil || result.Total != count {
			t.Fatalf("search index partially committed: %+v %v", result, err)
		}
	}
	history, err := s.History(ctx, r.ID)
	if err != nil || len(history) != 1 {
		t.Fatalf("history partially committed: %+v %v", history, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_revision"); err != nil {
		t.Fatal(err)
	}
	updated, replay, err := s.Update(ctx, r.ID, reviewer("agent-b"), "retry-write", input)
	if err != nil || replay || updated.Version != 2 {
		t.Fatalf("receipt partially committed: %+v %t %v", updated, replay, err)
	}
}

func TestSearchTreatsPathologicalStringsAsPlainText(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := createRecord(t, s, CreateInput{
		Kind: "knowledge", Title: "SQLite café 日本語", Body: "AND OR NOT NEAR prefix\x00suffix",
	})
	for _, query := range []string{`[SQLite]`, `"sqlite"`, "cafe", "日本語", "AND OR NOT", "NEAR", "prefix", "suffix"} {
		result, err := s.List(ctx, ListOptions{Query: query})
		if err != nil || result.Total != 1 || result.Items[0].ID != r.ID {
			t.Errorf("plain query %q: %+v, %v", query, result, err)
		}
	}
	for _, query := range []string{"\x00", "\u0301", "😀", "*:^(){}[]", `"; DROP TABLE records; --`} {
		result, err := s.List(ctx, ListOptions{Query: query})
		if err != nil || result.Total != 0 {
			t.Errorf("pathological query %q: %+v, %v", query, result, err)
		}
	}
	if _, err := s.Get(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Create(ctx, reviewer("agent-a"), "", CreateInput{Kind: "knowledge", Title: "Invalid UTF-8", Body: string([]byte{0xff})})
	assertValidation(t, err)
	_, err = s.List(ctx, ListOptions{Query: string([]byte{0xff})})
	assertValidation(t, err)
}

func TestOrderingUsesPriorityThenUpdateTimeThenID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first := createRecord(t, s, CreateInput{Kind: "task", Title: "First", Priority: 5})
	second := createRecord(t, s, CreateInput{Kind: "task", Title: "Second", Priority: 5})
	createRecord(t, s, CreateInput{Kind: "task", Title: "Newest but lower priority", Priority: 4})
	result, err := s.List(ctx, ListOptions{})
	if err != nil || len(result.Items) != 3 || result.Items[0].ID != second.ID || result.Items[1].ID != first.ID {
		t.Fatalf("creation order: %+v %v", result, err)
	}
	_, _, err = s.Update(ctx, first.ID, reviewer("agent-b"), "", UpdateInput{Version: 1, Body: pointer("Updated most recently")})
	if err != nil {
		t.Fatal(err)
	}
	result, err = s.List(ctx, ListOptions{})
	if err != nil || result.Items[0].ID != first.ID {
		t.Fatalf("update order: %+v %v", result, err)
	}
	// Equal timestamps are possible, so the last sort key must remain stable.
	if _, err := s.db.Exec("UPDATE records SET updated_ns = 100 WHERE priority = 5"); err != nil {
		t.Fatal(err)
	}
	result, err = s.List(ctx, ListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.List(ctx, ListOptions{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 3 || next.Total != 3 || len(result.Items) != 1 || len(next.Items) != 1 || result.Items[0].ID >= next.Items[0].ID {
		t.Fatalf("unstable pagination order: %+v %+v", result, next)
	}
}
