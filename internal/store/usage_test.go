package store

import (
	"context"
	"testing"
	"time"
)

func usageByID(t *testing.T, s *Store, o UsageOptions) (map[string]Usage, []string, int) {
	t.Helper()
	result, err := s.Usage(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	byID, order := map[string]Usage{}, []string{}
	for _, u := range result.Items {
		byID[u.ID] = u
		order = append(order, u.ID)
	}
	return byID, order, result.Total
}

func TestUsageCountsRecallsAndFetchesSeparately(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	used := accepted(t, s, "knowledge", "Retry budget", "body", "")
	unused := accepted(t, s, "principle", "Prefer simple", "body", "")
	before := time.Now().UTC()
	if err := s.CountRecalled(ctx, []string{used.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.CountRecalled(ctx, []string{used.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.CountFetched(ctx, used.ID); err != nil {
		t.Fatal(err)
	}
	byID, _, total := usageByID(t, s, UsageOptions{})
	if total != 2 || len(byID) != 2 {
		t.Fatalf("usage lists %d of %d records, want 2 of 2", len(byID), total)
	}
	got := byID[used.ID]
	if got.Recalled != 2 || got.Fetched != 1 {
		t.Errorf("used: recalled %d fetched %d, want 2 and 1", got.Recalled, got.Fetched)
	}
	if got.Kind != "knowledge" || got.Title != "Retry budget" || got.ReviewedAt == nil {
		t.Errorf("used: %+v", got)
	}
	if got.LastUsedAt == nil || got.LastUsedAt.Before(before) || got.LastUsedAt.After(time.Now().UTC()) {
		t.Errorf("used: last_used_at %v, want between %v and now", got.LastUsedAt, before)
	}
	if never := byID[unused.ID]; never.Recalled != 0 || never.Fetched != 0 || never.LastUsedAt != nil {
		t.Errorf("unused: %+v", never)
	}
}

func TestUsageLastUsedFollowsTheLatestUse(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := accepted(t, s, "knowledge", "Retry budget", "body", "")
	if err := s.CountRecalled(ctx, []string{r.ID}); err != nil {
		t.Fatal(err)
	}
	first, _, _ := usageByID(t, s, UsageOptions{})
	time.Sleep(5 * time.Millisecond)
	between := time.Now().UTC()
	time.Sleep(5 * time.Millisecond)
	if err := s.CountFetched(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	second, _, _ := usageByID(t, s, UsageOptions{})
	if !first[r.ID].LastUsedAt.Before(between) || !second[r.ID].LastUsedAt.After(between) {
		t.Errorf("last used %v then %v, want one before and one after %v", first[r.ID].LastUsedAt, second[r.ID].LastUsedAt, between)
	}
}

func TestUsageDoesNotChangeTheRecord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := accepted(t, s, "knowledge", "Retry budget", "body", "")
	if err := s.CountRecalled(ctx, []string{r.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.CountFetched(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != 1 || !after.UpdatedAt.Equal(r.UpdatedAt) {
		t.Errorf("counting changed the record: version %d, updated %v (was %v)", after.Version, after.UpdatedAt, r.UpdatedAt)
	}
	history, err := s.History(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Errorf("counting wrote %d revisions, want 1", len(history))
	}
}

func TestUsageListsLeastUsedFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	most := accepted(t, s, "knowledge", "Most used", "body", "")
	never := accepted(t, s, "knowledge", "Never used", "body", "")
	once := accepted(t, s, "knowledge", "Used once", "body", "")
	for i := 0; i < 3; i++ {
		if err := s.CountFetched(ctx, most.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CountRecalled(ctx, []string{once.ID, most.ID}); err != nil {
		t.Fatal(err)
	}
	_, order, _ := usageByID(t, s, UsageOptions{})
	want := []string{never.ID, once.ID, most.ID}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Errorf("order %v, want %v", order, want)
	}
	_, limited, total := usageByID(t, s, UsageOptions{Limit: 1})
	if total != 3 || len(limited) != 1 || limited[0] != never.ID {
		t.Errorf("limit 1: %v of %d, want [%s] of 3", limited, total, never.ID)
	}
}

// Usage answers which reviewed records agents use, so it lists what recall
// can return: accepted durable records, not proposals, rejected or archived
// records, or coordination records.
func TestUsageListsOnlyWhatRecallReturns(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	kept := accepted(t, s, "knowledge", "Kept", "body", "")
	proposed := createRecord(t, s, CreateInput{Kind: "knowledge", Title: "Proposed", Body: "body"})
	task := createRecord(t, s, CreateInput{Kind: "task", Title: "A task", Body: "body"})
	archived := accepted(t, s, "knowledge", "Archived", "body", "")
	yes := true
	if _, _, err := s.Update(ctx, archived.ID, reviewer("owner"), "", UpdateInput{Version: archived.Version, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	// A fetch of a record usage does not list is still counted without error.
	for _, id := range []string{proposed.ID, task.ID, archived.ID} {
		if err := s.CountFetched(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	byID, order, total := usageByID(t, s, UsageOptions{})
	if total != 1 || len(order) != 1 || order[0] != kept.ID {
		t.Errorf("usage lists %v (total %d), want only %s", order, total, kept.ID)
	}
	if _, ok := byID[kept.ID]; !ok {
		t.Errorf("accepted record missing: %v", order)
	}
}

func TestUsageProjectFilterKeepsGlobalRecords(t *testing.T) {
	s := testStore(t)
	mine := createRecord(t, s, CreateInput{Kind: "project", Title: "Mine"})
	other := createRecord(t, s, CreateInput{Kind: "project", Title: "Other"})
	global := accepted(t, s, "principle", "Global", "body", "")
	local := accepted(t, s, "knowledge", "Local", "body", mine.ID)
	foreign := accepted(t, s, "knowledge", "Foreign", "body", other.ID)
	byID, _, total := usageByID(t, s, UsageOptions{ProjectID: mine.ID})
	if total != 2 || len(byID) != 2 {
		t.Fatalf("project filter lists %d of %d, want 2", len(byID), total)
	}
	if _, ok := byID[global.ID]; !ok {
		t.Error("global record missing from a project's usage")
	}
	if byID[local.ID].ProjectID != mine.ID {
		t.Errorf("local record: %+v", byID[local.ID])
	}
	if _, ok := byID[foreign.ID]; ok {
		t.Error("another project's record listed")
	}
	if all, _, _ := usageByID(t, s, UsageOptions{}); len(all) != 3 {
		t.Errorf("no filter lists %d, want 3", len(all))
	}
}

func TestUsageRejectsBadOptionsAndUnknownRecords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, o := range []UsageOptions{{Limit: -1}, {Limit: 201}, {ProjectID: string(make([]byte, 101))}} {
		if _, err := s.Usage(ctx, o); err == nil {
			t.Errorf("Usage(%+v) accepted", o)
		} else {
			assertValidation(t, err)
		}
	}
	if err := s.CountFetched(ctx, "rec_00000000000000000000000000000000"); err == nil {
		t.Error("counted a fetch of a record that does not exist")
	}
	if err := s.CountRecalled(ctx, nil); err != nil {
		t.Errorf("counting no records: %v", err)
	}
}
