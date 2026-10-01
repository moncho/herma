package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/moncho/herma/internal/store"
)

func createContextRecord(t *testing.T, s *store.Store, input store.CreateInput) store.Record {
	t.Helper()
	record, _, err := s.Create(context.Background(), store.Author{Name: "context-test-session", Role: store.RoleReviewer}, "", input)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func decodeContextResponse(t *testing.T, response *httptest.ResponseRecorder, budget int) projectContext {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("context status = %d; body = %s", response.Code, response.Body.String())
	}
	data := response.Body.Bytes()
	if len(data) > budget || !bytes.HasSuffix(data, []byte{'\n'}) || !utf8.Valid(data) || !json.Valid(data) {
		t.Fatalf("invalid context wire response: length = %d, budget = %d, UTF-8 = %v, JSON = %v", len(data), budget, utf8.Valid(data), json.Valid(data))
	}
	if response.Header().Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Errorf("Content-Length = %q, actual length = %d", response.Header().Get("Content-Length"), len(data))
	}
	var packet projectContext
	if err := json.Unmarshal(data, &packet); err != nil {
		t.Fatal(err)
	}
	if packet.MaxBytes != budget || packet.Scope != contextScope || packet.Recall != recallHint || packet.GeneratedAt.IsZero() {
		t.Errorf("context metadata missing or inconsistent: %+v", packet)
	}
	return packet
}

func requestContext(t *testing.T, h http.Handler, projectID, extraQuery string, budget int) projectContext {
	t.Helper()
	response := apiTestRequest(h, http.MethodGet, "/v1/context?project_id="+projectID+extraQuery, "", "", "Bearer "+apiTestToken, "")
	return decodeContextResponse(t, response, budget)
}

func TestContextDefaultsToCoordinationAndPrinciplesAndOptsInKnowledge(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Session coordination", Body: "Current handoff scope."})
	task := createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "Next session intention", Body: "Continue the investigation.", ProjectID: project.ID, Owner: "next-session", Sources: []string{"https://linear.app/example/issue/ONE"}})
	feedback := createContextRecord(t, s, store.CreateInput{Kind: "feedback", Title: "Follow up", ProjectID: project.ID})
	note := createContextRecord(t, s, store.CreateInput{Kind: "note", Title: "Handoff", Body: "The latest experiment is in the workspace.", ProjectID: project.ID})
	knowledge := createContextRecord(t, s, store.CreateInput{Kind: "knowledge", Title: "Durable memory entry", Status: "accepted", Sources: []string{"https://example.com/review"}})
	principle := createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Project principle", Status: "accepted", Sources: []string{"https://example.com/review"}, ProjectID: project.ID})
	createContextRecord(t, s, store.CreateInput{Kind: "knowledge", Title: "Unreviewed idea", Status: "proposed", ProjectID: project.ID})
	createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "Completed intention", Status: "done", ProjectID: project.ID})
	createContextRecord(t, s, store.CreateInput{Kind: "feedback", Title: "Already handled", Status: "resolved", ProjectID: project.ID})

	packet := requestContext(t, h, project.ID, "", defaultContextBytes)
	if packet.IncludeDurable || packet.Truncated || packet.Omitted.any() || len(packet.Knowledge) != 0 || len(packet.Principles) != 1 || packet.Principles[0].ID != principle.ID {
		t.Errorf("default context must include accepted principles and exclude knowledge without claiming truncation: %+v", packet)
	}
	if packet.Recall != recallHint {
		t.Errorf("recall hint = %q", packet.Recall)
	}
	if packet.Project.ID != project.ID || len(packet.Tasks) != 1 || packet.Tasks[0].ID != task.ID || len(packet.Feedback) != 1 || packet.Feedback[0].ID != feedback.ID || len(packet.Notes) != 1 || packet.Notes[0].ID != note.ID {
		t.Fatalf("coordination context missing active records: %+v", packet)
	}
	if packet.Tasks[0].Body != task.Body || packet.Tasks[0].Owner != task.Owner || !reflect.DeepEqual(packet.Tasks[0].Sources, task.Sources) || packet.Tasks[0].UpdatedBy != task.UpdatedBy || packet.Tasks[0].Version != task.Version {
		t.Error("small coordination record lost relevant content or provenance")
	}
	optIn := requestContext(t, h, project.ID, "&include_durable=true", defaultContextBytes)
	if !optIn.IncludeDurable || optIn.Truncated || len(optIn.Knowledge) != 1 || optIn.Knowledge[0].ID != knowledge.ID || len(optIn.Principles) != 1 || optIn.Principles[0].ID != principle.ID {
		t.Errorf("durable opt-in did not preserve accepted global/project semantics: %+v", optIn)
	}
	for _, suffix := range []string{"&max_bytes=2048", "&max_bytes=65536"} {
		budget, _ := strconv.Atoi(strings.TrimPrefix(suffix, "&max_bytes="))
		bounded := requestContext(t, h, project.ID, suffix, budget)
		if bounded.Project.ID != project.ID {
			t.Error("valid budget lost project identity")
		}
	}
}

