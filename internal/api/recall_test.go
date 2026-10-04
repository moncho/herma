package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/moncho/herma/internal/store"
)

func recallRequest(t *testing.T, h http.Handler, token string, query url.Values, budget int) recallPacket {
	t.Helper()
	response := apiTestRequest(h, http.MethodGet, "/v1/recall?"+query.Encode(), "", "", "Bearer "+token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("recall status %d: %s", response.Code, response.Body.String())
	}
	data := response.Body.Bytes()
	if len(data) > budget || !bytes.HasSuffix(data, []byte{'\n'}) || !utf8.Valid(data) || !json.Valid(data) {
		t.Fatalf("invalid recall response: %d bytes (budget %d)", len(data), budget)
	}
	if response.Header().Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Errorf("Content-Length %q, body %d", response.Header().Get("Content-Length"), len(data))
	}
	var packet recallPacket
	if err := json.Unmarshal(data, &packet); err != nil {
		t.Fatal(err)
	}
	if packet.Scope != recallScope || packet.MaxBytes != budget {
		t.Errorf("recall metadata: %+v", packet)
	}
	return packet
}

func TestRecallReturnsRankedResultsWithinBudget(t *testing.T) {
	h, s := roleTestHandler(t)
	for i := 0; i < 30; i++ {
		createContextRecord(t, s, store.CreateInput{Kind: "knowledge", Title: fmt.Sprintf("Retry note %02d", i), Body: strings.Repeat("🧠 retry <&> ", 200), Status: "accepted", Sources: []string{"https://example.com/review"}})
	}
	for _, budget := range []int{2048, 4096, defaultRecallBytes, 65536} {
		packet := recallRequest(t, h, apiTestToken, url.Values{"q": {"retry"}, "max_bytes": {strconv.Itoa(budget)}, "limit": {"30"}}, budget)
		if len(packet.Results) == 0 || packet.Results[0].Rank != 1 {
			t.Fatalf("budget %d: no ranked results: %+v", budget, packet)
		}
		for i, result := range packet.Results {
			if result.Rank != i+1 || result.Status != "accepted" || !result.Reviewed || (result.ReviewedBy == "" && !slices.Contains(result.TruncatedFields, "reviewed_by")) {
				t.Errorf("budget %d result %d: %+v", budget, i, result)
			}
		}
		if packet.Omitted != 30-len(packet.Results) || packet.Truncated != (packet.Omitted > 0 || anyClipped(packet)) {
			t.Errorf("budget %d: omitted %d of 30 with %d results, truncated %t", budget, packet.Omitted, len(packet.Results), packet.Truncated)
		}
	}
	small := recallRequest(t, h, apiTestToken, url.Values{"q": {"retry"}, "max_bytes": {"2048"}, "limit": {"1"}}, 2048)
	if len(small.Results) != 1 || !small.Results[0].BodyTruncated || !small.Truncated {
		t.Errorf("long single result at minimum budget: %+v", small)
	}
}

func anyClipped(p recallPacket) bool {
	for _, r := range p.Results {
		if recordClipped(r.contextRecord) {
			return true
		}
	}
	return false
}

func TestRecallForEveryRoleAndProposedOptIn(t *testing.T) {
	h, s := roleTestHandler(t)
	createContextRecord(t, s, store.CreateInput{Kind: "principle", Title: "Gizmo convention", Status: "accepted", Sources: []string{"https://example.com/review"}})
	createContextRecord(t, s, store.CreateInput{Kind: "knowledge", Title: "Gizmo proposal"})
	for _, test := range []struct {
		handler http.Handler
		token   string
	}{{h, apiTestToken}, {h, readerToken}, {h.Socket(), reviewerToken}} {
		response := apiTestRequest(test.handler, http.MethodGet, "/v1/recall?q=gizmo", "", "", "Bearer "+test.token, "")
		var packet recallPacket
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &packet) != nil || len(packet.Results) != 1 {
			t.Fatalf("role recall: %d %s", response.Code, response.Body.String())
		}
	}
	with := recallRequest(t, h, apiTestToken, url.Values{"q": {"gizmo"}, "include_proposed": {"true"}}, defaultRecallBytes)
	statuses := map[string]bool{}
	for _, r := range with.Results {
		statuses[r.Status] = true
		if r.Status == "proposed" && r.ReviewedBy != "" {
			t.Errorf("proposed result carries review fields: %+v", r)
		}
	}
	if !with.IncludeProposed || !statuses["accepted"] || !statuses["proposed"] {
		t.Errorf("include_proposed: %+v", with)
	}
}

func TestRecallQueryTooLargeForBudgetIsRequestError(t *testing.T) {
	h, _ := roleTestHandler(t)
	query := strings.Repeat("<", 450) + " retry"
	small := apiTestRequest(h, http.MethodGet, "/v1/recall?"+url.Values{"q": {query}, "max_bytes": {"2048"}}.Encode(), "", "", "Bearer "+apiTestToken, "")
	assertAPIError(t, small, http.StatusBadRequest)
	if !strings.Contains(small.Body.String(), "max_bytes") {
		t.Errorf("message does not mention max_bytes: %s", small.Body.String())
	}
	recallRequest(t, h, apiTestToken, url.Values{"q": {query}, "max_bytes": {"65536"}}, 65536)
}

func TestRecallRejectsBadParameters(t *testing.T) {
	h, _ := roleTestHandler(t)
	for _, query := range []string{"", "q=", "q=%21%21", "q=x1&limit=0", "q=x1&limit=101", "q=x1&max_bytes=2047", "q=x1&max_bytes=65537", "q=x1&include_proposed=1", "q=x1&unknown=1", "q=a&q=b", "q=%20%20"} {
		t.Run(query, func(t *testing.T) {
			assertAPIError(t, apiTestRequest(h, http.MethodGet, "/v1/recall?"+query, "", "", "Bearer "+apiTestToken, ""), http.StatusBadRequest)
		})
	}
}
