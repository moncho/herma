package api

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/moncho/herma/internal/store"
)

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// loggingHandler returns a handler over a database file, its error log and
// the file's path, so a test can break the database behind the store's back.
func loggingHandler(t *testing.T) (*Handler, *store.Store, *logBuffer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.sqlite")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h := NewHandler(s, []Identity{
		{Name: "authenticated-session", Token: apiTestToken, Role: store.RoleAgent},
		{Name: "owner", Token: reviewerToken, Role: store.RoleReviewer},
	})
	log := &logBuffer{}
	h.SetErrorLog(log)
	return h, s, log, path
}

func dropTable(t *testing.T, path, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE " + table); err != nil {
		t.Fatal(err)
	}
}

func TestInternalErrorsAreLoggedWithTheirCauseAndAnsweredWithoutIt(t *testing.T) {
	h, s, log, path := loggingHandler(t)
	record := createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "A task"})
	dropTable(t, path, "changes")

	response := apiTestRequest(h, http.MethodGet, "/v1/changes?after=0&kind=task", "", "", "Bearer "+apiTestToken, "")
	assertAPIError(t, response, http.StatusInternalServerError)
	if body := response.Body.String(); !strings.Contains(body, `"code":"internal_error"`) || strings.Contains(body, "changes") || strings.Contains(body, "no such table") {
		t.Errorf("the response carries more than the generic error: %s", body)
	}
	line := log.String()
	if !regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} internal error: GET /v1/changes \(authenticated-session\): .*no such table: changes.*\n$`).MatchString(line) {
		t.Errorf("log line: %q", line)
	}
	// The query string can hold search words and is left out.
	if strings.Contains(line, "kind=task") || strings.Contains(line, apiTestToken) {
		t.Errorf("the log line carries the query string or the token: %q", line)
	}

	// The MCP endpoint answers and logs the same way, naming the tool. A
	// write fails here because it can no longer be numbered.
	before := len(log.String())
	result := rpc(t, h, apiTestToken, "tools/call", map[string]any{"name": "update", "arguments": map[string]any{"id": record.ID, "version": 1, "title": "Renamed"}})
	if result.Error == nil || result.Error.Message != "internal error" {
		t.Fatalf("MCP answer: %+v, want the generic internal error", result.Error)
	}
	if mcpLine := log.String()[before:]; !regexp.MustCompile(`internal error: POST /mcp \(authenticated-session\): tool update: .*no such table: changes`).MatchString(mcpLine) || strings.Count(mcpLine, "\n") != 1 {
		t.Errorf("MCP log line: %q", mcpLine)
	}
}

func TestRequestsTheCallerGotWrongOrAbandonedAreNotLogged(t *testing.T) {
	h, s, log, _ := loggingHandler(t)
	record := createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "A task"})
	missing := "rec_" + strings.Repeat("0", 32)
	for _, request := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/v1/records/" + missing, "", http.StatusNotFound},
		{http.MethodGet, "/v1/recall?q=", "", http.StatusBadRequest},
		{http.MethodPost, "/v1/records", `{"kind":"task","title":""}`, http.StatusBadRequest},
		{http.MethodPatch, "/v1/records/" + record.ID, `{"version":9,"title":"x"}`, http.StatusConflict},
		{http.MethodPost, "/v1/records", `{"kind":"knowledge","title":"k","body":"b","status":"accepted","sources":["https://example.com/a"]}`, http.StatusForbidden},
	} {
		assertAPIError(t, apiTestRequest(h, request.method, request.path, request.body, "application/json", "Bearer "+apiTestToken, ""), request.status)
	}
	if result := callTool(t, h, apiTestToken, "get", map[string]any{"id": missing}); !result.IsError {
		t.Fatalf("MCP get of a missing record: %+v", result)
	}
	if unknown := rpc(t, h, apiTestToken, "no/such/method", nil); unknown.Error == nil {
		t.Fatal("an unknown MCP method was answered")
	}
	// A caller that has gone away is not a server fault.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gone := httptest.NewRequest(http.MethodGet, "/v1/records/"+record.ID, nil).WithContext(ctx)
	gone.Header.Set("Authorization", "Bearer "+apiTestToken)
	h.ServeHTTP(httptest.NewRecorder(), gone)
	if log.String() != "" {
		t.Errorf("logged a request that was not a server fault:\n%s", log.String())
	}
}

func TestAUsageCountThatFailsIsLoggedAndTheReadStillAnswers(t *testing.T) {
	h, s, log, path := loggingHandler(t)
	record := createContextRecord(t, s, store.CreateInput{Kind: "knowledge", Title: "Retry budget", Body: "b", Status: "accepted", Sources: []string{"https://example.com/review"}})
	dropTable(t, path, "usage")

	if response := apiTestRequest(h, http.MethodGet, "/v1/records/"+record.ID, "", "", "Bearer "+apiTestToken, ""); response.Code != http.StatusOK {
		t.Fatalf("get: %d %s", response.Code, response.Body.String())
	}
	if response := apiTestRequest(h, http.MethodGet, "/v1/recall?q=retry", "", "", "Bearer "+apiTestToken, ""); response.Code != http.StatusOK {
		t.Fatalf("recall: %d %s", response.Code, response.Body.String())
	}
	lines := strings.Split(strings.TrimRight(log.String(), "\n"), "\n")
	if len(lines) != 2 ||
		!regexp.MustCompile(`usage count not recorded: GET /v1/records/`+record.ID+` \(authenticated-session\): .*no such table: usage`).MatchString(lines[0]) ||
		!regexp.MustCompile(`usage count not recorded: GET /v1/recall \(authenticated-session\): .*no such table: usage`).MatchString(lines[1]) {
		t.Errorf("log lines: %q", lines)
	}
	// The reviewer's reads are not counted, so nothing fails for them.
	before := len(log.String())
	if response := apiTestRequest(h.Socket(), http.MethodGet, "/v1/records/"+record.ID, "", "", "Bearer "+reviewerToken, ""); response.Code != http.StatusOK {
		t.Fatalf("reviewer get: %d", response.Code)
	}
	if extra := log.String()[before:]; extra != "" {
		t.Errorf("a reviewer read logged: %q", extra)
	}
}

// Error text can carry text a caller chose; it must not be able to start a
// log line of its own.
func TestLoggedTextStaysOnOneLine(t *testing.T) {
	h, _, log, _ := loggingHandler(t)
	h.logFailure(context.Background(), "internal error", errorString("first\n2026/01/01 00:00:00 internal error: forged"))
	if got := log.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, `first\n2026/01/01`) {
		t.Errorf("log output: %q", got)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
