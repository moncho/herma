package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func usageRequest(t *testing.T, h http.Handler, token, query string) map[string]store.Usage {
	t.Helper()
	response := apiTestRequest(h, http.MethodGet, "/v1/usage"+query, "", "", "Bearer "+token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("usage status %d: %s", response.Code, response.Body.String())
	}
	var result store.UsageResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != len(result.Items) {
		t.Fatalf("usage total %d with %d items", result.Total, len(result.Items))
	}
	byID := map[string]store.Usage{}
	for _, u := range result.Items {
		byID[u.ID] = u
	}
	return byID
}

func acceptedKnowledge(t *testing.T, s *store.Store, title, body string) store.Record {
	t.Helper()
	return createContextRecord(t, s, store.CreateInput{Kind: "knowledge", Title: title, Body: body, Status: "accepted", Sources: []string{"https://example.com/review"}})
}

func TestUsageCountsAgentRecallsAndFetches(t *testing.T) {
	h, s := roleTestHandler(t)
	hit := acceptedKnowledge(t, s, "Retry budget", "body")
	miss := acceptedKnowledge(t, s, "Unrelated", "body")
	recallRequest(t, h, apiTestToken, url.Values{"q": {"retry"}}, defaultRecallBytes)
	recallRequest(t, h, readerToken, url.Values{"q": {"retry"}}, defaultRecallBytes)
	for _, token := range []string{apiTestToken, readerToken, apiTestToken} {
		if response := apiTestRequest(h, http.MethodGet, "/v1/records/"+hit.ID, "", "", "Bearer "+token, ""); response.Code != http.StatusOK {
			t.Fatalf("get: %d %s", response.Code, response.Body.String())
		}
	}
	usage := usageRequest(t, h, apiTestToken, "")
	if got := usage[hit.ID]; got.Recalled != 2 || got.Fetched != 3 || got.LastUsedAt == nil || got.Title != "Retry budget" {
		t.Errorf("recalled and fetched record: %+v, want recalled 2 fetched 3", got)
	}
	if got, ok := usage[miss.ID]; !ok || got.Recalled != 0 || got.Fetched != 0 || got.LastUsedAt != nil {
		t.Errorf("record no one asked for: %+v (listed %t)", got, ok)
	}
}

// The reviewer reads every record while judging it; counting those reads
// would make each record look used.
func TestUsageIgnoresReviewerReads(t *testing.T) {
	h, s := roleTestHandler(t)
	r := acceptedKnowledge(t, s, "Retry budget", "body")
	recallRequest(t, h.Socket(), reviewerToken, url.Values{"q": {"retry"}}, defaultRecallBytes)
	if response := apiTestRequest(h.Socket(), http.MethodGet, "/v1/records/"+r.ID, "", "", "Bearer "+reviewerToken, ""); response.Code != http.StatusOK {
		t.Fatalf("reviewer get: %d %s", response.Code, response.Body.String())
	}
	if got := usageRequest(t, h.Socket(), reviewerToken, "")[r.ID]; got.Recalled != 0 || got.Fetched != 0 {
		t.Errorf("reviewer reads were counted: %+v", got)
	}
}

// A record matched by the query but left out of the response for lack of
// room was not given to the agent, so it is not counted.
func TestUsageCountsOnlyRecallResultsThatFit(t *testing.T) {
	h, s := roleTestHandler(t)
	for i := 0; i < 30; i++ {
		acceptedKnowledge(t, s, fmt.Sprintf("Retry note %02d", i), strings.Repeat("retry words ", 200))
	}
	packet := recallRequest(t, h, apiTestToken, url.Values{"q": {"retry"}, "max_bytes": {"2048"}, "limit": {"30"}}, 2048)
	if len(packet.Results) == 0 || packet.Omitted == 0 {
		t.Fatalf("want some results returned and some omitted: %d returned, %d omitted", len(packet.Results), packet.Omitted)
	}
	returned := map[string]bool{}
	for _, result := range packet.Results {
		returned[result.ID] = true
	}
	usage := usageRequest(t, h, apiTestToken, "")
	if len(usage) != 30 {
		t.Fatalf("usage lists %d records, want 30", len(usage))
	}
	for id, got := range usage {
		want := int64(0)
		if returned[id] {
			want = 1
		}
		if got.Recalled != want {
			t.Errorf("%s (returned %t): recalled %d, want %d", got.Title, returned[id], got.Recalled, want)
		}
	}
}

func TestUsageCountsMCPRecallAndGet(t *testing.T) {
	h, s := mcpHandler(t)
	r := acceptedKnowledge(t, s, "Retry budget", "body")
	if result := callTool(t, h, apiTestToken, "recall", map[string]any{"query": "retry"}); result.IsError {
		t.Fatalf("recall: %+v", result)
	}
	if result := callTool(t, h, apiTestToken, "get", map[string]any{"id": r.ID}); result.IsError {
		t.Fatalf("get: %+v", result)
	}
	if result := callTool(t, h, apiTestToken, "get", map[string]any{"id": "rec_" + strings.Repeat("0", 32)}); !result.IsError {
		t.Fatalf("get of an unknown record: %+v", result)
	}
	if got := usageRequest(t, h, apiTestToken, "")[r.ID]; got.Recalled != 1 || got.Fetched != 1 {
		t.Errorf("after MCP recall and get: %+v, want recalled 1 fetched 1", got)
	}
}

func TestUsageRequestValidation(t *testing.T) {
	h, s := roleTestHandler(t)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Mine"})
	other := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Other"})
	local := createContextRecord(t, s, store.CreateInput{Kind: "knowledge", Title: "Local", ProjectID: project.ID, Status: "accepted", Sources: []string{"https://example.com/review"}})
	foreign := createContextRecord(t, s, store.CreateInput{Kind: "knowledge", Title: "Foreign", ProjectID: other.ID, Status: "accepted", Sources: []string{"https://example.com/review"}})
	scoped := usageRequest(t, h, apiTestToken, "?project_id="+project.ID)
	if _, ok := scoped[local.ID]; !ok {
		t.Error("project filter dropped the project's record")
	}
	if _, ok := scoped[foreign.ID]; ok {
		t.Error("project filter kept another project's record")
	}
	limited := apiTestRequest(h, http.MethodGet, "/v1/usage?limit=1", "", "", "Bearer "+apiTestToken, "")
	var page store.UsageResult
	if err := json.Unmarshal(limited.Body.Bytes(), &page); err != nil || limited.Code != http.StatusOK || len(page.Items) != 1 || page.Total != 2 {
		t.Errorf("limit=1: status %d, %d items of %d (%v)", limited.Code, len(page.Items), page.Total, err)
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?limit=many", "?unknown=1", "?project_id=" + strings.Repeat("x", 101)} {
		assertAPIError(t, apiTestRequest(h, http.MethodGet, "/v1/usage"+query, "", "", "Bearer "+apiTestToken, ""), http.StatusBadRequest)
	}
	assertAPIError(t, apiTestRequest(h, http.MethodPost, "/v1/usage", "{}", "application/json", "Bearer "+apiTestToken, ""), http.StatusMethodNotAllowed)
	assertAPIError(t, apiTestRequest(h, http.MethodGet, "/v1/usage", "", "", "", ""), http.StatusUnauthorized)
}
