package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func bookmarkDefinition() map[string]any {
	return map[string]any{
		"schema": map[string]any{
			"url":      map[string]any{"type": "url", "required": true, "unique": true},
			"platform": map[string]any{"type": "enum", "values": []any{"web", "x"}},
			"rating":   map[string]any{"type": "integer", "min": 1, "max": 5},
			"authors":  map[string]any{"type": "string-list"},
		},
		"statuses": []any{"unread", "reading", "done"},
		"policy":   map[string]any{"recall": true},
	}
}

func proposeKind(t *testing.T, s *Store, name string, def map[string]any, project string) Record {
	t.Helper()
	r, _, err := s.Create(context.Background(), agent("agent-a"), "", CreateInput{Kind: "kind", Title: name, ProjectID: project, Fields: map[string]any{"definition": def}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func acceptKind(t *testing.T, s *Store, name string, def map[string]any, project string) Record {
	t.Helper()
	proposed := proposeKind(t, s, name, def, project)
	accepted, _, err := s.Update(context.Background(), proposed.ID, reviewer("owner"), "", UpdateInput{Version: proposed.Version, Status: pointer("accepted")})
	if err != nil {
		t.Fatal(err)
	}
	return accepted
}

func TestAgentProposesKindAndReviewerAccepts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	proposed := proposeKind(t, s, "bookmark", bookmarkDefinition(), "")
	if proposed.Status != "proposed" || proposed.Fields["definition"] == nil {
		t.Fatalf("proposed: %+v", proposed)
	}
	if _, _, err := s.Create(ctx, agent("agent-a"), "", CreateInput{Kind: "bookmark", Title: "Early", Fields: map[string]any{"url": "https://example.com"}}); err == nil || err.Error() != `kind "bookmark" is proposed, not yet accepted` {
		t.Fatalf("create before acceptance: %v", err)
	}
	if _, _, err := s.Update(ctx, proposed.ID, agent("agent-a"), "", UpdateInput{Version: 1, Status: pointer("accepted")}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent accepting: %v", err)
	}
	accepted, _, err := s.Update(ctx, proposed.ID, reviewer("owner"), "", UpdateInput{Version: 1, Status: pointer("accepted")})
	if err != nil || accepted.ReviewedBy != "owner" {
		t.Fatalf("accept: %+v %v", accepted, err)
	}
	b, _, err := s.Create(ctx, agent("agent-a"), "", CreateInput{Kind: "bookmark", Title: "WAL", Fields: map[string]any{"url": "https://sqlite.org/wal.html", "rating": 4, "authors": "Hipp"}})
	if err != nil || b.Status != "unread" || b.Fields["rating"] != float64(4) {
		t.Fatalf("bookmark: %+v %v", b, err)
	}
	updated, _, err := s.Update(ctx, b.ID, agent("agent-a"), "", UpdateInput{Version: 1, Status: pointer("done"), Fields: map[string]any{"rating": nil, "platform": "web"}})
	if err != nil || updated.Fields["rating"] != nil || updated.Fields["platform"] != "web" || updated.Fields["url"] != "https://sqlite.org/wal.html" {
		t.Fatalf("patch: %+v %v", updated, err)
	}
	if _, _, err := s.Update(ctx, b.ID, agent("agent-a"), "", UpdateInput{Version: 2, Fields: map[string]any{"url": nil}}); err == nil || err.Error() != "fields.url: required" {
		t.Fatalf("removing a required field: %v", err)
	}
	if _, _, err := s.Create(ctx, agent("agent-a"), "", CreateInput{Kind: "bookmark", Title: "Bad", Status: "accepted", Fields: map[string]any{"url": "https://a.example"}}); err == nil {
		t.Fatal("status outside the kind's statuses was accepted")
	}
}

func TestUnknownRetiredAndBuiltinKindRules(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Create(ctx, agent("a"), "", CreateInput{Kind: "nope", Title: "x"}); err == nil || err.Error() != `unknown kind "nope"` {
		t.Fatalf("unknown: %v", err)
	}
	if _, _, err := s.Create(ctx, agent("a"), "", CreateInput{Kind: "task", Title: "x", Fields: map[string]any{"a": "b"}}); err == nil || err.Error() != "fields.a: unknown field" {
		t.Fatalf("built-in with fields: %v", err)
	}
	k := acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	b := createRecord(t, s, CreateInput{Kind: "bookmark", Title: "Kept", Fields: map[string]any{"url": "https://kept.example"}})
	retired, _, err := s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: k.Version, Status: pointer("retired")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, agent("a"), "", CreateInput{Kind: "bookmark", Title: "New", Fields: map[string]any{"url": "https://new.example"}}); err == nil || err.Error() != `kind "bookmark" is retired` {
		t.Fatalf("create under retired kind: %v", err)
	}
	if _, _, err := s.Update(ctx, b.ID, agent("a"), "", UpdateInput{Version: 1, Status: pointer("done")}); err != nil {
		t.Fatalf("update under retired kind: %v", err)
	}
	if _, _, err := s.Update(ctx, retired.ID, reviewer("owner"), "", UpdateInput{Version: retired.Version, Status: pointer("accepted")}); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
}

func TestKindNamesAreUniqueAndArchivingFreesThem(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, name := range []string{"task", "kind", "Bad", "has-dash", ""} {
		if _, _, err := s.Create(ctx, agent("a"), "", CreateInput{Kind: "kind", Title: name, Fields: map[string]any{"definition": bookmarkDefinition()}}); err == nil {
			t.Errorf("kind name %q accepted", name)
		}
	}
	first := proposeKind(t, s, "bookmark", bookmarkDefinition(), "")
	if _, _, err := s.Create(ctx, agent("a"), "", CreateInput{Kind: "kind", Title: "bookmark", Fields: map[string]any{"definition": bookmarkDefinition()}}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate proposal: %v", err)
	}
	if _, _, err := s.Update(ctx, first.ID, agent("a"), "", UpdateInput{Version: 1, Archived: pointer(true)}); err != nil {
		t.Fatal(err)
	}
	second := proposeKind(t, s, "bookmark", bookmarkDefinition(), "")
	if _, _, err := s.Update(ctx, first.ID, agent("a"), "", UpdateInput{Version: 2, Archived: pointer(false)}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("restoring a shadowed proposal: %v", err)
	}
	accepted, _, err := s.Update(ctx, second.ID, reviewer("owner"), "", UpdateInput{Version: 1, Status: pointer("accepted")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Update(ctx, accepted.ID, reviewer("owner"), "", UpdateInput{Version: accepted.Version, Archived: pointer(true)}); err == nil {
		t.Fatal("an accepted kind was archived")
	}
	if _, _, err := s.Update(ctx, accepted.ID, reviewer("owner"), "", UpdateInput{Version: accepted.Version, Title: pointer("renamed")}); err == nil {
		t.Fatal("a kind was renamed")
	}
}

func TestAcceptedKindAllowsOnlyPendingFromAgents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	k := acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	if _, _, err := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: k.Version, Body: pointer("new words")}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent body edit on accepted kind: %v", err)
	}
	if _, _, err := s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"definition": bookmarkDefinition()}}); err == nil {
		t.Fatal("definition edited directly on an accepted kind")
	}
	next := bookmarkDefinition()
	next["schema"].(map[string]any)["notes"] = map[string]any{"type": "text"}
	pending, _, err := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"pending": next}})
	if err != nil || pending.Fields["pending"] == nil {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	proposal := proposeKind(t, s, "paper", bookmarkDefinition(), "")
	if _, _, err := s.Update(ctx, proposal.ID, agent("a"), "", UpdateInput{Version: 1, Fields: map[string]any{"pending": next}}); err == nil {
		t.Fatal("pending set on a proposed kind")
	}
}

