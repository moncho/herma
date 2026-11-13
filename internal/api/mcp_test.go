package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

const mcpReaderToken = "mcp-reader-secret"

func mcpHandler(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	return apiTestHandler(t, []Identity{
		{Name: "authenticated-session", Token: apiTestToken, Role: store.RoleAgent},
		{Name: "reader", Token: mcpReaderToken, Role: store.RoleReadOnly},
		{Name: "owner", Token: "owner-secret", Role: store.RoleReviewer},
	})
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func mcpPost(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func rpc(t *testing.T, h http.Handler, token, method string, params any) rpcResponse {
	t.Helper()
	message := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		message["params"] = params
	}
	body, _ := json.Marshal(message)
	w := mcpPost(t, h, token, string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", method, w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s: content type %q", method, ct)
	}
	var response rpcResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("%s: %v: %s", method, err, w.Body.String())
	}
	if response.JSONRPC != "2.0" || string(response.ID) != "1" {
		t.Fatalf("%s: envelope %s", method, w.Body.String())
	}
	return response
}

type toolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func callTool(t *testing.T, h http.Handler, token, name string, args map[string]any) toolCallResult {
	t.Helper()
	response := rpc(t, h, token, "tools/call", map[string]any{"name": name, "arguments": args})
	if response.Error != nil {
		t.Fatalf("%s: rpc error %d %s", name, response.Error.Code, response.Error.Message)
	}
	var result toolCallResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("%s: content %+v", name, result)
	}
	return result
}

