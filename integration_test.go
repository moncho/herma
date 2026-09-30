package herma_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moncho/herma/internal/api"
	"github.com/moncho/herma/internal/store"
)

const (
	tokenA        = "integration-secret-for-session-a"
	tokenB        = "integration-secret-for-session-b"
	tokenReviewer = "integration-secret-for-the-reviewer"
)

type integrationServer struct {
	server *httptest.Server
	// Serves Handler.Socket over TCP to stand in for the Unix socket listener;
	// the real socket is exercised by the CLI end-to-end test.
	reviewer *httptest.Server
	close    func()
}

func startIntegrationServer(t *testing.T, path string) *integrationServer {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open knowledge store: %v", err)
	}
	handler := api.NewHandler(db, []api.Identity{
		{Name: "session-a", Token: tokenA, Role: store.RoleAgent},
		{Name: "session-b", Token: tokenB, Role: store.RoleAgent},
		{Name: "reviewer", Token: tokenReviewer, Role: store.RoleReviewer},
	})
	server := httptest.NewServer(handler)
	reviewer := httptest.NewServer(handler.Socket())
	var once sync.Once
	result := &integrationServer{server: server, reviewer: reviewer}
	result.close = func() {
		once.Do(func() {
			server.Close()
			reviewer.Close()
			if err := db.Close(); err != nil {
				t.Errorf("close knowledge store: %v", err)
			}
		})
	}
	t.Cleanup(result.close)
	return result
}

func (s *integrationServer) request(method, path, token, key string, input any) (int, []byte, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(encoded)
	}
	target := s.server
	if token == tokenReviewer {
		target = s.reviewer
	}
	req, err := http.NewRequest(method, target.URL+path, body)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := target.Client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return response.StatusCode, data, err
}

func integrationRequest[T any](t *testing.T, s *integrationServer, method, path, token, key string, input any, wantStatus int) T {
	t.Helper()
	status, data, err := s.request(method, path, token, key, input)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if status != wantStatus {
		t.Fatalf("%s %s: status = %d, want %d; body = %s", method, path, status, wantStatus, data)
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode %s %s: %v; body = %s", method, path, err, data)
	}
	return result
}

func createRecord(t *testing.T, s *integrationServer, input store.CreateInput) store.Record {
	t.Helper()
	return integrationRequest[store.Record](t, s, http.MethodPost, "/v1/records", tokenA, "", input, http.StatusCreated)
}

func createReviewed(t *testing.T, s *integrationServer, input store.CreateInput) store.Record {
	t.Helper()
	return integrationRequest[store.Record](t, s, http.MethodPost, "/v1/records", tokenReviewer, "", input, http.StatusCreated)
}

func recordPath(record store.Record) string { return "/v1/records/" + url.PathEscape(record.ID) }

func pointer[T any](value T) *T { return &value }

type contextResponse struct {
	Project     store.Record   `json:"project"`
	Principles  []store.Record `json:"principles"`
	Knowledge   []store.Record `json:"knowledge"`
	Tasks       []store.Record `json:"tasks"`
	Notes       []store.Record `json:"notes"`
	Feedback    []store.Record `json:"feedback"`
	GeneratedAt time.Time      `json:"generated_at"`
	Truncated   bool           `json:"truncated"`
	MaxBytes    int            `json:"max_bytes"`
	Omitted     struct {
		Knowledge int `json:"knowledge"`
	} `json:"omitted"`
}

type historyResponse struct {
	Items []store.Revision `json:"items"`
}

func assertRecordIDs(t *testing.T, records []store.Record, expected ...store.Record) {
	t.Helper()
	actualIDs := make([]string, 0, len(records))
	for _, record := range records {
		actualIDs = append(actualIDs, record.ID)
	}
	expectedIDs := make([]string, 0, len(expected))
	for _, record := range expected {
		expectedIDs = append(expectedIDs, record.ID)
	}
	sort.Strings(actualIDs)
	sort.Strings(expectedIDs)
	if fmt.Sprint(actualIDs) != fmt.Sprint(expectedIDs) {
		t.Errorf("record IDs = %v, want %v", actualIDs, expectedIDs)
	}
}

