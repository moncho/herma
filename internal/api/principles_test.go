package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func requestPrinciples(t *testing.T, h http.Handler, projectID string) string {
	t.Helper()
	path := "/v1/principles"
	if projectID != "" {
		path += "?project_id=" + projectID
	}
	response := apiTestRequest(h, http.MethodGet, path, "", "", "Bearer "+apiTestToken, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	return response.Body.String()
}

func TestPrinciplesFileRendersAcceptedPrinciplesInContextOrder(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Rules"})
	other := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Other"})
	src := []string{"https://example.com/review"}
	older := createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Older rule", Body: "Run `make test`.\n\n```sh\nmake test\n```\n", Status: "accepted", Sources: src, ProjectID: project.ID})
	global := createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Global rule", Body: "Applies everywhere.", Status: "accepted", Sources: src})
	urgent := createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Urgent rule", Status: "accepted", Priority: 5, Sources: src, ProjectID: project.ID})
	createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Other project rule", Status: "accepted", Sources: src, ProjectID: other.ID})
	createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Proposed rule", ProjectID: project.ID})
	for _, status := range []string{"rejected", "superseded"} {
		createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: status + " rule", Status: status, ProjectID: project.ID})
	}
	want := principlesFileHeader +
		"\n## Urgent rule\n\nherma: " + urgent.ID + " · project\n" +
		"\n## Older rule\nRun `make test`.\n\n```sh\nmake test\n```\n\nherma: " + older.ID + " · project\n"
	if got := requestPrinciples(t, h, project.ID); got != want {
		t.Fatalf("rendered file:\n%s\nwant:\n%s", got, want)
	}
	wantGlobal := globalPrinciplesFileHeader + "\n## Global rule\nApplies everywhere.\n\nherma: " + global.ID + " · global\n"
	if got := requestPrinciples(t, h, ""); got != wantGlobal {
		t.Fatalf("global file:\n%s\nwant:\n%s", got, wantGlobal)
	}
}

func TestGlobalPrinciplesFileIsEmptyWhenNoneExist(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Only project"})
	createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Project rule", Status: "accepted", Sources: []string{"https://example.com/review"}, ProjectID: project.ID})
	if got := requestPrinciples(t, h, ""); got != "" {
		t.Fatalf("body = %q", got)
	}
}

func TestPrinciplesFileIsEmptyWhenNoneApply(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Empty"})
	if got := requestPrinciples(t, h, project.ID); got != "" {
		t.Fatalf("body = %q", got)
	}
}

func TestPrinciplesFileReportsPrinciplesBeyondTheCandidateLimit(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Many"})
	for i := 0; i < contextLimit+1; i++ {
		createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: fmt.Sprintf("Rule %03d", i), Status: "accepted", Sources: []string{"https://example.com/review"}, ProjectID: project.ID})
	}
	got := requestPrinciples(t, h, project.ID)
	if strings.Count(got, "\n## ") != contextLimit || !strings.HasSuffix(got, "\n1 more accepted principle (the lowest-priority one) is not shown; list them all with: herma list --project "+project.ID+" --kind principle --status accepted --limit 200\n") {
		t.Fatalf("limit not reported: %d sections, tail %q", strings.Count(got, "\n## "), got[max(0, len(got)-200):])
	}
	createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Global rule", Status: "accepted", Sources: []string{"https://example.com/review"}})
	if again := requestPrinciples(t, h, project.ID); again != got {
		t.Fatal("a global principle changed the project file")
	}
	for i := 0; i < contextLimit; i++ {
		createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: fmt.Sprintf("Global %03d", i), Status: "accepted", Sources: []string{"https://example.com/review"}})
	}
	global := requestPrinciples(t, h, "")
	if !strings.HasSuffix(global, "\n1 more accepted principle (the lowest-priority one) is not shown; list them all with: herma list --global --kind principle --status accepted --limit 200\n") {
		t.Fatalf("global limit not reported: tail %q", global[max(0, len(global)-200):])
	}
}

func TestPrinciplesFileFlattensMultilineTitles(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Titles"})
	createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "First line\n## Injected", Status: "accepted", Sources: []string{"https://example.com/review"}, ProjectID: project.ID})
	if got := requestPrinciples(t, h, project.ID); !strings.Contains(got, "\n## First line ## Injected\n") || strings.Contains(got, "\n## Injected") {
		t.Fatalf("title not flattened:\n%s", got)
	}
}

func TestPrinciplesEndpointValidatesTheProject(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	task := createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "Not a project"})
	archived := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Archived project"})
	if _, _, err := s.Update(context.Background(), archived.ID, store.Author{Name: "writer", Role: store.RoleReviewer}, "", store.UpdateInput{Version: archived.Version, Archived: pointerTo(true)}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query  string
		status int
		code   string
	}{
		{"?project_id=" + task.ID, http.StatusBadRequest, "invalid_request"},
		{"?project_id=rec_" + strings.Repeat("0", 32), http.StatusNotFound, projectUnavailableCode},
		{"?project_id=" + archived.ID, http.StatusGone, projectUnavailableCode},
		{"?project_id=" + task.ID + "&extra=1", http.StatusBadRequest, "invalid_request"},
	} {
		response := apiTestRequest(h, http.MethodGet, "/v1/principles"+tc.query, "", "", "Bearer "+apiTestToken, "")
		assertAPIError(t, response, tc.status)
		var problem struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
			t.Fatal(err)
		}
		if problem.Error.Code != tc.code {
			t.Errorf("%q: code = %q, want %q", tc.query, problem.Error.Code, tc.code)
		}
	}
}