func TestProjectScopedKind(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	here := createRecord(t, s, CreateInput{Kind: "project", Title: "Here"})
	there := createRecord(t, s, CreateInput{Kind: "project", Title: "There"})
	def := map[string]any{"schema": map[string]any{"model": map[string]any{"type": "string", "required": true}}, "statuses": []any{"running", "finished"}, "policy": map[string]any{}}
	acceptKind(t, s, "experiment_run", def, here.ID)
	if _, _, err := s.Create(ctx, agent("a"), "", CreateInput{Kind: "experiment_run", Title: "r", ProjectID: there.ID, Fields: map[string]any{"model": "small"}}); err == nil {
		t.Fatal("record created in another project")
	}
	if _, _, err := s.Create(ctx, agent("a"), "", CreateInput{Kind: "experiment_run", Title: "r", Fields: map[string]any{"model": "small"}}); err == nil {
		t.Fatal("record created without the kind's project")
	}
	createRecord(t, s, CreateInput{Kind: "experiment_run", Title: "r", ProjectID: here.ID, Fields: map[string]any{"model": "small"}})
}

func TestReviewerOnlyWriters(t *testing.T) {
	s := testStore(t)
	def := bookmarkDefinition()
	def["policy"] = map[string]any{"writers": "reviewer"}
	acceptKind(t, s, "decree", def, "")
	if _, _, err := s.Create(context.Background(), agent("a"), "", CreateInput{Kind: "decree", Title: "x", Fields: map[string]any{"url": "https://a.example"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent write to reviewer-only kind: %v", err)
	}
	createRecord(t, s, CreateInput{Kind: "decree", Title: "x", Fields: map[string]any{"url": "https://a.example"}})
}

func TestReviewedCustomKindUsesReviewLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	def := map[string]any{"schema": map[string]any{"url": map[string]any{"type": "url"}}, "policy": map[string]any{"review": true}}
	acceptKind(t, s, "citation", def, "")
	c, _, err := s.Create(ctx, agent("a"), "", CreateInput{Kind: "citation", Title: "x"})
	if err != nil || c.Status != "proposed" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, _, err := s.Update(ctx, c.ID, reviewer("owner"), "", UpdateInput{Version: 1, Status: pointer("accepted")}); err == nil {
		t.Fatal("accepted without a source")
	}
	accepted, _, err := s.Update(ctx, c.ID, reviewer("owner"), "", UpdateInput{Version: 1, Status: pointer("accepted"), Sources: pointer([]string{"https://src.example"})})
	if err != nil || accepted.ReviewedBy != "owner" {
		t.Fatalf("%+v %v", accepted, err)
	}
}

func TestRecordsWithoutFieldsDecodeAsEmpty(t *testing.T) {
	r, err := decodeRecord(`{"id":"rec_x","kind":"task","title":"Old","status":"open","version":1}`)
	if err != nil || r.Fields == nil {
		t.Fatalf("%+v %v", r, err)
	}
	s := testStore(t)
	task := createRecord(t, s, CreateInput{Kind: "task", Title: "New"})
	history, err := s.History(context.Background(), task.ID)
	if err != nil || task.Fields == nil || history[0].Record.Fields == nil {
		t.Fatalf("fields: %+v %+v %v", task.Fields, history, err)
	}
}

func TestListFiltersByCustomKind(t *testing.T) {
	s := testStore(t)
	acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	createRecord(t, s, CreateInput{Kind: "bookmark", Title: "One", Fields: map[string]any{"url": "https://one.example"}})
	result, err := s.List(context.Background(), ListOptions{Kind: "bookmark", Status: "unread"})
	if err != nil || result.Total != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	for _, o := range []ListOptions{{Kind: "bookmark", Status: "open"}, {Kind: "missing"}, {Status: "Bad Status"}} {
		if _, err := s.List(context.Background(), o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
	if unscoped, err := s.List(context.Background(), ListOptions{Status: "unread"}); err != nil || unscoped.Total != 1 {
		t.Fatalf("unscoped custom status: %+v %v", unscoped, err)
	}
	if _, err := s.List(context.Background(), ListOptions{Status: "unknown"}); err == nil {
		t.Error("unscoped unknown status accepted")
	}
	kinds, err := s.Kinds(context.Background())
	if err != nil || len(kinds) != len(builtinOrder)+1 || kinds[len(kinds)-1].Name != "bookmark" || kinds[0].Name != "project" {
		t.Fatalf("kinds: %+v %v", kinds, err)
	}
}

func TestAcceptPendingAddsOptionalField(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	k := acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	next := bookmarkDefinition()
	next["schema"].(map[string]any)["notes"] = map[string]any{"type": "text"}
	pending, _, err := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"pending": next}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: pending.Version, AcceptPending: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent accepting pending: %v", err)
	}
	if _, _, err := s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: pending.Version, AcceptPending: true, Body: pointer("x")}); err == nil {
		t.Fatal("accept_pending combined with another change")
	}
	accepted, _, err := s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: pending.Version, AcceptPending: true})
	if err != nil || accepted.Fields["pending"] != nil || accepted.ReviewedBy != "owner" {
		t.Fatalf("accept: %+v %v", accepted, err)
	}
	createRecord(t, s, CreateInput{Kind: "bookmark", Title: "With notes", Fields: map[string]any{"url": "https://n.example", "notes": "long text"}})
	if _, _, err := s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: accepted.Version, AcceptPending: true}); err == nil || !strings.Contains(err.Error(), "no pending change") {
		t.Fatalf("nothing pending: %v", err)
	}
}