func TestSharedKnowledgeSurvivesRestartAndConcurrentUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knowledge.sqlite")
	first := startIntegrationServer(t, path)
	project := createRecord(t, first, store.CreateInput{Kind: "project", Title: "Shared project", Body: "Build a durable project memory."})
	decision := createReviewed(t, first, store.CreateInput{
		Kind: "knowledge", Title: "Keep the source of each decision", Body: "Every durable decision includes its original source.",
		ProjectID: project.ID, Status: "accepted", Sources: []string{"https://example.org/decisions/source-attribution"},
	})
	task := createRecord(t, first, store.CreateInput{
		Kind: "task", Title: "Document the import workflow", Body: "Pending work for the next session.",
		ProjectID: project.ID, Owner: "session-a", Sources: []string{"https://example.org/issues/import-workflow"},
	})
	if task.CreatedBy != "session-a" || task.UpdatedBy != "session-a" || task.Version != 1 {
		t.Fatalf("creation did not preserve authenticated actor and first version: %+v", task)
	}
	first.close()

	second := startIntegrationServer(t, path)
	context := integrationRequest[contextResponse](t, second, http.MethodGet, "/v1/context?project_id="+url.QueryEscape(project.ID)+"&include_durable=true", tokenB, "", nil, http.StatusOK)
	if context.Project.ID != project.ID || context.GeneratedAt.IsZero() || context.Truncated {
		t.Errorf("unexpected project context metadata: %+v", context)
	}
	assertRecordIDs(t, context.Knowledge, decision)
	assertRecordIDs(t, context.Tasks, task)

	updated := integrationRequest[store.Record](t, second, http.MethodPatch, recordPath(task), tokenB, "", store.UpdateInput{
		Version: task.Version, Body: pointer("Session B picked up the unfinished work."), Owner: pointer("session-b"),
	}, http.StatusOK)
	if updated.Version != task.Version+1 || updated.CreatedBy != "session-a" || updated.UpdatedBy != "session-b" || updated.Owner != "session-b" {
		t.Fatalf("session B did not safely continue session A's task: %+v", updated)
	}
	history := integrationRequest[historyResponse](t, second, http.MethodGet, recordPath(task)+"/history", tokenB, "", nil, http.StatusOK)
	if len(history.Items) != 2 {
		t.Fatalf("task history has %d revisions, want 2", len(history.Items))
	}
	actors := map[int64]string{1: "session-a", 2: "session-b"}
	for _, revision := range history.Items {
		if revision.Actor != actors[revision.Version] || revision.Record.Version != revision.Version || revision.At.IsZero() {
			t.Errorf("revision does not preserve actor, version, and time: %+v", revision)
		}
		if len(revision.Record.Sources) != 1 || revision.Record.Sources[0] != task.Sources[0] {
			t.Errorf("source link was lost from task revision: %+v", revision)
		}
	}
	decisionHistory := integrationRequest[historyResponse](t, second, http.MethodGet, recordPath(decision)+"/history", tokenB, "", nil, http.StatusOK)
	if len(decisionHistory.Items) != 1 || decisionHistory.Items[0].Actor != "reviewer" || len(decisionHistory.Items[0].Record.Sources) != 1 || decisionHistory.Items[0].Record.Sources[0] != decision.Sources[0] {
		t.Errorf("decision provenance did not survive restart: %+v", decisionHistory)
	}

	type editResult struct {
		status int
		data   []byte
		err    error
	}
	results := make(chan editResult, 2)
	start := make(chan struct{})
	for index, token := range []string{tokenA, tokenB} {
		go func(index int, token string) {
			<-start
			status, data, err := second.request(http.MethodPatch, recordPath(task), token, "", store.UpdateInput{
				Version: updated.Version, Body: pointer(fmt.Sprintf("Competing edit %d", index)),
			})
			results <- editResult{status: status, data: data, err: err}
		}(index, token)
	}
	close(start)
	statuses := map[int]int{}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent edit request: %v", result.err)
		}
		statuses[result.status]++
		if result.status != http.StatusOK && result.status != http.StatusConflict {
			t.Errorf("concurrent edit: status = %d, body = %s", result.status, result.data)
		}
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("same-version edits must have one winner and one conflict, got %v", statuses)
	}
	current := integrationRequest[store.Record](t, second, http.MethodGet, recordPath(task), tokenA, "", nil, http.StatusOK)
	if current.Version != 3 {
		t.Errorf("competing edits produced version %d, want 3", current.Version)
	}
	history = integrationRequest[historyResponse](t, second, http.MethodGet, recordPath(task)+"/history", tokenA, "", nil, http.StatusOK)
	if len(history.Items) != 3 {
		t.Errorf("competing edits created %d revisions, want 3", len(history.Items))
	}
}

