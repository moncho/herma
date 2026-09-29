package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

const apiTestToken = "api-test-bearer-secret"

func apiTestHandler(t *testing.T, identities map[string]string) (http.Handler, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "api.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if identities == nil {
		identities = map[string]string{"authenticated-session": apiTestToken}
	}
	return NewHandler(s, identities), s
}

func apiTestRequest(h http.Handler, method, path, body, contentType, authorization, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func assertAPIError(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, status, response.Body.String())
	}
	var problem struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("error is not JSON: %v", err)
	}
	if problem.Error.Code == "" || problem.Error.Message == "" {
		t.Errorf("response does not contain a structured error: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), apiTestToken) {
		t.Error("error response contains an authentication credential")
	}
}

func TestRejectedCreateBodiesLeaveNoRecords(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		contentType string
		status      int
	}{
		{"empty", "", "application/json", http.StatusBadRequest},
		{"null object", "null", "application/json", http.StatusBadRequest},
		{"array", `[{"kind":"note","title":"one"}]`, "application/json", http.StatusBadRequest},
		{"unfinished object", `{"kind":"note","title":`, "application/json", http.StatusBadRequest},
		{"second object", `{"kind":"note","title":"one"} {"kind":"note","title":"two"}`, "application/json", http.StatusBadRequest},
		{"trailing garbage", `{"kind":"note","title":"one"} garbage`, "application/json", http.StatusBadRequest},
		{"unknown field", `{"kind":"note","title":"one","typo":true}`, "application/json", http.StatusBadRequest},
		{"forged creator", `{"kind":"note","title":"one","created_by":"someone-else"}`, "application/json", http.StatusBadRequest},
		{"forged updater", `{"kind":"note","title":"one","updated_by":"someone-else"}`, "application/json", http.StatusBadRequest},
		{"forged version", `{"kind":"note","title":"one","version":200}`, "application/json", http.StatusBadRequest},
		{"duplicate field", `{"kind":"note","title":"one","title":"two"}`, "application/json", http.StatusBadRequest},
		{"escaped duplicate field", `{"kind":"note","title":"one","titl\u0065":"two"}`, "application/json", http.StatusBadRequest},
		{"case duplicate field", `{"kind":"note","title":"one","Title":"two"}`, "application/json", http.StatusBadRequest},
		{"case alias", `{"kind":"note","Title":"one"}`, "application/json", http.StatusBadRequest},
		{"unicode case duplicate field", `{"kind":"note","title":"one","sources":["original"],"\u017fources":["overwrite"]}`, "application/json", http.StatusBadRequest},
		{"unicode case alias", `{"kind":"note","title":"one","\u017ftatus":"published"}`, "application/json", http.StatusBadRequest},
		{"null body", `{"kind":"note","title":"one","body":null}`, "application/json", http.StatusBadRequest},
		{"null tags", `{"kind":"note","title":"one","tags":null}`, "application/json", http.StatusBadRequest},
		{"wrong field type", `{"kind":"note","title":"one","priority":"high"}`, "application/json", http.StatusBadRequest},
		{"invalid UTF-8", `{"kind":"note","title":"one","body":"` + string([]byte{0xff}) + `"}`, "application/json", http.StatusBadRequest},
		{"wrong content type", `{"kind":"note","title":"one"}`, "text/plain", http.StatusUnsupportedMediaType},
		{"missing content type", `{"kind":"note","title":"one"}`, "", http.StatusUnsupportedMediaType},
		{"oversized body", `{"kind":"note","title":"one","body":"` + strings.Repeat("x", maxBody) + `"}`, "application/json", http.StatusRequestEntityTooLarge},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h, s := apiTestHandler(t, nil)
			response := apiTestRequest(h, http.MethodPost, "/v1/records", test.body, test.contentType, "Bearer "+apiTestToken, "rejected-then-corrected")
			assertAPIError(t, response, test.status)
			result, err := s.List(context.Background(), store.ListOptions{Archived: true})
			if err != nil || result.Total != 0 {
				t.Fatalf("rejected request changed the store: total = %d, error = %v", result.Total, err)
			}
			// Validation failures cannot consume the retry key or block a corrected request.
			valid := apiTestRequest(h, http.MethodPost, "/v1/records", `{"kind":"note","title":"Corrected request"}`, "application/json", "Bearer "+apiTestToken, "rejected-then-corrected")
			if valid.Code != http.StatusCreated {
				t.Fatalf("corrected request could not reuse its key: status = %d; body = %s", valid.Code, valid.Body.String())
			}
		})
	}
}

func TestMalformedListQueriesAreRejectedInsteadOfBroadeningFilters(t *testing.T) {
	h, _ := apiTestHandler(t, nil)
	queries := []string{
		"unknown=true", "kind=task&kind=knowledge", "global=yes", "include_archived=1",
		"limit=", "limit=0", "limit=201", "limit=abc", "offset=-1", "offset=abc",
		"kind=unsupported", "kind=task&status=accepted", "global=true&project_id=example",
		"project_id=%ZZ", "kind=task;global=true", "project_id=%ZZ&limit=1",
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			response := apiTestRequest(h, http.MethodGet, "/v1/records?"+query, "", "", "Bearer "+apiTestToken, "")
			assertAPIError(t, response, http.StatusBadRequest)
		})
	}
	for _, path := range []string{"/v1/export?unknown=true", "/v1/export?ignored=%ZZ", "/v1/context", "/v1/context?project_id=x&project_id=y"} {
		t.Run(path, func(t *testing.T) {
			response := apiTestRequest(h, http.MethodGet, path, "", "", "Bearer "+apiTestToken, "")
			assertAPIError(t, response, http.StatusBadRequest)
		})
	}
}