func TestContextRejectsInvalidBudgetsAndOptions(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Budget validation"})
	for _, query := range []string{
		"max_bytes=", "max_bytes=0", "max_bytes=-1", "max_bytes=2047", "max_bytes=65537",
		"max_bytes=3.5", "max_bytes=text", "max_bytes=9999999999999999999999999",
		"max_bytes=2048&max_bytes=4096", "include_durable=1", "include_durable=",
		"include_durable=true&include_durable=false",
	} {
		t.Run(query, func(t *testing.T) {
			response := apiTestRequest(h, http.MethodGet, "/v1/context?project_id="+project.ID+"&"+query, "", "", "Bearer "+apiTestToken, "")
			assertAPIError(t, response, http.StatusBadRequest)
		})
	}
}

func TestContextByteBudgetIncludesUnicodeEscapingAndHugeMetadata(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	links := make([]string, 0, 100)
	for index := range 100 {
		record := createContextRecord(t, s, store.CreateInput{Kind: "note", Title: fmt.Sprintf("Reference %03d", index)})
		links = append(links, record.ID)
	}
	sources := make([]string, 50)
	for index := range sources {
		sources[index] = fmt.Sprintf("%02d%s", index, strings.Repeat("<&", 1023))
	}
	tags := make([]string, 32)
	for index := range tags {
		tags[index] = fmt.Sprintf("%02d%s", index, strings.Repeat("x", 62))
	}
	body := strings.Repeat("🧠<&>\u2028\t", 5000)
	project, _, err := s.Create(context.Background(), store.Author{Name: strings.Repeat(">", 200), Role: store.RoleReviewer}, "", store.CreateInput{
		Kind: "project", Title: strings.Repeat("<&界", 100), Body: body,
		Owner: strings.Repeat("<", 200), Sources: sources, Links: links, Tags: tags, Priority: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	originals := map[string]store.Record{project.ID: project}
	for _, kind := range []string{"task", "feedback", "note", "knowledge", "principle"} {
		input := store.CreateInput{Kind: kind, Title: strings.Repeat("\"界🧠", 100), Body: body, ProjectID: project.ID, Sources: sources, Links: links, Owner: strings.Repeat("&", 200), Priority: 5}
		if kind == "knowledge" || kind == "principle" {
			input.Status = "accepted"
		}
		record := createContextRecord(t, s, input)
		originals[record.ID] = record
	}
	for _, budget := range []int{2048, 2051, 4096, defaultContextBytes, 65536} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			packet := requestContext(t, h, project.ID, "&include_durable=true&max_bytes="+strconv.Itoa(budget), budget)
			if packet.Project.ID != project.ID || packet.Project.Kind != "project" || packet.Project.Status != project.Status || packet.Project.Version != project.Version || !packet.Truncated || !packet.Project.BodyTruncated {
				t.Fatalf("oversized project lost identity or clipping metadata: %+v", packet.Project)
			}
			all := []contextRecord{packet.Project}
			for _, group := range [][]contextRecord{packet.Tasks, packet.Feedback, packet.Notes, packet.Knowledge, packet.Principles} {
				all = append(all, group...)
			}
			for _, preview := range all {
				assertContextPreview(t, originals[preview.ID], preview)
			}
			for name, counts := range map[string][2]int{
				"tasks": {len(packet.Tasks), packet.Omitted.Tasks}, "feedback": {len(packet.Feedback), packet.Omitted.Feedback},
				"notes": {len(packet.Notes), packet.Omitted.Notes}, "knowledge": {len(packet.Knowledge), packet.Omitted.Knowledge},
				"principles": {len(packet.Principles), packet.Omitted.Principles},
			} {
				if counts[0]+counts[1] != 1 {
					t.Errorf("%s omission count does not describe the full eligible set: %v", name, counts)
				}
			}
		})
	}
	stored, err := s.Get(context.Background(), project.ID)
	if err != nil || !reflect.DeepEqual(stored, project) {
		t.Error("context preview altered the complete stored project")
	}
}

