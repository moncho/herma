package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

var recallSources = []string{"https://example.com/review"}

func accepted(t *testing.T, s *Store, kind, title, body, project string) Record {
	t.Helper()
	r, _, err := s.Create(context.Background(), reviewer("owner"), "", CreateInput{Kind: kind, Title: title, Body: body, ProjectID: project, Status: "accepted", Sources: recallSources})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func recallIDs(t *testing.T, s *Store, o RecallOptions) []string {
	t.Helper()
	records, err := s.Recall(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(records))
	for i, r := range records {
		ids[i] = r.ID
	}
	return ids
}

func TestRecallQueryTerms(t *testing.T) {
	for query, want := range map[string][2]string{
		"api":               {`"api"`, ``},
		"sync":              {`"sync"*`, ``},
		"retries":           {`"retries"*`, `"retr"*`},
		"a retries retries": {`"retries"*`, `"retr"*`},
		// A stem equal to a whole-word term is not repeated in the stem pass.
		"retr retries": {`"retr"* OR "retries"*`, ``},
		// One-rune words (x, y) are dropped; keywords become quoted plain terms.
		`title:x OR "NEAR" (y)`: {`"title"* OR "OR" OR "NEAR"*`, `"titl"*`},
	} {
		whole, stems, err := recallQuery(query)
		if err != nil || whole != want[0] || stems != want[1] {
			t.Errorf("recallQuery(%q) = %q, %q, %v; want %q", query, whole, stems, err, want)
		}
	}
	for _, bad := range []string{"", "a ! ?", strings.Join(distinctWords(33), " ")} {
		if _, _, err := recallQuery(bad); err == nil {
			t.Errorf("recallQuery(%q) accepted", bad)
		}
	}
}

func distinctWords(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("w%02d", i)
	}
	return out
}

func TestRecallWordCapCountsDistinctWords(t *testing.T) {
	words := distinctWords
	// 32 distinct words of 2+ runes, but 37 raw pieces counting s, d and t.
	ok := strings.Join(words(29), " ") + " what's we'd don't what's"
	if _, _, err := recallQuery(ok); err != nil {
		t.Errorf("32 distinct words with contractions rejected: %v", err)
	}
	if _, _, err := recallQuery(strings.Join(words(33), " ")); err == nil {
		t.Error("33 distinct words accepted")
	}
}

func TestRecallWholeWordMatchesRankBeforeStemMatches(t *testing.T) {
	s := testStore(t)
	// The stem-only record matches the stem "retr" many times in its title.
	unrelated := accepted(t, s, "knowledge", "Retro retro retro retro retrospective retrofit retry retrace", "Team retro outcomes.", "")
	wholes := map[string]bool{}
	for i := 0; i < 4; i++ {
		r := accepted(t, s, "knowledge", fmt.Sprintf("Backoff policy %d", i), strings.Repeat("filler ", 100)+"retries", "")
		wholes[r.ID] = true
	}
	got := recallIDs(t, s, RecallOptions{Query: "retries"})
	if len(got) != 5 || got[4] != unrelated.ID {
		t.Fatalf("stem-only match not ranked after whole-word matches: %v (stem-only %s)", got, unrelated.ID)
	}
	if limited := recallIDs(t, s, RecallOptions{Query: "retries", Limit: 4}); len(limited) != 4 || !wholes[limited[3]] {
		t.Errorf("limit across passes: got %v", limited)
	}
	if limited := recallIDs(t, s, RecallOptions{Query: "retries", Limit: 1}); !reflect.DeepEqual(limited, got[:1]) {
		t.Errorf("limit 1: got %v, want %v", limited, got[:1])
	}
}

func TestRecallFindsStemOnlyMatchesWithoutWholeWordMatch(t *testing.T) {
	s := testStore(t)
	unrelated := accepted(t, s, "knowledge", "Retrospective notes", "Team retro outcomes.", "")
	retry := accepted(t, s, "knowledge", "Retry budget", "How many attempts a client may make before giving up on a failed request and reporting the error upstream.", "")
	for i := 0; i < 5; i++ {
		accepted(t, s, "knowledge", fmt.Sprintf("Filler %d", i), "unrelated text", "")
	}
	got := recallIDs(t, s, RecallOptions{Query: "retries"})
	if len(got) != 2 || got[0] == got[1] || (got[0] != retry.ID && got[0] != unrelated.ID) || (got[1] != retry.ID && got[1] != unrelated.ID) {
		t.Errorf("stem-only matches not both found: %v", got)
	}
}

func TestRecallRanksTitleAndMoreMatchesFirst(t *testing.T) {
	s := testStore(t)
	body := accepted(t, s, "knowledge", "Other topic", "notes about the retry path", "")
	title := accepted(t, s, "knowledge", "Retry path", "notes about something else", "")
	both := accepted(t, s, "knowledge", "Timeouts", "retry with backoff and jitter", "")
	if got := recallIDs(t, s, RecallOptions{Query: "retry"}); len(got) != 3 || got[0] != title.ID {
		t.Fatalf("title match not first: %v (title %s, body %s, both %s)", got, title.ID, body.ID, both.ID)
	}
	// Among body-only matches, the record containing more query words ranks higher.
	got := recallIDs(t, s, RecallOptions{Query: "retry backoff"})
	position := map[string]int{}
	for i, id := range got {
		position[id] = i
	}
	if pb, okB := position[both.ID]; !okB || pb > position[body.ID] {
		t.Fatalf("record matching more words ranked below one matching fewer: %v", got)
	}
}

