package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func acceptTestKind(t *testing.T, s *store.Store, name, definition, project string) {
	t.Helper()
	var def map[string]any
	if err := json.Unmarshal([]byte(definition), &def); err != nil {
		t.Fatal(err)
	}
	createContextRecord(t, s, store.CreateInput{Kind: "kind", Title: name, ProjectID: project, Status: "accepted", Fields: map[string]any{"definition": def}})
}

func TestContextAddsCustomSectionsAfterBuiltins(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "P"})
	other := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Other"})
	acceptTestKind(t, s, "experiment_run", `{"schema":{},"statuses":["running","done"],"policy":{"context":{"statuses":["running"],"order":"recent","max_records":2}}}`, project.ID)
	acceptTestKind(t, s, "bookmark", `{"statuses":["unread"],"policy":{}}`, "")
	acceptTestKind(t, s, "elsewhere", `{"statuses":["a"],"policy":{}}`, other.ID)
	createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "T", ProjectID: project.ID})
	for i := range 3 {
		createContextRecord(t, s, store.CreateInput{Kind: "experiment_run", Title: fmt.Sprintf("run %d", i), ProjectID: project.ID})
	}
	createContextRecord(t, s, store.CreateInput{Kind: "experiment_run", Title: "finished", ProjectID: project.ID, Status: "done"})
	packet := requestContext(t, h, project.ID, "", defaultContextBytes)
	if strings.Join(packet.Kinds, ",") != "bookmark,experiment_run" {
		t.Fatalf("kinds = %v", packet.Kinds)
	}
	var order []string
	for _, section := range packet.Sections {
		order = append(order, section.Kind)
	}
	if strings.Join(order, ",") != "task,feedback,note,experiment_run" {
		t.Fatalf("section order = %v", order)
	}
	runs := packet.section("experiment_run")
	if len(runs) != 2 || runs[0].Title != "run 2" || packet.omittedFor("experiment_run") != 1 || !packet.Truncated {
		t.Fatalf("runs = %+v omitted %d", runs, packet.omittedFor("experiment_run"))
	}
	text := apiTestRequest(h, http.MethodGet, "/v1/context?format=text&project_id="+project.ID, "", "", "Bearer "+apiTestToken, "").Body.String()
	if !strings.Contains(text, "\nCustom kinds: bookmark, experiment_run (herma schema for fields)\n") || !strings.Contains(text, "\n## experiment_run\n") || !strings.Contains(text, "Omitted: experiment_run 1.") {
		t.Fatalf("text context:\n%s", text)
	}
}

func TestKindsLineCapsAtTwenty(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "P"})
	for i := range 23 {
		acceptTestKind(t, s, fmt.Sprintf("kind_%02d", i), `{"statuses":["a"],"policy":{}}`, "")
	}
	for _, budget := range []int{minContextBytes, defaultContextBytes} {
		response := apiTestRequest(h, http.MethodGet, fmt.Sprintf("/v1/context?format=text&max_bytes=%d&project_id=%s", budget, project.ID), "", "", "Bearer "+apiTestToken, "")
		if response.Code != http.StatusOK || response.Body.Len() > budget || !strings.Contains(response.Body.String(), "kind_19 +3 more (herma schema for fields)") || strings.Contains(response.Body.String(), "kind_20") {
			t.Fatalf("budget %d: %d %s", budget, response.Code, response.Body.String())
		}
	}
}

func TestKindsLineOmittedWithoutCustomKinds(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "P"})
	text := apiTestRequest(h, http.MethodGet, "/v1/context?format=text&project_id="+project.ID, "", "", "Bearer "+apiTestToken, "").Body.String()
	if strings.Contains(text, "Custom kinds") {
		t.Fatalf("text: %s", text)
	}
}

func TestContextFitsBudgetWithManyCustomKinds(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "P"})
	const kinds = 40
	for i := range kinds {
		name := fmt.Sprintf("long_custom_kind_name_number_%02d", i)
		acceptTestKind(t, s, name, `{"statuses":["open"],"policy":{"context":{"statuses":["open"],"order":"recent","max_records":5}}}`, "")
		createContextRecord(t, s, store.CreateInput{Kind: name, Title: "record of " + name, ProjectID: project.ID, Body: strings.Repeat("detail ", 20)})
	}
	for _, budget := range []int{minContextBytes, defaultContextBytes} {
		packet := requestContext(t, h, project.ID, fmt.Sprintf("&max_bytes=%d", budget), budget)
		if len(packet.Kinds) != maxKindsInLine || packet.KindsMore != kinds-maxKindsInLine {
			t.Fatalf("budget %d: kinds %d, more %d", budget, len(packet.Kinds), packet.KindsMore)
		}
		var builtins []string
		placed, omitted := 0, packet.CustomOmitted
		for _, section := range packet.Sections {
			switch section.Kind {
			case "task", "feedback", "note", "knowledge":
				builtins = append(builtins, section.Kind)
				continue
			}
			if len(section.Records) == 0 {
				t.Fatalf("budget %d: empty custom section %s listed", budget, section.Kind)
			}
			placed += len(section.Records)
			omitted += section.Omitted
		}
		if strings.Join(builtins, ",") != "task,feedback,note" || placed+omitted != kinds || !packet.Truncated || !packet.anyOmitted() {
			t.Fatalf("budget %d: builtins %v, placed %d + omitted %d != %d", budget, builtins, placed, omitted, kinds)
		}
		response := apiTestRequest(h, http.MethodGet, fmt.Sprintf("/v1/context?format=text&max_bytes=%d&project_id=%s", budget, project.ID), "", "", "Bearer "+apiTestToken, "")
		text := response.Body.String()
		if response.Code != http.StatusOK || len(text) > budget || (budget == minContextBytes && !strings.Contains(text, "Omitted: custom ")) {
			t.Fatalf("budget %d text: %d %d bytes\n%s", budget, response.Code, len(text), text)
		}
	}
}