func TestIdempotencyPersistsAcrossRestartWithoutDuplicateWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knowledge.sqlite")
	first := startIntegrationServer(t, path)
	input := store.CreateInput{Kind: "note", Title: "Idempotency probe", Body: "A retried request creates one note."}
	created := integrationRequest[store.Record](t, first, http.MethodPost, "/v1/records", tokenA, "retry-one-note", input, http.StatusCreated)
	first.close()
	second := startIntegrationServer(t, path)
	replayed := integrationRequest[store.Record](t, second, http.MethodPost, "/v1/records", tokenA, "retry-one-note", input, http.StatusOK)
	if replayed.ID != created.ID || replayed.Version != created.Version || !replayed.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("retry did not replay the original result: original = %+v; retry = %+v", created, replayed)
	}
	input.Body = "A different request must not reuse the same key."
	integrationRequest[map[string]any](t, second, http.MethodPost, "/v1/records", tokenA, "retry-one-note", input, http.StatusConflict)
	listed := integrationRequest[store.ListResult](t, second, http.MethodGet, "/v1/records?kind=note&q="+url.QueryEscape(created.Title), tokenA, "", nil, http.StatusOK)
	if listed.Total != 1 {
		t.Errorf("retried create produced %d matching notes, want 1", listed.Total)
	}
	assertRecordIDs(t, listed.Items, created)
	history := integrationRequest[historyResponse](t, second, http.MethodGet, recordPath(created)+"/history", tokenA, "", nil, http.StatusOK)
	if len(history.Items) != 1 {
		t.Errorf("retried create produced %d revisions, want 1", len(history.Items))
	}
}