func TestAuthenticationDerivesAttributionFromBearerIdentity(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/v1/records", strings.NewReader(`{"kind":"note","title":"Authenticated authorship"}`))
	r.Header.Set("Authorization", "bEaReR "+apiTestToken)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set("X-Actor", "forged-session")
	r.Header.Set("X-User", "forged-session")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", w.Code, w.Body.String())
	}
	var record store.Record
	if err := json.Unmarshal(w.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.CreatedBy != "authenticated-session" || record.UpdatedBy != "authenticated-session" {
		t.Errorf("record attribution ignored bearer identity: %+v", record)
	}
	history, err := s.History(context.Background(), record.ID)
	if err != nil || len(history) != 1 || history[0].Actor != "authenticated-session" {
		t.Errorf("history attribution ignored bearer identity: %+v, error = %v", history, err)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("private JSON response headers missing: %v", w.Header())
	}
	for _, header := range []string{"", "Basic " + apiTestToken, "Bearer", "Bearer wrong", "Bearer " + apiTestToken + " extra"} {
		response := apiTestRequest(h, http.MethodGet, "/v1/export", "", "", header, "")
		assertAPIError(t, response, http.StatusUnauthorized)
		if response.Header().Get("WWW-Authenticate") == "" {
			t.Error("unauthorized response omits its authentication challenge")
		}
	}
}

func TestInvalidCredentialConfigurationFailsClosed(t *testing.T) {
	for name, identities := range map[string]map[string]string{
		"empty":           {},
		"empty actor":     {"": apiTestToken},
		"empty token":     {"session": ""},
		"duplicate token": {"session-a": apiTestToken, "session-b": apiTestToken},
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := apiTestHandler(t, identities)
			response := apiTestRequest(h, http.MethodGet, "/v1/records", "", "", "Bearer "+apiTestToken, "")
			assertAPIError(t, response, http.StatusUnauthorized)
			health := apiTestRequest(h, http.MethodGet, "/health", "", "", "", "")
			if health.Code != http.StatusOK {
				t.Errorf("public health check status = %d, want 200", health.Code)
			}
		})
	}
}

func TestPatchReplayReturnsOriginalSnapshotWithoutNewRevision(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	created := apiTestRequest(h, http.MethodPost, "/v1/records", `{"kind":"task","title":"Original task"}`, "application/json", "Bearer "+apiTestToken, "create-task")
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var task store.Record
	if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	path := "/v1/records/" + task.ID
	firstBody := `{"version":1,"title":"First update"}`
	first := apiTestRequest(h, http.MethodPatch, path, firstBody, "application/json", "Bearer "+apiTestToken, "first-patch")
	if first.Code != http.StatusOK {
		t.Fatalf("first patch: %d %s", first.Code, first.Body.String())
	}
	second := apiTestRequest(h, http.MethodPatch, path, `{"version":2,"title":"Second update"}`, "application/json", "Bearer "+apiTestToken, "second-patch")
	if second.Code != http.StatusOK {
		t.Fatalf("second patch: %d %s", second.Code, second.Body.String())
	}
	replay := apiTestRequest(h, http.MethodPatch, path, firstBody, "application/json", "Bearer "+apiTestToken, "first-patch")
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Body.String() != first.Body.String() {
		t.Errorf("PATCH retry did not replay the original snapshot: status = %d; body = %s", replay.Code, replay.Body.String())
	}
	conflict := apiTestRequest(h, http.MethodPatch, path, `{"version":3,"title":"Changed key payload"}`, "application/json", "Bearer "+apiTestToken, "first-patch")
	assertAPIError(t, conflict, http.StatusConflict)
	stale := apiTestRequest(h, http.MethodPatch, path, firstBody, "application/json", "Bearer "+apiTestToken, "new-key-stale-version")
	assertAPIError(t, stale, http.StatusConflict)
	for _, body := range []string{
		`{"version":3,"title":"Invalid patch","updated_by":"forged"}`,
		`{"version":3,"title":"Invalid patch","title":"Duplicate"}`,
		`{"version":3,"body":null}`,
		`{"version":3,"version":2,"title":"Duplicate version"}`,
	} {
		invalid := apiTestRequest(h, http.MethodPatch, path, body, "application/json", "Bearer "+apiTestToken, "invalid-patch")
		assertAPIError(t, invalid, http.StatusBadRequest)
	}
	current, err := s.Get(context.Background(), task.ID)
	if err != nil || current.Version != 3 || current.Title != "Second update" {
		t.Errorf("replay or rejected PATCH changed current task: %+v, error = %v", current, err)
	}
	history, err := s.History(context.Background(), task.ID)
	if err != nil || len(history) != 3 {
		t.Errorf("replay or rejected PATCH added revisions: count = %d, error = %v", len(history), err)
	}
}