func assertContextPreview(t *testing.T, original store.Record, preview contextRecord) {
	t.Helper()
	flags := map[string]bool{}
	for _, field := range preview.TruncatedFields {
		flags[field] = true
	}
	if !utf8.ValidString(preview.Title) || !utf8.ValidString(preview.Body) || !strings.HasPrefix(original.Title, preview.Title) || !strings.HasPrefix(original.Body, preview.Body) {
		t.Errorf("context text is not a valid UTF-8 prefix of its record: %+v", preview)
	}
	if (original.Body != preview.Body) != preview.BodyTruncated {
		t.Errorf("body clipping is not accurately signaled for %s", original.ID)
	}
	for field, changed := range map[string]bool{
		"title": original.Title != preview.Title, "owner": original.Owner != preview.Owner,
		"updated_by": original.UpdatedBy != preview.UpdatedBy, "project_id": original.ProjectID != preview.ProjectID,
		"priority": original.Priority != preview.Priority, "updated_at": preview.UpdatedAt == nil,
		"sources": len(original.Sources) != len(preview.Sources), "links": len(original.Links) != len(preview.Links),
		"reviewed_by": original.ReviewedBy != preview.ReviewedBy,
		"reviewed_at": (original.ReviewedAt == nil) != (preview.ReviewedAt == nil),
	} {
		if changed && !flags[field] {
			t.Errorf("context silently changed %s for %s", field, original.ID)
		}
	}
	for index, source := range preview.Sources {
		if source != original.Sources[index] {
			t.Error("context shortened or invented a source reference")
		}
	}
	for index, link := range preview.Links {
		if link != original.Links[index] {
			t.Error("context shortened or invented a record link")
		}
	}
}

func TestContextPrioritizesCoordinationThenRecentNotesOverDurable(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Many handoffs"})
	task := createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "Current intention", ProjectID: project.ID, Priority: 0})
	feedback := createContextRecord(t, s, store.CreateInput{Kind: "feedback", Title: "Current unresolved feedback", ProjectID: project.ID, Priority: 0})
	for index := range 105 {
		createContextRecord(t, s, store.CreateInput{Kind: "note", Title: fmt.Sprintf("Old handoff %03d", index), ProjectID: project.ID, Priority: 5, Body: strings.Repeat("old ", 100)})
	}
	latest := createContextRecord(t, s, store.CreateInput{Kind: "note", Title: "Newest handoff has lower priority", ProjectID: project.ID, Priority: 0, Body: strings.Repeat("recent ", 100)})
	createContextRecord(t, s, store.CreateInput{Kind: "knowledge", Title: "Durable fact with high priority", Status: "accepted", Sources: []string{"https://example.com/review"}, Priority: 5, Body: strings.Repeat("durable ", 100)})
	packet := requestContext(t, h, project.ID, "&max_bytes=4096&include_durable=true", 4096)
	if len(packet.Tasks) != 1 || packet.Tasks[0].ID != task.ID || len(packet.Feedback) != 1 || packet.Feedback[0].ID != feedback.ID {
		t.Error("high-priority notes or durable knowledge crowded out session intentions and feedback")
	}
	if len(packet.Notes) == 0 || packet.Notes[0].ID != latest.ID || packet.Omitted.Notes != 106-len(packet.Notes) {
		t.Errorf("context missed the latest handoff or understated omitted notes: notes = %+v; omissions = %+v", packet.Notes, packet.Omitted)
	}
	if len(packet.Knowledge) != 0 || packet.Omitted.Knowledge != 1 || !packet.Truncated {
		t.Errorf("durable data took space from recent handoffs: %+v", packet)
	}
}