func TestAcceptPendingRefusesChangesThatBreakRecords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	k := acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	for i := range 7 {
		createRecord(t, s, CreateInput{Kind: "bookmark", Title: "b", Fields: map[string]any{"url": fmt.Sprintf("https://%d.example", i)}})
	}
	next := bookmarkDefinition()
	next["schema"].(map[string]any)["rating"] = map[string]any{"type": "integer", "required": true}
	pending, _, err := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"pending": next}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: pending.Version, AcceptPending: true})
	if err == nil || !strings.HasPrefix(err.Error(), "change breaks 7 records (showing 5): ") || strings.Count(err.Error(), "fields.rating: required") != 5 {
		t.Fatalf("breaking change: %v", err)
	}
	current, _ := s.Get(ctx, k.ID)
	if current.Version != pending.Version || current.Fields["pending"] == nil {
		t.Fatalf("a refused change modified the kind: %+v", current)
	}
}

func TestAcceptPendingAllowsBreakingChangeWhenNoRecordIsAffected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	k := acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	createRecord(t, s, CreateInput{Kind: "bookmark", Title: "b", Fields: map[string]any{"url": "https://a.example"}})
	next := bookmarkDefinition()
	delete(next["schema"].(map[string]any), "rating")
	pending, _, _ := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"pending": next}})
	if _, _, err := s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: pending.Version, AcceptPending: true}); err != nil {
		t.Fatalf("removing an unused field: %v", err)
	}
}

