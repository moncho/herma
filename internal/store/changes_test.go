package store

import (
	"context"
	"path/filepath"
	"testing"
)

func changesAfter(t *testing.T, s *Store, o ChangesOptions) ChangesResult {
	t.Helper()
	result, err := s.Changes(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func changed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func TestChangesNumberEveryWriteInCommitOrder(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if head := changesAfter(t, s, ChangesOptions{}); len(head.Items) != 0 || head.Next != 0 {
		t.Fatalf("empty store: %+v", head)
	}
	project := createRecord(t, s, CreateInput{Kind: "project", Title: "Mine"})
	task := createRecord(t, s, CreateInput{Kind: "task", Title: "First title", ProjectID: project.ID})
	status := "done"
	if _, _, err := s.Update(ctx, task.ID, agent("worker"), "", UpdateInput{Version: 1, Title: pointer("Second title"), Status: &status}); err != nil {
		t.Fatal(err)
	}
	all := changesAfter(t, s, ChangesOptions{})
	if len(all.Items) != 3 || all.Next != 3 {
		t.Fatalf("got %d changes with next %d, want 3 and 3: %+v", len(all.Items), all.Next, all)
	}
	for i, c := range all.Items {
		if c.Seq != int64(i+1) {
			t.Errorf("change %d has seq %d", i, c.Seq)
		}
	}
	created, updated := all.Items[1], all.Items[2]
	if created.Action != "created" || created.Actor != "agent-a" || created.Record.ID != task.ID || created.Record.Version != 1 ||
		created.Record.Title != "First title" || created.Record.Kind != "task" || created.Record.ProjectID != project.ID || !created.At.Equal(task.CreatedAt) {
		t.Errorf("created change: %+v", created)
	}
	// A change shows the record as that write left it, not as it is now.
	if updated.Action != "updated" || updated.Actor != "worker" || updated.Record.ID != task.ID || updated.Record.Version != 2 ||
		updated.Record.Title != "Second title" || updated.Record.Status != "done" {
		t.Errorf("updated change: %+v", updated)
	}
	if later := changesAfter(t, s, ChangesOptions{After: 2}); len(later.Items) != 1 || later.Items[0].Seq != 3 || later.Next != 3 {
		t.Errorf("after 2: %+v", later)
	}
	if none := changesAfter(t, s, ChangesOptions{After: 3}); len(none.Items) != 0 || none.Next != 3 {
		t.Errorf("after the newest change: %+v", none)
	}
}

// Next is where to continue: after the last change returned when the page is
// full, and after everything scanned otherwise, so a filtered reader does not
// rescan changes that can never match.
func TestChangesFiltersAndPagesWithACursor(t *testing.T) {
	s := testStore(t)
	mine := createRecord(t, s, CreateInput{Kind: "project", Title: "Mine"})                  // 1
	other := createRecord(t, s, CreateInput{Kind: "project", Title: "Other"})                // 2
	a := createRecord(t, s, CreateInput{Kind: "task", Title: "A", ProjectID: mine.ID})       // 3
	createRecord(t, s, CreateInput{Kind: "task", Title: "B", ProjectID: other.ID})           // 4
	createRecord(t, s, CreateInput{Kind: "note", Title: "N", Body: "b", ProjectID: mine.ID}) // 5
	c := createRecord(t, s, CreateInput{Kind: "task", Title: "C", ProjectID: mine.ID})       // 6
	createRecord(t, s, CreateInput{Kind: "task", Title: "Global"})                           // 7

	tasks := changesAfter(t, s, ChangesOptions{Kind: "task"})
	if len(tasks.Items) != 4 || tasks.Next != 7 {
		t.Errorf("kind filter: %d items, next %d, want 4 and 7", len(tasks.Items), tasks.Next)
	}
	// A project's feed has that project's records only, not global ones.
	scoped := changesAfter(t, s, ChangesOptions{Kind: "task", ProjectID: mine.ID})
	if len(scoped.Items) != 2 || scoped.Items[0].Record.ID != a.ID || scoped.Items[1].Record.ID != c.ID || scoped.Next != 7 {
		t.Errorf("kind and project filter: %+v", scoped)
	}
	page := changesAfter(t, s, ChangesOptions{Kind: "task", ProjectID: mine.ID, Limit: 1})
	if len(page.Items) != 1 || page.Items[0].Record.ID != a.ID || page.Next != 3 {
		t.Errorf("first page: %+v, want A and next 3", page)
	}
	page = changesAfter(t, s, ChangesOptions{Kind: "task", ProjectID: mine.ID, Limit: 1, After: page.Next})
	if len(page.Items) != 1 || page.Items[0].Record.ID != c.ID || page.Next != 6 {
		t.Errorf("second page: %+v, want C and next 6", page)
	}
	page = changesAfter(t, s, ChangesOptions{Kind: "task", ProjectID: mine.ID, Limit: 1, After: page.Next})
	if len(page.Items) != 0 || page.Next != 7 {
		t.Errorf("last page: %+v, want nothing and next 7", page)
	}
	// A cursor from a database that was since restored to an earlier state
	// is moved back to the newest change instead of waiting to be overtaken.
	if ahead := changesAfter(t, s, ChangesOptions{After: 50}); len(ahead.Items) != 0 || ahead.Next != 7 {
		t.Errorf("cursor past the newest change: %+v, want next 7", ahead)
	}
}

func TestChangesRejectsBadOptions(t *testing.T) {
	s := testStore(t)
	for _, o := range []ChangesOptions{{After: -1}, {Limit: -1}, {Limit: 201}, {Kind: string(make([]byte, 41))}, {ProjectID: string(make([]byte, 101))}} {
		if _, err := s.Changes(context.Background(), o); err == nil {
			t.Errorf("Changes(%+v) accepted", o)
		} else {
			assertValidation(t, err)
		}
	}
}

func TestChangedIsSignalledByCommittedWritesOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	before := s.Changed()
	if changed(before) {
		t.Fatal("signalled before any write")
	}
	if _, _, err := s.Create(ctx, agent("worker"), "", CreateInput{Kind: "task", Title: ""}); err == nil {
		t.Fatal("an invalid record was created")
	}
	if changed(before) {
		t.Fatal("a refused write signalled a change")
	}
	task := createRecord(t, s, CreateInput{Kind: "task", Title: "A"})
	if !changed(before) {
		t.Fatal("a create did not signal")
	}
	before = s.Changed()
	if err := s.CountFetched(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Update(ctx, task.ID, agent("worker"), "", UpdateInput{Version: 9, Title: pointer("B")}); err == nil {
		t.Fatal("a stale update was applied")
	}
	if changed(before) {
		t.Fatal("a usage count or a stale update signalled a change")
	}
	if _, _, err := s.Update(ctx, task.ID, agent("worker"), "", UpdateInput{Version: 1, Title: pointer("B")}); err != nil {
		t.Fatal(err)
	}
	if !changed(before) {
		t.Fatal("an update did not signal")
	}
}

// Upgrading numbers the history already there, oldest write first.
func TestUpgradeNumbersExistingHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "herma.sqlite3")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := createRecord(t, s, CreateInput{Kind: "task", Title: "First"})
	second := createRecord(t, s, CreateInput{Kind: "task", Title: "Second"})
	if _, _, err := s.Update(ctx, first.ID, agent("worker"), "", UpdateInput{Version: 1, Title: pointer("First again")}); err != nil {
		t.Fatal(err)
	}
	// Put the database back to the schema before changes were numbered.
	if _, err := s.db.Exec("DROP TABLE changes; PRAGMA user_version = 7;"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	history := changesAfter(t, s, ChangesOptions{})
	if len(history.Items) != 3 || history.Next != 3 {
		t.Fatalf("upgraded history: %+v", history)
	}
	for i, want := range []struct {
		id      string
		version int64
	}{{first.ID, 1}, {second.ID, 1}, {first.ID, 2}} {
		if got := history.Items[i]; got.Seq != int64(i+1) || got.Record.ID != want.id || got.Record.Version != want.version {
			t.Errorf("change %d: seq %d %s v%d, want %s v%d", i, got.Seq, got.Record.ID, got.Record.Version, want.id, want.version)
		}
	}
	third := createRecord(t, s, CreateInput{Kind: "task", Title: "Third"})
	if next := changesAfter(t, s, ChangesOptions{After: 3}); len(next.Items) != 1 || next.Items[0].Seq != 4 || next.Items[0].Record.ID != third.ID {
		t.Errorf("first write after the upgrade: %+v", next)
	}
}
