package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

const bookmarkJSON = `{"schema":{"url":{"type":"url","required":true,"unique":true},"rating":{"type":"integer","min":1,"max":5}},"statuses":["unread","done"],"policy":{"recall":true}}`

func bookmarkDefinition(t *testing.T) map[string]any {
	t.Helper()
	var def map[string]any
	if err := json.Unmarshal([]byte(bookmarkJSON), &def); err != nil {
		t.Fatal(err)
	}
	return def
}

func TestKindLifecycleOverHTTP(t *testing.T) {
	h, _ := roleTestHandler(t)
	agentAuth, reviewerAuth := "Bearer "+apiTestToken, "Bearer "+reviewerToken
	created := apiTestRequest(h, http.MethodPost, "/v1/records", `{"kind":"kind","title":"bookmark","fields":{"definition":`+bookmarkJSON+`}}`, "application/json", agentAuth, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("propose: %d %s", created.Code, created.Body.String())
	}
	var kind store.Record
	_ = json.Unmarshal(created.Body.Bytes(), &kind)
	path := "/v1/records/" + kind.ID
	assertAPIError(t, apiTestRequest(h, http.MethodPatch, path, `{"version":1,"status":"accepted"}`, "application/json", agentAuth, ""), http.StatusForbidden)
	if r := apiTestRequest(h.Socket(), http.MethodPatch, path, `{"version":1,"status":"accepted"}`, "application/json", reviewerAuth, ""); r.Code != http.StatusOK {
		t.Fatalf("accept: %d %s", r.Code, r.Body.String())
	}
	bad := apiTestRequest(h, http.MethodPost, "/v1/records", `{"kind":"bookmark","title":"x","fields":{"url":"https://a.example","rating":9}}`, "application/json", agentAuth, "")
	assertAPIError(t, bad, http.StatusBadRequest)
	if !strings.Contains(bad.Body.String(), "fields.rating: must be an integer from 1 to 5") {
		t.Fatalf("validation message: %s", bad.Body.String())
	}
	first := apiTestRequest(h, http.MethodPost, "/v1/records", `{"kind":"bookmark","title":"x","fields":{"url":"https://a.example"}}`, "application/json", agentAuth, "")
	var firstRecord store.Record
	_ = json.Unmarshal(first.Body.Bytes(), &firstRecord)
	dup := apiTestRequest(h, http.MethodPost, "/v1/records", `{"kind":"bookmark","title":"y","fields":{"url":"https://a.example"}}`, "application/json", agentAuth, "")
	assertAPIError(t, dup, http.StatusConflict)
	var envelope struct {
		Error struct {
			Code       string `json:"code"`
			Field      string `json:"field"`
			ExistingID string `json:"existing_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(dup.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "duplicate" || envelope.Error.ExistingID != firstRecord.ID {
		t.Fatalf("duplicate envelope: %s", dup.Body.String())
	}
	patched := apiTestRequest(h, http.MethodPatch, "/v1/records/"+firstRecord.ID, `{"version":1,"fields":{"rating":2}}`, "application/json", agentAuth, "")
	if patched.Code != http.StatusOK || !strings.Contains(patched.Body.String(), `"rating":2`) {
		t.Fatalf("patch: %d %s", patched.Code, patched.Body.String())
	}
	removed := apiTestRequest(h, http.MethodPatch, "/v1/records/"+firstRecord.ID, `{"version":2,"fields":{"rating":null}}`, "application/json", agentAuth, "")
	if removed.Code != http.StatusOK || strings.Contains(removed.Body.String(), `"rating"`) {
		t.Fatalf("null removes a field: %d %s", removed.Code, removed.Body.String())
	}
}

func TestSchemaListsLiveKinds(t *testing.T) {
	h, s := roleTestHandler(t)
	if _, _, err := s.Create(t.Context(), store.Author{Name: "owner", Role: store.RoleReviewer}, "", store.CreateInput{Kind: "kind", Title: "bookmark", Status: "accepted", Fields: map[string]any{"definition": bookmarkDefinition(t)}}); err != nil {
		t.Fatal(err)
	}
	response := apiTestRequest(h, http.MethodGet, "/v1/schema", "", "", "Bearer "+apiTestToken, "")
	var doc struct {
		Kinds map[string][]struct {
			Name     string         `json:"name"`
			Statuses []string       `json:"statuses"`
			Fields   map[string]any `json:"fields"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Kinds["builtin"]) != 7 || len(doc.Kinds["accepted"]) != 1 || doc.Kinds["accepted"][0].Name != "bookmark" || doc.Kinds["accepted"][0].Fields["url"] == nil {
		t.Fatalf("schema kinds: %s", response.Body.String())
	}
}

func TestRecallMarksUnreviewedResults(t *testing.T) {
	h, s := roleTestHandler(t)
	owner := store.Author{Name: "owner", Role: store.RoleReviewer}
	if _, _, err := s.Create(t.Context(), owner, "", store.CreateInput{Kind: "kind", Title: "bookmark", Status: "accepted", Fields: map[string]any{"definition": bookmarkDefinition(t)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(t.Context(), owner, "", store.CreateInput{Kind: "bookmark", Title: "Zebra stripes", Fields: map[string]any{"url": "https://z.example"}}); err != nil {
		t.Fatal(err)
	}
	response := apiTestRequest(h, http.MethodGet, "/v1/recall?q=zebra", "", "", "Bearer "+apiTestToken, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"reviewed":false`) {
		t.Fatalf("recall: %d %s", response.Code, response.Body.String())
	}
}
