package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

const (
	reviewerToken = "api-test-reviewer-secret"
	readerToken   = "api-test-reader-secret"
)

func roleTestHandler(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	return apiTestHandler(t, []Identity{
		{Name: "owner", Token: reviewerToken, Role: store.RoleReviewer},
		{Name: "authenticated-session", Token: apiTestToken, Role: store.RoleAgent},
		{Name: "reader", Token: readerToken, Role: store.RoleReadOnly},
	})
}

func TestReadOnlyWritesAreRefusedBeforeReadingBody(t *testing.T) {
	h, _ := roleTestHandler(t)
	oversized := `{"kind":"task","title":"` + strings.Repeat("x", maxBody) + `"}`
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/v1/records"},
		{http.MethodPatch, "/v1/records/rec_" + strings.Repeat("a", 32)},
	} {
		response := apiTestRequest(h, request.method, request.path, oversized, "application/json", "Bearer "+readerToken, "")
		assertAPIError(t, response, http.StatusForbidden)
		if !strings.Contains(response.Body.String(), `"code":"forbidden"`) {
			t.Fatalf("%s %s: %s", request.method, request.path, response.Body.String())
		}
	}
	if response := apiTestRequest(h, http.MethodGet, "/v1/records", "", "", "Bearer "+readerToken, ""); response.Code != http.StatusOK {
		t.Fatalf("read-only list: %d %s", response.Code, response.Body.String())
	}
}

func TestReviewerTokenRequiresSocket(t *testing.T) {
	h, _ := roleTestHandler(t)
	overTCP := apiTestRequest(h, http.MethodGet, "/v1/records", "", "", "Bearer "+reviewerToken, "")
	assertAPIError(t, overTCP, http.StatusForbidden)
	if !strings.Contains(overTCP.Body.String(), `"code":"reviewer_requires_socket"`) || !strings.Contains(overTCP.Body.String(), "unset HERMA_TOKEN") {
		t.Fatalf("reviewer over TCP: %s", overTCP.Body.String())
	}
	body := `{"kind":"knowledge","title":"Reviewed fact","status":"accepted","sources":["https://example.com/review"]}`
	created := apiTestRequest(h.Socket(), http.MethodPost, "/v1/records", body, "application/json", "Bearer "+reviewerToken, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("reviewer over socket: %d %s", created.Code, created.Body.String())
	}
	var record store.Record
	if err := json.Unmarshal(created.Body.Bytes(), &record); err != nil || record.ReviewedBy != "owner" || record.ReviewedAt == nil {
		t.Fatalf("reviewed record: %+v %v", record, err)
	}
}

func TestAgentForbiddenWriteReturns403AndWritesNothing(t *testing.T) {
	h, s := roleTestHandler(t)
	body := `{"kind":"knowledge","title":"Self-approved","status":"accepted","sources":["https://example.com/review"]}`
	response := apiTestRequest(h, http.MethodPost, "/v1/records", body, "application/json", "Bearer "+apiTestToken, "retry-key")
	assertAPIError(t, response, http.StatusForbidden)
	if !strings.Contains(response.Body.String(), "only a reviewer") {
		t.Fatalf("forbidden message: %s", response.Body.String())
	}
	result, err := s.List(t.Context(), store.ListOptions{Archived: true})
	if err != nil || result.Total != 0 {
		t.Fatalf("forbidden write stored records: %+v %v", result, err)
	}
}

func TestReviewFieldsCannotBeSetByClients(t *testing.T) {
	h, _ := roleTestHandler(t)
	for _, field := range []string{`"reviewed_by":"owner"`, `"reviewed_at":"2026-01-01T00:00:00Z"`} {
		body := `{"kind":"knowledge","title":"Forged review",` + field + `}`
		assertAPIError(t, apiTestRequest(h, http.MethodPost, "/v1/records", body, "application/json", "Bearer "+apiTestToken, ""), http.StatusBadRequest)
	}
}

func TestWhoamiReportsIdentityRoleAndListener(t *testing.T) {
	h, _ := roleTestHandler(t)
	for _, test := range []struct {
		handler  http.Handler
		token    string
		identity string
		role     string
		listener string
	}{
		{h, apiTestToken, "authenticated-session", "agent", "tcp"},
		{h, readerToken, "reader", "read-only", "tcp"},
		{h.Socket(), reviewerToken, "owner", "reviewer", "socket"},
	} {
		response := apiTestRequest(test.handler, http.MethodGet, "/v1/whoami", "", "", "Bearer "+test.token, "")
		var got map[string]string
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil {
			t.Fatalf("whoami: %d %s", response.Code, response.Body.String())
		}
		if got["identity"] != test.identity || got["role"] != test.role || got["listener"] != test.listener {
			t.Fatalf("whoami = %v, want %s/%s/%s", got, test.identity, test.role, test.listener)
		}
	}
}

func TestSetIdentitiesRevokesAndRefusesInvalidSets(t *testing.T) {
	h, _ := roleTestHandler(t)
	if err := h.SetIdentities([]Identity{{Name: "owner", Token: reviewerToken, Role: store.RoleReviewer}}); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, apiTestRequest(h, http.MethodGet, "/v1/records", "", "", "Bearer "+apiTestToken, ""), http.StatusUnauthorized)
	invalid := []Identity{
		{Name: "a", Token: apiTestToken, Role: store.RoleAgent},
		{Name: "b", Token: apiTestToken, Role: store.RoleAgent},
	}
	if err := h.SetIdentities(invalid); err == nil {
		t.Fatal("accepted duplicate tokens")
	}
	if response := apiTestRequest(h.Socket(), http.MethodGet, "/v1/records", "", "", "Bearer "+reviewerToken, ""); response.Code != http.StatusOK {
		t.Fatalf("invalid set replaced the previous one: %d", response.Code)
	}
}