func TestAcceptPendingRefusesReviewChangeWhileRecordsExist(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	k := acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	createRecord(t, s, CreateInput{Kind: "bookmark", Title: "b", Fields: map[string]any{"url": "https://a.example"}})
	next := bookmarkDefinition()
	delete(next, "statuses")
	next["policy"] = map[string]any{"review": true}
	pending, _, _ := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"pending": next}})
	if _, _, err := s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: pending.Version, AcceptPending: true}); err == nil || !strings.Contains(err.Error(), "review cannot change") {
		t.Fatalf("review change: %v", err)
	}
}

func TestAcceptPendingOnNonKindRecordWritesNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	task := createRecord(t, s, CreateInput{Kind: "task", Title: "t"})
	_, _, err := s.Update(ctx, task.ID, reviewer("owner"), "", UpdateInput{Version: task.Version, AcceptPending: true})
	var verr *ValidationError
	if !errors.As(err, &verr) || !strings.Contains(err.Error(), "only to kind records") {
		t.Fatalf("accept_pending on a task: %v", err)
	}
	current, _ := s.Get(ctx, task.ID)
	if current.Version != task.Version {
		t.Fatalf("a refused accept_pending saved a revision: %+v", current)
	}
}

func TestAcceptPendingRefusesReviewChangeForArchivedRecordsOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	k := acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	rec := createRecord(t, s, CreateInput{Kind: "bookmark", Title: "b", Fields: map[string]any{"url": "https://a.example"}})
	if _, _, err := s.Update(ctx, rec.ID, agent("a"), "", UpdateInput{Version: rec.Version, Archived: pointer(true)}); err != nil {
		t.Fatal(err)
	}
	next := bookmarkDefinition()
	delete(next, "statuses")
	next["policy"] = map[string]any{"review": true}
	pending, _, _ := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"pending": next}})
	if _, _, err := s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: pending.Version, AcceptPending: true}); err == nil || !strings.Contains(err.Error(), "review cannot change") {
		t.Fatalf("review change with only an archived record: %v", err)
	}
}