func TestProjectContextIsolationAndPortableExport(t *testing.T) {
	s := startIntegrationServer(t, filepath.Join(t.TempDir(), "knowledge.sqlite"))
	project := createRecord(t, s, store.CreateInput{Kind: "project", Title: "Relevant project"})
	otherProject := createRecord(t, s, store.CreateInput{Kind: "project", Title: "Other project"})
	globalPrinciple := createReviewed(t, s, store.CreateInput{Kind: "principle", Title: "Global accepted principle", Status: "accepted", Sources: []string{"https://example.com/review"}})
	localPrinciple := createReviewed(t, s, store.CreateInput{Kind: "principle", Title: "Local accepted principle", ProjectID: project.ID, Status: "accepted", Sources: []string{"https://example.com/review"}})
	globalKnowledge := createReviewed(t, s, store.CreateInput{Kind: "knowledge", Title: "Global accepted knowledge", Status: "accepted", Sources: []string{"https://example.com/review"}})
	localKnowledge := createReviewed(t, s, store.CreateInput{Kind: "knowledge", Title: "Local accepted knowledge", ProjectID: project.ID, Status: "accepted", Sources: []string{"https://example.org/research"}})
	createRecord(t, s, store.CreateInput{Kind: "knowledge", Title: "Unaccepted hypothesis", ProjectID: project.ID, Status: "proposed"})
	createRecord(t, s, store.CreateInput{Kind: "principle", Title: "Unaccepted principle", ProjectID: project.ID, Status: "proposed"})
	createReviewed(t, s, store.CreateInput{Kind: "knowledge", Title: "Other project's knowledge", ProjectID: otherProject.ID, Status: "accepted", Sources: []string{"https://example.com/review"}})
	archived := createReviewed(t, s, store.CreateInput{Kind: "knowledge", Title: "Archived knowledge", ProjectID: project.ID, Status: "accepted", Sources: []string{"https://example.com/review"}})
	archived = integrationRequest[store.Record](t, s, http.MethodPatch, recordPath(archived), tokenReviewer, "", store.UpdateInput{Version: archived.Version, Archived: pointer(true)}, http.StatusOK)
	task := createRecord(t, s, store.CreateInput{Kind: "task", Title: "Pending local work", ProjectID: project.ID})
	createRecord(t, s, store.CreateInput{Kind: "task", Title: "Completed local work", ProjectID: project.ID, Status: "done"})
	createRecord(t, s, store.CreateInput{Kind: "task", Title: "Other project's work", ProjectID: otherProject.ID})
	note := createRecord(t, s, store.CreateInput{Kind: "note", Title: "Local working note", ProjectID: project.ID})
	createRecord(t, s, store.CreateInput{Kind: "note", Title: "Global working note"})
	createRecord(t, s, store.CreateInput{Kind: "note", Title: "Other project's note", ProjectID: otherProject.ID})
	feedback := createRecord(t, s, store.CreateInput{Kind: "feedback", Title: "Unresolved local feedback", ProjectID: project.ID})
	createRecord(t, s, store.CreateInput{Kind: "feedback", Title: "Resolved local feedback", ProjectID: project.ID, Status: "resolved"})
	createRecord(t, s, store.CreateInput{Kind: "feedback", Title: "Other project's feedback", ProjectID: otherProject.ID})

	context := integrationRequest[contextResponse](t, s, http.MethodGet, "/v1/context?project_id="+url.QueryEscape(project.ID)+"&include_durable=true", tokenB, "", nil, http.StatusOK)
	assertRecordIDs(t, context.Principles, globalPrinciple, localPrinciple)
	assertRecordIDs(t, context.Knowledge, globalKnowledge, localKnowledge)
	assertRecordIDs(t, context.Tasks, task)
	assertRecordIDs(t, context.Notes, note)
	assertRecordIDs(t, context.Feedback, feedback)
	if context.Project.ID != project.ID || context.Truncated || context.GeneratedAt.IsZero() {
		t.Errorf("unexpected context metadata: %+v", context)
	}

	status, data, err := s.request(http.MethodGet, "/v1/export", tokenB, "", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("export: status = %d, error = %v, body = %s", status, err, data)
	}
	for _, token := range []string{tokenA, tokenB, tokenReviewer} {
		if bytes.Contains(data, []byte(token)) {
			t.Error("export contains a bearer credential")
		}
	}
	var exported struct {
		FormatVersion int                         `json:"format_version"`
		ExportedAt    time.Time                   `json:"exported_at"`
		Records       []store.Record              `json:"records"`
		History       map[string][]store.Revision `json:"history"`
	}
	if err := json.Unmarshal(data, &exported); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if exported.FormatVersion != 1 || exported.ExportedAt.IsZero() {
		t.Errorf("export metadata is missing: version = %d, at = %v", exported.FormatVersion, exported.ExportedAt)
	}
	if len(exported.Records) != 19 || len(exported.History) != len(exported.Records) {
		t.Errorf("export omitted records or histories: records = %d, histories = %d", len(exported.Records), len(exported.History))
	}
	var foundArchived bool
	for _, record := range exported.Records {
		if record.ID == archived.ID {
			foundArchived = record.Archived
		}
		if len(exported.History[record.ID]) == 0 {
			t.Errorf("export omitted history for %s", record.ID)
		}
	}
	if !foundArchived || len(exported.History[archived.ID]) != 2 {
		t.Errorf("export omitted archived record or its revisions: %+v", exported.History[archived.ID])
	}
	for _, revision := range exported.History[archived.ID] {
		if revision.Actor != "reviewer" {
			t.Errorf("export revision actor = %q, want reviewer", revision.Actor)
		}
	}
}

