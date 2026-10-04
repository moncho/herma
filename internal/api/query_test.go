package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func TestListFiltersAndSortsByFields(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	acceptTestKind(t, s, "bookmark", `{"schema":{"rating":{"type":"integer"},"read_at":{"type":"datetime"}},"statuses":["unread"],"policy":{}}`, "")
	for title, fields := range map[string]map[string]any{
		"low":  {"rating": 2, "read_at": "2026-10-01T09:00:00Z"},
		"mid":  {"rating": 4, "read_at": "2026-10-02T09:00:00Z"},
		"high": {"rating": 5, "read_at": "2026-10-03T09:00:00Z"},
	} {
		createContextRecord(t, s, store.CreateInput{Kind: "bookmark", Title: title, Fields: fields})
	}
	query := url.Values{"kind": {"bookmark"}, "where": {"rating>=4", "read_at<2026-10-03T12:00:00+02:00"}, "sort": {"-rating"}}
	response := apiTestRequest(h, http.MethodGet, "/v1/records?"+query.Encode(), "", "", "Bearer "+apiTestToken, "")
	if response.Code != http.StatusOK {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	var page store.ListResult
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, r := range page.Items {
		titles = append(titles, r.Title)
	}
	if strings.Join(titles, ",") != "high,mid" || page.Total != 2 {
		t.Fatalf("page: %v total %d", titles, page.Total)
	}
	// The encoder escapes < and >, so compare the decoded message.
	for _, c := range []struct{ query, want string }{
		{"where=rating%3E%3D4", "where requires a kind filter"},
		{"kind=bookmark&where=rating%3E%3Dfour", `where "rating>=four": fields.rating: must be an integer`},
		{"kind=bookmark&sort=colour", `sort "colour": unknown field of kind bookmark`},
		{"kind=bookmark&kind=task", `query parameter "kind" must appear once`},
		{"kind=bookmark&sort=-rating&sort=rating", `query parameter "sort" must appear once`},
	} {
		response := apiTestRequest(h, http.MethodGet, "/v1/records?"+c.query, "", "", "Bearer "+apiTestToken, "")
		assertAPIError(t, response, http.StatusBadRequest)
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || !strings.HasPrefix(envelope.Error.Message, c.want) {
			t.Errorf("%s: %s", c.query, response.Body.String())
		}
	}
}

func TestSchemaDescribesWhereAndSort(t *testing.T) {
	h, _ := apiTestHandler(t, nil)
	body := apiTestRequest(h, http.MethodGet, "/v1/schema", "", "", "Bearer "+apiTestToken, "").Body.String()
	for _, want := range []string{`"where"`, `"sort"`, "list_where", "list_sort"} {
		if !strings.Contains(body, want) {
			t.Errorf("schema lacks %s", want)
		}
	}
}