func TestAcceptPendingChecksNewUniqueField(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	def := bookmarkDefinition()
	def["schema"].(map[string]any)["author"] = map[string]any{"type": "string"}
	k := acceptKind(t, s, "bookmark", def, "")
	first := createRecord(t, s, CreateInput{Kind: "bookmark", Title: "1", Fields: map[string]any{"url": "https://1.example", "author": "ann"}})
	second := createRecord(t, s, CreateInput{Kind: "bookmark", Title: "2", Fields: map[string]any{"url": "https://2.example", "author": "ann"}})
	archived := createRecord(t, s, CreateInput{Kind: "bookmark", Title: "3", Fields: map[string]any{"url": "https://3.example", "author": "bob"}})
	dup := createRecord(t, s, CreateInput{Kind: "bookmark", Title: "4", Fields: map[string]any{"url": "https://4.example", "author": "bob"}})
	if _, _, err := s.Update(ctx, archived.ID, agent("a"), "", UpdateInput{Version: archived.Version, Archived: pointer(true)}); err != nil {
		t.Fatal(err)
	}
	next := bookmarkDefinition()
	next["schema"].(map[string]any)["author"] = map[string]any{"type": "string", "unique": true}
	pending, _, err := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"pending": next}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: pending.Version, AcceptPending: true})
	if err == nil || !strings.Contains(err.Error(), "change breaks 1 records") || !strings.Contains(err.Error(), "fields.author duplicates") || !strings.Contains(err.Error(), first.ID) || !strings.Contains(err.Error(), second.ID) || strings.Contains(err.Error(), dup.ID) || strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("duplicate on a new unique field: %v", err)
	}
	if current, _ := s.Get(ctx, k.ID); current.Version != pending.Version || current.Fields["pending"] == nil {
		t.Fatalf("a refused change modified the kind: %+v", current)
	}
}

func TestAcceptPendingRefusesRemovedStatusInUse(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	k := acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	rec := createRecord(t, s, CreateInput{Kind: "bookmark", Title: "b", Status: "reading", Fields: map[string]any{"url": "https://a.example"}})
	next := bookmarkDefinition()
	next["statuses"] = []any{"unread", "done"}
	pending, _, err := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"pending": next}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: pending.Version, AcceptPending: true})
	if err == nil || !strings.Contains(err.Error(), rec.ID+" status reading is no longer allowed") {
		t.Fatalf("removed status: %v", err)
	}
}

func TestUniqueFieldRefusesDuplicates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	first := createRecord(t, s, CreateInput{Kind: "bookmark", Title: "One", Fields: map[string]any{"url": "https://same.example"}})
	_, _, err := s.Create(ctx, agent("a"), "", CreateInput{Kind: "bookmark", Title: "Two", Fields: map[string]any{"url": "https://same.example"}})
	var dup *DuplicateError
	if !errors.As(err, &dup) || dup.Field != "url" || dup.ExistingID != first.ID || err.Error() != "fields.url: already used by "+first.ID {
		t.Fatalf("duplicate: %v", err)
	}
	if _, _, err := s.Update(ctx, first.ID, agent("a"), "", UpdateInput{Version: 1, Status: pointer("done")}); err != nil {
		t.Fatalf("keeping its own value: %v", err)
	}
	createRecord(t, s, CreateInput{Kind: "bookmark", Title: "Other slash", Fields: map[string]any{"url": "https://same.example/"}})
	archived, _, err := s.Update(ctx, first.ID, agent("a"), "", UpdateInput{Version: 2, Archived: pointer(true)})
	if err != nil {
		t.Fatal(err)
	}
	second := createRecord(t, s, CreateInput{Kind: "bookmark", Title: "Two", Fields: map[string]any{"url": "https://same.example"}})
	if _, _, err := s.Update(ctx, archived.ID, agent("a"), "", UpdateInput{Version: archived.Version, Archived: pointer(false)}); !errors.As(err, &dup) || dup.ExistingID != second.ID {
		t.Fatalf("restoring a duplicate: %v", err)
	}
}