func TestConcurrentContextRequestsKeepIndependentBudgets(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Independent reads", Body: strings.Repeat("project ", 1000)})
	for index := range 12 {
		createContextRecord(t, s, store.CreateInput{Kind: "note", Title: fmt.Sprintf("Handoff %02d", index), ProjectID: project.ID, Body: strings.Repeat("🧠<&\n", 1000)})
	}
	type response struct {
		budget int
		value  *httptest.ResponseRecorder
	}
	results := make(chan response, 12)
	var readers sync.WaitGroup
	for range 3 {
		for _, budget := range []int{2048, 4096, defaultContextBytes, 65536} {
			readers.Add(1)
			go func() {
				defer readers.Done()
				value := apiTestRequest(h, http.MethodGet, "/v1/context?project_id="+project.ID+"&max_bytes="+strconv.Itoa(budget), "", "", "Bearer "+apiTestToken, "")
				results <- response{budget, value}
			}()
		}
	}
	readers.Wait()
	close(results)
	for result := range results {
		packet := decodeContextResponse(t, result.value, result.budget)
		if packet.Project.ID != project.ID || !packet.Truncated || packet.Omitted.Notes != 12-len(packet.Notes) {
			t.Errorf("concurrent context requests leaked state or budget: %+v", packet)
		}
	}
}

func TestContextPrinciplesUseAtMostAQuarterAndYieldUnusedSpace(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Principled"})
	for i := 0; i < 20; i++ {
		createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: fmt.Sprintf("Rule %02d", i), Body: strings.Repeat("rule ", 80), Status: "accepted", Sources: []string{"https://example.com/review"}})
	}
	for i := 0; i < 40; i++ {
		createContextRecord(t, s, store.CreateInput{Kind: "task", Title: fmt.Sprintf("Task %02d", i), Body: strings.Repeat("task ", 40), ProjectID: project.ID})
	}
	const budget = 8192
	packet := requestContext(t, h, project.ID, "&max_bytes=8192", budget)
	principleBytes := 0
	for _, p := range packet.Principles {
		data, _ := json.Marshal(p)
		principleBytes += len(data) + 1
	}
	if len(packet.Principles) == 0 || principleBytes > budget/4 || packet.Omitted.Principles != 20-len(packet.Principles) || len(packet.Tasks) == 0 {
		t.Fatalf("principles %d (%d bytes, omitted %d), tasks %d", len(packet.Principles), principleBytes, packet.Omitted.Principles, len(packet.Tasks))
	}
	// With one tiny principle, coordination gets the unused principle share.
	h2, s2 := apiTestHandler(t, nil)
	p2 := createContextRecord(t, s2, store.CreateInput{Kind: "project", Title: "Few rules"})
	createContextRecord(t, s2, store.CreateInput{Kind: "principle", Title: "One rule", Status: "accepted", Sources: []string{"https://example.com/review"}})
	for i := 0; i < 40; i++ {
		createContextRecord(t, s2, store.CreateInput{Kind: "task", Title: fmt.Sprintf("Task %02d", i), Body: strings.Repeat("task ", 40), ProjectID: p2.ID})
	}
	response := apiTestRequest(h2, http.MethodGet, "/v1/context?project_id="+p2.ID+"&max_bytes=8192", "", "", "Bearer "+apiTestToken, "")
	if response.Body.Len() < budget*85/100 {
		t.Errorf("coordination did not use the unused principle share: %d of %d bytes", response.Body.Len(), budget)
	}
}

func TestContextClipsAnOversizedPrinciple(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Big rule"})
	createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Huge rule", Body: strings.Repeat("rule ", 4000), Status: "accepted", Sources: []string{"https://example.com/review"}, ProjectID: project.ID})
	task := createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "Keep working", ProjectID: project.ID})
	packet := requestContext(t, h, project.ID, "&max_bytes=2048", 2048)
	if len(packet.Principles) != 1 || !packet.Principles[0].BodyTruncated || len(packet.Tasks) != 1 || packet.Tasks[0].ID != task.ID {
		t.Fatalf("oversized principle: %+v", packet)
	}
}

func TestContextNeverLoadsUnacceptedPrinciples(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Pending rules"})
	createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Proposed rule", ProjectID: project.ID})
	for _, status := range []string{"rejected", "superseded"} {
		createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: status + " rule", Status: status, ProjectID: project.ID})
	}
	if packet := requestContext(t, h, project.ID, "", defaultContextBytes); len(packet.Principles) != 0 || packet.Omitted.Principles != 0 {
		t.Fatalf("unaccepted principles loaded: %+v", packet)
	}
}

func TestRecallHintWording(t *testing.T) {
	const want = "Not all reviewed knowledge is loaded here. When a task touches past decisions or conventions, search it with: herma recall \"<words>\""
	if recallHint != want {
		t.Errorf("recallHint = %q, want %q", recallHint, want)
	}
}