func TestMCPInitializeNegotiatesVersionAndOffersTools(t *testing.T) {
	h, _ := mcpHandler(t)
	response := rpc(t, h, apiTestToken, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test", "version": "1"}})
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct{} `json:"tools"`
		} `json:"capabilities"`
		ServerInfo   struct{ Name string } `json:"serverInfo"`
		Instructions string                `json:"instructions"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.ProtocolVersion != "2025-06-18" || result.Capabilities.Tools == nil || result.ServerInfo.Name != "herma" || result.Instructions == "" {
		t.Fatalf("%s", response.Result)
	}
	response = rpc(t, h, apiTestToken, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	if !strings.Contains(string(response.Result), `"protocolVersion":"`+mcpDefaultVersion+`"`) {
		t.Fatalf("unknown client version should get the default: %s", response.Result)
	}
	w := mcpPost(t, h, apiTestToken, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("notification: %d %q", w.Code, w.Body.String())
	}
	if response := rpc(t, h, apiTestToken, "ping", nil); string(response.Result) != "{}" {
		t.Fatalf("ping: %s", response.Result)
	}
}

func TestMCPListsItsTools(t *testing.T) {
	h, _ := mcpHandler(t)
	response := rpc(t, h, apiTestToken, "tools/list", nil)
	var result struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" || tool.InputSchema["type"] != "object" {
			t.Fatalf("tool %s lacks a description or object schema", tool.Name)
		}
	}
	if strings.Join(names, ",") != "recall,get,principles,propose,context,create,update" {
		t.Fatalf("tools %v", names)
	}
}

func TestMCPToolsReadAndProposeAsTheIdentity(t *testing.T) {
	h, s := mcpHandler(t)
	owner := store.Author{Name: "owner", Role: store.RoleReviewer}
	accepted, _, err := s.Create(t.Context(), owner, "", store.CreateInput{Kind: "knowledge", Title: "SQLite runs in WAL mode", Body: "Set journal_mode=WAL once.", Status: "accepted", Sources: []string{"https://sqlite.org/wal.html"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(t.Context(), owner, "", store.CreateInput{Kind: "principle", Title: "Keep answers short", Body: "Say less.", Status: "accepted", Sources: []string{"https://example.com/short"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(t.Context(), store.Author{Name: "agent", Role: store.RoleAgent}, "", store.CreateInput{Kind: "knowledge", Title: "WAL mode guess", Body: "unreviewed"}); err != nil {
		t.Fatal(err)
	}

	result := callTool(t, h, apiTestToken, "recall", map[string]any{"query": "WAL mode"})
	if result.IsError || !strings.Contains(result.Content[0].Text, accepted.ID) || strings.Contains(result.Content[0].Text, "WAL mode guess") {
		t.Fatalf("recall: %+v", result)
	}
	result = callTool(t, h, apiTestToken, "recall", map[string]any{"query": "WAL mode", "include_proposed": true})
	if !strings.Contains(result.Content[0].Text, "WAL mode guess") {
		t.Fatalf("recall with proposed: %+v", result)
	}

	result = callTool(t, h, apiTestToken, "get", map[string]any{"id": accepted.ID})
	if result.IsError || !strings.Contains(result.Content[0].Text, "Set journal_mode=WAL once.") {
		t.Fatalf("get: %+v", result)
	}
	result = callTool(t, h, apiTestToken, "get", map[string]any{"id": "rec_" + strings.Repeat("0", 32)})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "not found") {
		t.Fatalf("get missing: %+v", result)
	}

	result = callTool(t, h, apiTestToken, "principles", map[string]any{})
	if result.IsError || !strings.Contains(result.Content[0].Text, "Keep answers short") {
		t.Fatalf("principles: %+v", result)
	}

	result = callTool(t, h, apiTestToken, "propose", map[string]any{"kind": "knowledge", "title": "Backups run hourly", "body": "See the Makefile.", "sources": []string{"Makefile"}})
	if result.IsError {
		t.Fatalf("propose: %+v", result)
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &created); err != nil || created.Status != "proposed" {
		t.Fatalf("propose result %q: %v", result.Content[0].Text, err)
	}
	record, err := s.Get(t.Context(), created.ID)
	if err != nil || record.CreatedBy != "authenticated-session" || record.Kind != "knowledge" || len(record.Sources) != 1 {
		t.Fatalf("%+v %v", record, err)
	}
	result = callTool(t, h, apiTestToken, "propose", map[string]any{"kind": "task", "title": "Not durable"})
	if !result.IsError {
		t.Fatalf("propose should take only knowledge and principle: %+v", result)
	}
}

func TestMCPToolsCoordinateThroughTasksAndNotes(t *testing.T) {
	h, s := mcpHandler(t)
	agent := store.Author{Name: "other-session", Role: store.RoleAgent}
	project, _, err := s.Create(t.Context(), agent, "", store.CreateInput{Kind: "project", Title: "Shared repo", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(t.Context(), agent, "", store.CreateInput{Kind: "task", Title: "Migrate the importer", Body: "Open: the CSV path.", ProjectID: project.ID}); err != nil {
		t.Fatal(err)
	}

	result := callTool(t, h, apiTestToken, "context", map[string]any{"project_id": project.ID})
	if result.IsError || !strings.Contains(result.Content[0].Text, "Migrate the importer") {
		t.Fatalf("context: %+v", result)
	}
	result = callTool(t, h, apiTestToken, "context", map[string]any{"project_id": "rec_" + strings.Repeat("0", 32)})
	if !result.IsError {
		t.Fatalf("context of a missing project: %+v", result)
	}

	result = callTool(t, h, apiTestToken, "create", map[string]any{"kind": "task", "title": "Rewrite the exporter", "body": "Next: the schema.", "project_id": project.ID, "tags": []string{"export"}})
	if result.IsError {
		t.Fatalf("create: %+v", result)
	}
	var created struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &created); err != nil || created.Version != 1 {
		t.Fatalf("create result %q: %v", result.Content[0].Text, err)
	}
	record, err := s.Get(t.Context(), created.ID)
	if err != nil || record.CreatedBy != "authenticated-session" || record.Kind != "task" || record.ProjectID != project.ID || len(record.Tags) != 1 {
		t.Fatalf("%+v %v", record, err)
	}
	if result := callTool(t, h, apiTestToken, "create", map[string]any{"kind": "knowledge", "title": "Skips review"}); !result.IsError || !strings.Contains(result.Content[0].Text, "propose") {
		t.Fatalf("create must not take knowledge: %+v", result)
	}
	if result := callTool(t, h, apiTestToken, "create", map[string]any{"kind": "no-such-kind", "title": "x"}); !result.IsError {
		t.Fatalf("create of an unknown kind: %+v", result)
	}

	result = callTool(t, h, apiTestToken, "update", map[string]any{"id": created.ID, "version": 1, "status": "done", "body": "Done: the schema."})
	if result.IsError || !strings.Contains(result.Content[0].Text, `"version":2`) {
		t.Fatalf("update: %+v", result)
	}
	record, err = s.Get(t.Context(), created.ID)
	if err != nil || record.Status != "done" || record.Body != "Done: the schema." || record.Title != "Rewrite the exporter" {
		t.Fatalf("%+v %v", record, err)
	}
	result = callTool(t, h, apiTestToken, "update", map[string]any{"id": created.ID, "version": 1, "title": "Stale edit"})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "record changed") {
		t.Fatalf("stale update: %+v", result)
	}
	proposal, _, err := s.Create(t.Context(), agent, "", store.CreateInput{Kind: "knowledge", Title: "Unreviewed", Body: "x", Sources: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	result = callTool(t, h, apiTestToken, "update", map[string]any{"id": proposal.ID, "version": 1, "status": "accepted"})
	if !result.IsError {
		t.Fatalf("an agent accepted its own proposal: %+v", result)
	}
	if record, _ := s.Get(t.Context(), proposal.ID); record.Status != "proposed" {
		t.Fatalf("status %q", record.Status)
	}
}

func TestMCPReadOnlyIdentityCanReadButNotPropose(t *testing.T) {
	h, _ := mcpHandler(t)
	if result := callTool(t, h, mcpReaderToken, "recall", map[string]any{"query": "anything"}); result.IsError {
		t.Fatalf("reader recall: %+v", result)
	}
	result := callTool(t, h, mcpReaderToken, "propose", map[string]any{"kind": "knowledge", "title": "Nope"})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "read-only") {
		t.Fatalf("reader propose: %+v", result)
	}
	for name, args := range map[string]map[string]any{
		"create": {"kind": "task", "title": "Nope"},
		"update": {"id": "rec_" + strings.Repeat("0", 32), "version": 1, "title": "Nope"},
	} {
		if result := callTool(t, h, mcpReaderToken, name, args); !result.IsError || !strings.Contains(result.Content[0].Text, "read-only") {
			t.Fatalf("reader %s: %+v", name, result)
		}
	}
}

func TestMCPProtocolErrors(t *testing.T) {
	h, _ := mcpHandler(t)
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"parse error":    {"{not json", -32700},
		"batch":          {`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, -32600},
		"not a request":  {`{"jsonrpc":"2.0","id":1}`, -32600},
		"unknown method": {`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, -32601},
		"unknown tool":   {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete","arguments":{}}}`, -32602},
		"bad arguments":  {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get","arguments":{"id":7}}}`, -32602},
	} {
		t.Run(name, func(t *testing.T) {
			w := mcpPost(t, h, apiTestToken, tc.body)
			var response rpcResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Error == nil || response.Error.Code != tc.code {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
		})
	}
	w := mcpPost(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	assertAPIError(t, w, http.StatusUnauthorized)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		r := httptest.NewRequest(method, "/mcp", nil)
		r.Header.Set("Authorization", "Bearer "+apiTestToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /mcp: %d", method, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+apiTestToken)
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign origin: %d %s", w.Code, w.Body.String())
	}
	w = mcpPost(t, h, "owner-secret", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	assertAPIError(t, w, http.StatusForbidden)
}