func TestUniqueIntegerAndMakingFieldUnique(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	def := map[string]any{"schema": map[string]any{"n": map[string]any{"type": "integer"}}, "statuses": []any{"a"}, "policy": map[string]any{}}
	k := acceptKind(t, s, "ticket", def, "")
	createRecord(t, s, CreateInput{Kind: "ticket", Title: "a", Fields: map[string]any{"n": 7}})
	createRecord(t, s, CreateInput{Kind: "ticket", Title: "b", Fields: map[string]any{"n": "7"}})
	unique := map[string]any{"schema": map[string]any{"n": map[string]any{"type": "integer", "unique": true}}, "statuses": []any{"a"}, "policy": map[string]any{}}
	pending, _, _ := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"pending": unique}})
	if _, _, err := s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: pending.Version, AcceptPending: true}); err == nil || !strings.Contains(err.Error(), "fields.n duplicates") {
		t.Fatalf("making a duplicated field unique: %v", err)
	}
}

func TestConcurrentUniqueCreatesHaveOneWinner(t *testing.T) {
	s := testStore(t)
	acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = s.Create(context.Background(), agent("a"), "", CreateInput{Kind: "bookmark", Title: "race", Fields: map[string]any{"url": "https://race.example"}})
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		var dup *DuplicateError
		switch {
		case err == nil:
			wins++
		case !errors.As(err, &dup):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d creates succeeded", wins)
	}
}

func TestAcceptPendingRacesRecordWrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	k := acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	next := bookmarkDefinition()
	next["schema"].(map[string]any)["rating"] = map[string]any{"type": "integer", "required": true}
	pending, _, _ := s.Update(ctx, k.ID, agent("a"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"pending": next}})
	var wg sync.WaitGroup
	var acceptErr, createErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, acceptErr = s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: pending.Version, AcceptPending: true})
	}()
	go func() {
		defer wg.Done()
		_, _, createErr = s.Create(ctx, agent("a"), "", CreateInput{Kind: "bookmark", Title: "no rating", Fields: map[string]any{"url": "https://r.example"}})
	}()
	wg.Wait()
	// Either the record landed first and blocked the change, or the change
	// landed first and refused the record. Never both.
	if (acceptErr == nil) == (createErr == nil) {
		t.Fatalf("accept=%v create=%v", acceptErr, createErr)
	}
}

func TestKindStatusCannotChangeWhileArchiving(t *testing.T) {
	s := testStore(t)
	proposed := proposeKind(t, s, "bookmark", bookmarkDefinition(), "")
	if _, _, err := s.Update(context.Background(), proposed.ID, reviewer("owner"), "", UpdateInput{Version: proposed.Version, Status: pointer("accepted"), Archived: pointer(true)}); err == nil {
		t.Fatal("a proposed kind was accepted and archived in one update")
	}
}

func TestKindNameCustomIsReserved(t *testing.T) {
	s := testStore(t)
	if _, _, err := s.Create(context.Background(), agent("a"), "", CreateInput{Kind: "kind", Title: "custom", Fields: map[string]any{"definition": bookmarkDefinition()}}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("kind named custom: %v", err)
	}
}

func TestAcceptPendingRefreshesSearchText(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	def := bookmarkDefinition()
	def["schema"].(map[string]any)["platform"] = map[string]any{"type": "string"}
	k := acceptKind(t, s, "bookmark", def, "")
	createRecord(t, s, CreateInput{Kind: "bookmark", Title: "Plain", Fields: map[string]any{"url": "https://p.example", "platform": "web"}})
	if found, err := s.List(ctx, ListOptions{Kind: "bookmark", Query: "web"}); err != nil || len(found.Items) != 1 {
		t.Fatalf("before accept: %+v %v", found, err)
	}
	// bookmarkDefinition makes platform an enum, which search does not index.
	pending, _, err := s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: k.Version, Fields: map[string]any{"pending": bookmarkDefinition()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Update(ctx, k.ID, reviewer("owner"), "", UpdateInput{Version: pending.Version, AcceptPending: true}); err != nil {
		t.Fatal(err)
	}
	if found, err := s.List(ctx, ListOptions{Kind: "bookmark", Query: "web"}); err != nil || len(found.Items) != 0 {
		t.Fatalf("an enum value is still searchable: %+v %v", found, err)
	}
	if found, err := s.List(ctx, ListOptions{Kind: "bookmark", Query: "p.example"}); err != nil || len(found.Items) != 1 {
		t.Fatalf("url no longer searchable: %+v %v", found, err)
	}
}