func TestAuthenticationProtectsRecordsContextHistoryAndExport(t *testing.T) {
	s := startIntegrationServer(t, filepath.Join(t.TempDir(), "knowledge.sqlite"))
	integrationRequest[map[string]any](t, s, http.MethodGet, "/health", "", "", nil, http.StatusOK)
	project := createRecord(t, s, store.CreateInput{Kind: "project", Title: "Private project"})
	requests := []struct {
		method string
		path   string
		input  any
	}{
		{http.MethodGet, "/v1/records", nil},
		{http.MethodPost, "/v1/records", store.CreateInput{Kind: "note", Title: "Unauthorized write"}},
		{http.MethodGet, recordPath(project), nil},
		{http.MethodPatch, recordPath(project), store.UpdateInput{Version: project.Version, Title: pointer("Unauthorized change")}},
		{http.MethodGet, recordPath(project) + "/history", nil},
		{http.MethodGet, "/v1/context?project_id=" + url.QueryEscape(project.ID), nil},
		{http.MethodGet, "/v1/export", nil},
	}
	for _, token := range []string{"", "invalid-token"} {
		for _, request := range requests {
			t.Run(request.method+" "+request.path+" token="+token, func(t *testing.T) {
				result := integrationRequest[struct {
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}](t, s, request.method, request.path, token, "", request.input, http.StatusUnauthorized)
				if strings.TrimSpace(result.Error.Code) == "" || strings.TrimSpace(result.Error.Message) == "" {
					t.Error("authentication failure did not use the structured error contract")
				}
			})
		}
	}
	current := integrationRequest[store.Record](t, s, http.MethodGet, recordPath(project), tokenA, "", nil, http.StatusOK)
	if current.Title != project.Title || current.Version != project.Version {
		t.Error("unauthorized request changed the private project")
	}
	listed := integrationRequest[store.ListResult](t, s, http.MethodGet, "/v1/records", tokenA, "", nil, http.StatusOK)
	if listed.Total != 1 {
		t.Errorf("unauthorized request created a record: total = %d, want 1", listed.Total)
	}
}

func TestProjectContextReportsByteBudgetOmissions(t *testing.T) {
	s := startIntegrationServer(t, filepath.Join(t.TempDir(), "knowledge.sqlite"))
	project := createRecord(t, s, store.CreateInput{Kind: "project", Title: "Large project"})
	for index := range 101 {
		createReviewed(t, s, store.CreateInput{
			Kind: "knowledge", Title: fmt.Sprintf("Accepted fact %03d", index),
			ProjectID: project.ID, Status: "accepted", Sources: []string{"https://example.com/review"},
		})
	}
	task := createRecord(t, s, store.CreateInput{Kind: "task", Title: "A task still fits in context", ProjectID: project.ID})
	status, data, err := s.request(http.MethodGet, "/v1/context?project_id="+url.QueryEscape(project.ID)+"&include_durable=true&max_bytes=4096", tokenB, "", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("context: status = %d, error = %v, body = %s", status, err, data)
	}
	if len(data) > 4096 {
		t.Fatalf("context exceeded its complete wire budget: %d bytes", len(data))
	}
	var context contextResponse
	if err := json.Unmarshal(data, &context); err != nil {
		t.Fatal(err)
	}
	if len(context.Knowledge) == 0 || len(context.Knowledge) >= 100 || !context.Truncated || context.MaxBytes != 4096 || context.Omitted.Knowledge != 101-len(context.Knowledge) {
		t.Errorf("large context must report its byte budget and omissions: %+v", context)
	}
	assertRecordIDs(t, context.Tasks, task)
}
