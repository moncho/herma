package store

import (
	"context"
	"errors"
	"testing"
)

func reviewer(name string) Author { return Author{Name: name, Role: RoleReviewer} }
func agent(name string) Author    { return Author{Name: name, Role: RoleAgent} }

var reviewSources = []string{"https://example.com/review"}

func TestAgentCreatesDurableRecordsOnlyAsProposed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r, _, err := s.Create(ctx, agent("agent-a"), "", CreateInput{Kind: "knowledge", Title: "Proposed fact"})
	if err != nil || r.Status != "proposed" || r.ReviewedBy != "" || r.ReviewedAt != nil {
		t.Fatalf("proposed create: %+v %v", r, err)
	}
	for _, status := range []string{"accepted", "rejected", "superseded"} {
		_, _, err := s.Create(ctx, agent("agent-a"), "self-approve-"+status, CreateInput{Kind: "principle", Title: "Self-approved", Status: status, Sources: reviewSources})
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("agent create with status %s: %v", status, err)
		}
	}
	result, err := s.List(ctx, ListOptions{Kind: "principle", Archived: true})
	if err != nil || result.Total != 0 {
		t.Fatalf("forbidden creates wrote records: %+v %v", result, err)
	}
	var receipts int
	if err := s.db.QueryRow("SELECT count(*) FROM idempotency").Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("forbidden creates stored %d receipts (%v)", receipts, err)
	}
	if _, _, err := s.Create(ctx, agent("agent-a"), "", CreateInput{Kind: "task", Title: "Agent task", Status: "in_progress"}); err != nil {
		t.Fatalf("agent coordination create: %v", err)
	}
}

func TestAgentEditsOnlyProposedDurableRecords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	proposed, _, err := s.Create(ctx, agent("agent-a"), "", CreateInput{Kind: "knowledge", Title: "Draft"})
	if err != nil {
		t.Fatal(err)
	}
	edited, _, err := s.Update(ctx, proposed.ID, agent("agent-b"), "", UpdateInput{Version: 1, Body: pointer("Refined draft")})
	if err != nil || edited.Body != "Refined draft" {
		t.Fatalf("agent edit of proposed record: %+v %v", edited, err)
	}
	if _, _, err := s.Update(ctx, proposed.ID, agent("agent-b"), "", UpdateInput{Version: 2, Status: pointer("accepted"), Sources: pointer(reviewSources)}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent status change: %v", err)
	}
	if _, _, err := s.Update(ctx, proposed.ID, agent("agent-b"), "", UpdateInput{Version: 2, Archived: pointer(true)}); err != nil {
		t.Fatalf("agent archive of proposed record: %v", err)
	}
	for _, status := range []string{"accepted", "rejected", "superseded"} {
		judged, _, err := s.Create(ctx, reviewer("owner"), "", CreateInput{Kind: "principle", Title: "Judged " + status, Status: status, Sources: reviewSources})
		if err != nil {
			t.Fatal(err)
		}
		for name, input := range map[string]UpdateInput{
			"edit":    {Version: 1, Body: pointer("Changed by an agent")},
			"archive": {Version: 1, Archived: pointer(true)},
			"status":  {Version: 1, Status: pointer("proposed")},
		} {
			if _, _, err := s.Update(ctx, judged.ID, agent("agent-b"), "", input); !errors.Is(err, ErrForbidden) {
				t.Fatalf("agent %s of %s record: %v", name, status, err)
			}
		}
		history, err := s.History(ctx, judged.ID)
		if err != nil || len(history) != 1 {
			t.Fatalf("forbidden updates changed %s history: %d %v", status, len(history), err)
		}
	}
}

func TestReviewerAcceptanceRequiresSourcesAndRecordsReview(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	proposed, _, err := s.Create(ctx, agent("agent-a"), "", CreateInput{Kind: "knowledge", Title: "Candidate"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Update(ctx, proposed.ID, reviewer("owner"), "", UpdateInput{Version: 1, Status: pointer("accepted")})
	assertValidation(t, err)
	accepted, _, err := s.Update(ctx, proposed.ID, reviewer("owner"), "", UpdateInput{Version: 1, Status: pointer("accepted"), Sources: pointer(reviewSources)})
	if err != nil || accepted.ReviewedBy != "owner" || accepted.ReviewedAt == nil || accepted.ReviewedAt.Location().String() != "UTC" {
		t.Fatalf("accept did not record review: %+v %v", accepted, err)
	}
	reopened, _, err := s.Update(ctx, proposed.ID, reviewer("owner"), "", UpdateInput{Version: 2, Status: pointer("proposed")})
	if err != nil || reopened.ReviewedBy != "" || reopened.ReviewedAt != nil {
		t.Fatalf("return to proposed kept review fields: %+v %v", reopened, err)
	}
	rejected, _, err := s.Update(ctx, proposed.ID, reviewer("owner"), "", UpdateInput{Version: 3, Status: pointer("rejected")})
	if err != nil || rejected.ReviewedBy != "owner" || rejected.ReviewedAt == nil {
		t.Fatalf("reject did not record review: %+v %v", rejected, err)
	}
	direct, _, err := s.Create(ctx, reviewer("owner"), "", CreateInput{Kind: "principle", Title: "Directly accepted", Status: "accepted", Sources: reviewSources})
	if err != nil || direct.ReviewedBy != "owner" || direct.ReviewedAt == nil {
		t.Fatalf("reviewer create did not record review: %+v %v", direct, err)
	}
	_, _, err = s.Create(ctx, reviewer("owner"), "", CreateInput{Kind: "task", Title: "Coordination", Status: "rejected"})
	assertValidation(t, err)
}

func TestReadOnlyAndInvalidRolesCannotWrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := createRecord(t, s, CreateInput{Kind: "task", Title: "Existing"})
	reader := Author{Name: "reader", Role: RoleReadOnly}
	if _, _, err := s.Create(ctx, reader, "", CreateInput{Kind: "task", Title: "Read-only write"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only create: %v", err)
	}
	if _, _, err := s.Update(ctx, r.ID, reader, "", UpdateInput{Version: 1, Title: pointer("Changed")}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only update: %v", err)
	}
	_, _, err := s.Create(ctx, Author{Name: "admin", Role: "admin"}, "", CreateInput{Kind: "task", Title: "Unknown role"})
	assertValidation(t, err)
}

func TestAgentEditAndReviewerAcceptRaceHaveOneWinner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	proposed, _, err := s.Create(ctx, agent("agent-a"), "", CreateInput{Kind: "knowledge", Title: "Contested"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, _, err := s.Update(ctx, proposed.ID, agent("agent-b"), "", UpdateInput{Version: 1, Body: pointer("Agent edit")})
		results <- err
	}()
	go func() {
		<-start
		_, _, err := s.Update(ctx, proposed.ID, reviewer("owner"), "", UpdateInput{Version: 1, Status: pointer("accepted"), Sources: pointer(reviewSources)})
		results <- err
	}()
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) || !(errors.Is(first, ErrConflict) || errors.Is(second, ErrConflict)) {
		t.Fatalf("want one success and one conflict, got %v and %v", first, second)
	}
	current, err := s.Get(ctx, proposed.ID)
	if err != nil || current.Version != 2 {
		t.Fatalf("current: %+v %v", current, err)
	}
	if current.Status == "accepted" && current.ReviewedBy != "owner" {
		t.Fatalf("accepted winner lacks review fields: %+v", current)
	}
	if current.Status == "proposed" && current.Body != "Agent edit" {
		t.Fatalf("agent winner lost its edit: %+v", current)
	}
}