func TestContextShortensTheKindsListToFitTheBudget(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: strings.Repeat("P", 300), Body: strings.Repeat("project body ", 100)})
	const kinds = 30
	for i := range kinds {
		acceptTestKind(t, s, fmt.Sprintf("k%02d_", i)+strings.Repeat("x", 36), `{"statuses":["a"],"policy":{}}`, "")
	}
	packet := requestContext(t, h, project.ID, fmt.Sprintf("&max_bytes=%d", minContextBytes), minContextBytes)
	if len(packet.Kinds) == 0 || len(packet.Kinds) >= maxKindsInLine || len(packet.Kinds)+packet.KindsMore != kinds {
		t.Fatalf("kinds %d, more %d", len(packet.Kinds), packet.KindsMore)
	}
	response := apiTestRequest(h, http.MethodGet, fmt.Sprintf("/v1/context?format=text&max_bytes=%d&project_id=%s", minContextBytes, project.ID), "", "", "Bearer "+apiTestToken, "")
	if response.Code != http.StatusOK || response.Body.Len() > minContextBytes || !strings.Contains(response.Body.String(), " more (herma schema for fields)") {
		t.Fatalf("text: %d %s", response.Code, response.Body.String())
	}
}

const readingJSON = `{"schema":{"url":{"type":"url"},"tags":{"type":"string-list"},"rating":{"type":"number"},"read":{"type":"boolean"},"notes":{"type":"text"}},"statuses":["unread"],"policy":{"context":{"statuses":["unread"],"order":"recent","max_records":5}}}`

func TestContextShowsTypedFields(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "P"})
	acceptTestKind(t, s, "reading", readingJSON, "")
	createContextRecord(t, s, store.CreateInput{Kind: "reading", Title: "Paper", Body: "summary", ProjectID: project.ID, Fields: map[string]any{
		"url": "https://paper.example/a", "tags": []any{"ml", "search"}, "rating": 4.5, "read": false, "notes": "first line\n## not a section",
	}})
	packet := requestContext(t, h, project.ID, "", defaultContextBytes)
	records := packet.section("reading")
	if len(records) != 1 || records[0].Fields["url"] != "https://paper.example/a" || records[0].Fields["rating"] != 4.5 {
		t.Fatalf("json fields: %+v", records)
	}
	text := requestTextContext(t, h, project.ID, "", defaultContextBytes)
	want := "    summary\n    notes: first line\n    ## not a section\n    rating: 4.5\n    read: false\n    tags: ml, search\n    url: https://paper.example/a\n"
	if !strings.Contains(text, want) {
		t.Fatalf("text fields missing:\n%s", text)
	}
}

func TestContextDropsFieldsThatDoNotFit(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "P"})
	acceptTestKind(t, s, "reading", readingJSON, "")
	createContextRecord(t, s, store.CreateInput{Kind: "reading", Title: "Paper", Body: "summary", ProjectID: project.ID, Fields: map[string]any{
		"url": "https://paper.example/" + strings.Repeat("a", 1500), "notes": strings.Repeat("long notes ", 200),
	}})
	packet := requestContext(t, h, project.ID, fmt.Sprintf("&max_bytes=%d", minContextBytes), minContextBytes)
	records := packet.section("reading")
	if len(records) != 1 || records[0].Fields != nil || !slices.Contains(records[0].TruncatedFields, "fields") || records[0].Body != "summary" || !packet.Truncated {
		t.Fatalf("fields not dropped whole: %+v", records)
	}
	text := requestTextContext(t, h, project.ID, fmt.Sprintf("&max_bytes=%d", minContextBytes), minContextBytes)
	if strings.Contains(text, "url: ") || !strings.Contains(text, " fields") {
		t.Fatalf("text:\n%s", text)
	}
}

func TestRecallShowsTypedFields(t *testing.T) {
	h, s := roleTestHandler(t)
	owner := store.Author{Name: "owner", Role: store.RoleReviewer}
	if _, _, err := s.Create(t.Context(), owner, "", store.CreateInput{Kind: "kind", Title: "bookmark", Status: "accepted", Fields: map[string]any{"definition": bookmarkDefinition(t)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(t.Context(), owner, "", store.CreateInput{Kind: "bookmark", Title: "Zebra stripes", Fields: map[string]any{"url": "https://z.example/zebra"}}); err != nil {
		t.Fatal(err)
	}
	packet := recallRequest(t, h, apiTestToken, url.Values{"q": {"zebra"}}, defaultRecallBytes)
	if len(packet.Results) != 1 || packet.Results[0].Fields["url"] != "https://z.example/zebra" {
		t.Fatalf("recall fields: %+v", packet.Results)
	}
}

func TestKindsLineCountsKindsWhenNoNameFits(t *testing.T) {
	packet := projectContext{Project: contextRecord{ID: "rec_p", Title: "P"}, KindsMore: 3, Principles: []contextRecord{}, Sections: []contextSection{}}
	data, err := textFormat{}.encode(packet)
	if err != nil || !strings.Contains(string(data), "\nCustom kinds: +3 more (herma schema for fields)\n") {
		t.Fatalf("text: %v %s", err, data)
	}
}