func TestRecallMatchesStemsPrefixesAndAnyWord(t *testing.T) {
	s := testStore(t)
	retry := accepted(t, s, "knowledge", "Retry budget", "", "")
	api := accepted(t, s, "knowledge", "The api gateway", "", "")
	accepted(t, s, "knowledge", "Apis everywhere", "", "")
	accented := accepted(t, s, "knowledge", "Configuración del servidor", "", "")
	if got := recallIDs(t, s, RecallOptions{Query: "retries"}); !reflect.DeepEqual(got, []string{retry.ID}) {
		t.Errorf("retries did not find retry: %v", got)
	}
	if got := recallIDs(t, s, RecallOptions{Query: "api"}); !reflect.DeepEqual(got, []string{api.ID}) {
		t.Errorf("three-letter word matched more than exactly: %v", got)
	}
	if got := recallIDs(t, s, RecallOptions{Query: "zebra quantum retry"}); !reflect.DeepEqual(got, []string{retry.ID}) {
		t.Errorf("one matching word did not find the record: %v", got)
	}
	if got := recallIDs(t, s, RecallOptions{Query: "configuracion"}); !reflect.DeepEqual(got, []string{accented.ID}) {
		t.Errorf("unaccented query missed accented record: %v", got)
	}
	if got := recallIDs(t, s, RecallOptions{Query: `"retry" OR NEAR( title:budget`}); len(got) == 0 || got[0] != retry.ID {
		t.Errorf("FTS syntax in query was not plain text: %v", got)
	}
}

func TestRecallEligibility(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	keep := accepted(t, s, "principle", "Widget rule", "", "")
	proposed, _, err := s.Create(ctx, agent("agent"), "", CreateInput{Kind: "knowledge", Title: "Widget idea"})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"rejected", "superseded"} {
		if _, _, err := s.Create(ctx, reviewer("owner"), "", CreateInput{Kind: "knowledge", Title: "Widget " + status, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	archived := accepted(t, s, "knowledge", "Widget archived", "", "")
	if _, _, err := s.Update(ctx, archived.ID, reviewer("owner"), "", UpdateInput{Version: 1, Archived: pointer(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, reviewer("owner"), "", CreateInput{Kind: "task", Title: "Widget task"}); err != nil {
		t.Fatal(err)
	}
	if got := recallIDs(t, s, RecallOptions{Query: "widget"}); !reflect.DeepEqual(got, []string{keep.ID}) {
		t.Errorf("default eligibility: %v", got)
	}
	got := recallIDs(t, s, RecallOptions{Query: "widget", IncludeProposed: true})
	if len(got) != 2 || got[0] != keep.ID && got[1] != keep.ID || got[0] != proposed.ID && got[1] != proposed.ID {
		t.Errorf("include proposed: %v", got)
	}
}

func TestRecallProjectScopeAndStableOrder(t *testing.T) {
	s := testStore(t)
	p := createRecord(t, s, CreateInput{Kind: "project", Title: "P"})
	q := createRecord(t, s, CreateInput{Kind: "project", Title: "Q"})
	global := accepted(t, s, "knowledge", "Gadget notes", "same text", "")
	inP := accepted(t, s, "knowledge", "Gadget notes", "same text", p.ID)
	inQ := accepted(t, s, "knowledge", "Gadget notes", "same text", q.ID)
	if got := recallIDs(t, s, RecallOptions{Query: "gadget", ProjectID: p.ID}); !reflect.DeepEqual(got, []string{inP.ID, global.ID}) {
		t.Errorf("project scope: %v", got)
	}
	all := recallIDs(t, s, RecallOptions{Query: "gadget"})
	if len(all) != 3 {
		t.Fatalf("all projects: %v (q %s)", all, inQ.ID)
	}
	if again := recallIDs(t, s, RecallOptions{Query: "gadget"}); !reflect.DeepEqual(all, again) {
		t.Errorf("order changed between runs: %v vs %v", all, again)
	}
}

func TestRecallValidatesOptions(t *testing.T) {
	s := testStore(t)
	for name, o := range map[string]RecallOptions{
		"no words":  {Query: "!!"},
		"limit 101": {Query: "x1", Limit: 101},
		"limit -1":  {Query: "x1", Limit: -1},
		"too long":  {Query: strings.Repeat("x", 501)},
	} {
		if _, err := s.Recall(context.Background(), o); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			assertValidation(t, err)
		}
	}
}

func TestRecallIncludesUnreviewedCustomKinds(t *testing.T) {
	s := testStore(t)
	acceptKind(t, s, "bookmark", bookmarkDefinition(), "")
	noRecall := bookmarkDefinition()
	noRecall["policy"] = map[string]any{"recall": false}
	acceptKind(t, s, "hidden", noRecall, "")
	b := createRecord(t, s, CreateInput{Kind: "bookmark", Title: "Write-ahead logging", Fields: map[string]any{"url": "https://sqlite.org/wal.html"}})
	createRecord(t, s, CreateInput{Kind: "hidden", Title: "Write-ahead logging", Fields: map[string]any{"url": "https://h.example"}})
	k := accepted(t, s, "knowledge", "Write-ahead logging is on", "", "")
	results, err := s.Recall(context.Background(), RecallOptions{Query: "write ahead logging"})
	if err != nil || len(results) != 2 {
		t.Fatalf("%+v %v", results, err)
	}
	byID := map[string]bool{}
	for _, r := range results {
		byID[r.ID] = r.Reviewed
	}
	if reviewed, ok := byID[k.ID]; !ok || !reviewed {
		t.Error("accepted knowledge must be reviewed")
	}
	if reviewed, ok := byID[b.ID]; !ok || reviewed {
		t.Error("bookmark must be present and unreviewed")
	}
}
