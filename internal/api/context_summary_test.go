package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func requestSummary(t *testing.T, h http.Handler, projectID string) string {
	t.Helper()
	response := apiTestRequest(h, http.MethodGet, "/v1/context?format=summary&project_id="+projectID, "", "", "Bearer "+apiTestToken, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	return response.Body.String()
}

func TestContextSummaryCountsWithoutRecordText(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Busy"})
	createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "Secret task text", ProjectID: project.ID})
	createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "Second task", ProjectID: project.ID})
	createContextRecord(t, s, store.CreateInput{Kind: "feedback", Title: "Friction", ProjectID: project.ID})
	note := createContextRecord(t, s, store.CreateInput{Kind: "note", Title: "Handoff", ProjectID: project.ID})
	createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Rule", Status: "accepted", Sources: []string{"https://example.com/review"}, ProjectID: project.ID})
	got := requestSummary(t, h, project.ID)
	want := "herma coordination: Feedback 1 · Tasks 2 · Notes 1 (latest " + note.UpdatedAt.UTC().Format("2006-01-02") + ") — read them with: herma context\n"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if strings.Contains(got, "Secret") || strings.Contains(got, "Rule") {
		t.Fatal("summary leaked record text")
	}
}

func TestContextSummaryIsEmptyWhenNothingIsOpen(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Quiet"})
	if got := requestSummary(t, h, project.ID); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestContextSummaryMarksSectionsWithMore(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Many"})
	for i := 0; i < 101; i++ {
		createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "Task", ProjectID: project.ID})
	}
	if got := requestSummary(t, h, project.ID); !strings.Contains(got, "Tasks 100+") {
		t.Fatalf("got %q", got)
	}
}
