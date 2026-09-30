package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestSelfLinkRejectedWithoutRecordOrHistoryChanges(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := createRecord(t, s, CreateInput{Kind: "knowledge", Title: "Original decision"})
	_, replay, err := s.Update(ctx, r.ID, reviewer("agent-b"), "link-update", UpdateInput{
		Version: 1,
		Title:   pointer("This title must not be saved"),
		Links:   pointer([]string{r.ID}),
	})
	assertValidation(t, err)
	if replay || !strings.Contains(err.Error(), "record itself") {
		t.Fatalf("self-link did not produce a clear validation error: replay=%t, error=%v", replay, err)
	}
	current, err := s.Get(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(current, r) {
		t.Fatalf("rejected self-link changed the record: %+v, %v", current, err)
	}
	history, err := s.History(ctx, r.ID)
	if err != nil || len(history) != 1 || !reflect.DeepEqual(history[0].Record, r) {
		t.Fatalf("rejected self-link changed revision history: %+v, %v", history, err)
	}
	other := createRecord(t, s, CreateInput{Kind: "knowledge", Title: "Related decision"})
	updated, replay, err := s.Update(ctx, r.ID, reviewer("agent-b"), "link-update", UpdateInput{
		Version: 1,
		Links:   pointer([]string{other.ID}),
	})
	if err != nil || replay || updated.Version != 2 || !reflect.DeepEqual(updated.Links, []string{other.ID}) {
		t.Fatalf("rejected self-link consumed the revision or retry key: %+v, replay=%t, error=%v", updated, replay, err)
	}
}

func TestLinksToArchivedRecordsRemainAllowed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target := createRecord(t, s, CreateInput{Kind: "knowledge", Title: "Historical decision"})
	if _, _, err := s.Update(ctx, target.ID, reviewer("agent-a"), "", UpdateInput{Version: 1, Archived: pointer(true)}); err != nil {
		t.Fatal(err)
	}
	created := createRecord(t, s, CreateInput{
		Kind: "knowledge", Title: "References archived evidence", Links: []string{target.ID},
	})
	retained, _, err := s.Update(ctx, created.ID, reviewer("agent-b"), "", UpdateInput{
		Version: 1, Body: pointer("The archived decision remains relevant evidence."),
	})
	if err != nil || !reflect.DeepEqual(retained.Links, []string{target.ID}) {
		t.Fatalf("could not retain a link to archived evidence: %+v, %v", retained, err)
	}
	unlinked := createRecord(t, s, CreateInput{Kind: "knowledge", Title: "Another decision"})
	linked, _, err := s.Update(ctx, unlinked.ID, reviewer("agent-b"), "", UpdateInput{
		Version: 1, Links: pointer([]string{target.ID}),
	})
	if err != nil || !reflect.DeepEqual(linked.Links, []string{target.ID}) {
		t.Fatalf("could not add a link to archived evidence: %+v, %v", linked, err)
	}
}
