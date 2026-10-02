package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/api"
	"github.com/moncho/herma/internal/store"
)

func cleanEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"HERMA_URL", "HERMA_CREDENTIALS", "HERMA_IDENTITY", "HERMA_TOKEN", "HERMA_SOCKET", "HERMA_REVIEWER_CREDENTIALS"} {
		t.Setenv(key, "")
	}
}

func TestCredentialsCreationAndIdentityAddition(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "private", "credentials.json")
	var stdout, stderr bytes.Buffer
	args := []string{"--credentials", path, "init"}
	if err := Run(context.Background(), args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	credentials, err := loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := credentials["owner"].Token
	if len(owner) != 64 {
		t.Fatalf("unexpected token length %d", len(owner))
	}
	if strings.Contains(stdout.String(), owner) {
		t.Fatal("printed secret token")
	}
	for filename, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(filename)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("unsafe permissions for %s: %v, %v", filename, info, err)
		}
	}
	if err := Run(context.Background(), args, &stdout, &stderr); err == nil {
		t.Fatal("init overwrote credentials")
	}
	if err := Run(context.Background(), []string{"--credentials", path, "identity", "add", "research-agent"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	credentials, err = loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if credentials["owner"].Token != owner || credentials["research-agent"].Token == owner || len(credentials["research-agent"].Token) != 64 {
		t.Fatal("identity addition damaged credentials")
	}
	if strings.Contains(stdout.String(), credentials["research-agent"].Token) {
		t.Fatal("printed agent token")
	}
	if err := addIdentity(path, "research-agent", store.RoleAgent); err == nil {
		t.Fatal("replaced existing identity")
	}
	if err := addIdentity(path, "../bad", store.RoleAgent); err == nil {
		t.Fatal("accepted invalid identity")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCredentials(path); err == nil {
		t.Fatal("accepted readable credentials")
	}
}

func TestCreatePreservesFileAndGeneratesRequestKey(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HERMA_TOKEN", strings.Repeat("a", 64))
	body := "First line.\n\nSecond line with tabs:\tand punctuation.\n"
	bodyPath := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/records" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if len(r.Header.Get("Idempotency-Key")) != 64 {
			t.Error("missing generated request key")
		}
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 64) {
			t.Error("missing environment token")
		}
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input["body"] != body || input["title"] != "Decision" || input["kind"] != "knowledge" {
			t.Errorf("incorrect create body: %#v", input)
		}
		if _, exists := input["actor"]; exists {
			t.Error("client sent writer attribution")
		}
		if _, exists := input["created_by"]; exists {
			t.Error("client sent writer attribution")
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"record-1","version":1}`))
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"--url", server.URL, "create", "--kind", "knowledge", "--title", "Decision", "--body-file", bodyPath, "--tags", "a, b"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("invalid output: %s", stdout.String())
	}
}

func TestUpdateSendsOnlyExplicitFieldsAndCanClearValues(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HERMA_TOKEN", strings.Repeat("b", 64))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/v1/records/record-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Idempotency-Key") != "retry-the-same-write" {
			t.Error("lost supplied request key")
		}
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if len(input) != 6 || input["version"] != float64(2) || input["body"] != "" || input["owner"] != "" || input["priority"] != float64(0) || input["archived"] != false {
			t.Errorf("incorrect update payload: %#v", input)
		}
		tags, ok := input["tags"].([]any)
		if !ok || len(tags) != 0 {
			t.Errorf("cannot clear tags: %#v", input["tags"])
		}
		_, _ = w.Write([]byte(`{"id":"record-1","version":3}`))
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"--url", server.URL, "update", "record-1", "--version", "2", "--body", "", "--owner", "", "--priority", "0", "--tags", "", "--archived", "false", "--request-id", "retry-the-same-write"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
}

func TestListFiltersAndCredentialSelection(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := initCredentials(path); err != nil {
		t.Fatal(err)
	}
	if err := addIdentity(path, "worker", store.RoleAgent); err != nil {
		t.Fatal(err)
	}
	credentials, err := loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+credentials["worker"].Token {
			t.Error("wrong selected identity")
		}
		q := r.URL.Query()
		for key, want := range map[string]string{"project_id": "proj", "kind": "task", "status": "open", "owner": "worker", "tag": "backend", "q": "memory & context", "limit": "10", "offset": "5", "include_archived": "true"} {
			if q.Get(key) != want {
				t.Errorf("%s: got %q, want %q", key, q.Get(key), want)
			}
		}
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	err = Run(context.Background(), []string{"--url", server.URL, "--credentials", path, "--identity", "worker", "list", "--project", "proj", "--kind", "task", "--status", "open", "--owner", "worker", "--tag", "backend", "--q", "memory & context", "--limit", "10", "--offset", "5", "--include-archived"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
}

func TestBadArgumentsDoNotSendRequests(t *testing.T) {
	cleanEnv(t)
	for _, args := range [][]string{
		{"create", "--kind", "task"},
		{"create", "--kind", "task", "--title", "t", "--body", "x", "--body-file", "x"},
		{"create", "--kind", "task", "--title", "t", "--body", strings.Repeat("x", (64<<10)+1)},
		{"create", "--kind", "task", "--title", "t", "--body", string([]byte{0xff})},
		{"update", "id", "--title", "new"},
		{"update", "id", "--version", "1"},
		{"update", "id", "--version", "1", "--archived", "yes"},
		{"list", "--global", "--project", "id"},
		{"list", "--limit", "201"},
		{"list", "--offset", "-1"},
		{"list", "extra"},
		{"context"},
		{"schema", "extra"},
		{"unknown"},
	} {
		var stdout, stderr bytes.Buffer
		if err := Run(context.Background(), args, &stdout, &stderr); err == nil || strings.Contains(err.Error(), "credentials") {
			t.Errorf("%v: expected argument error, got %v", args, err)
		}
		if stdout.Len() != 0 {
			t.Errorf("unexpected successful output for %v", args)
		}
	}
}

func TestPrivateDatabaseFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "knowledge.sqlite3")
	if err := prepareDatabase(path); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for filename, mode := range map[string]os.FileMode{filepath.Dir(path): 0700, path: 0600, path + "-wal": 0600, path + "-shm": 0600} {
		info, err := os.Stat(filename)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("unsafe database permissions for %s: %v, %v", filename, info, err)
		}
	}
	other := filepath.Join(t.TempDir(), "existing.sqlite3")
	if err := os.WriteFile(other, []byte("existing bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepareDatabase(other); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(other)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("did not secure an existing database")
	}
	data, err := os.ReadFile(other)
	if err != nil || string(data) != "existing bytes" {
		t.Fatal("changed an existing database's contents")
	}
	link := filepath.Join(t.TempDir(), "linked.sqlite3")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if err := prepareDatabase(link); err == nil {
		t.Fatal("accepted a database symlink")
	}
}

func TestFreshAgentCLIWorkflowAndRetry(t *testing.T) {
	cleanEnv(t)
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ownerToken, agentToken := strings.Repeat("o", 64), strings.Repeat("a", 64)
	server := httptest.NewServer(api.NewHandler(db, []api.Identity{
		{Name: "owner", Token: ownerToken, Role: store.RoleReviewer},
		{Name: "agent", Token: agentToken, Role: store.RoleAgent},
	}))
	defer server.Close()
	t.Setenv("HERMA_URL", server.URL)
	t.Setenv("HERMA_TOKEN", agentToken)
	runCommand := func(args ...string) []byte {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := Run(context.Background(), args, &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return stdout.Bytes()
	}
	var project, task store.Record
	if err := json.Unmarshal(runCommand("create", "--kind", "project", "--title", "Persistent memory"), &project); err != nil {
		t.Fatal(err)
	}
	// Reviewer tokens are refused over TCP; write the reviewed record directly.
	if _, _, err := db.Create(context.Background(), store.Author{Name: "owner", Role: store.RoleReviewer}, "", store.CreateInput{
		Kind: "knowledge", Title: "Use Go", Body: "The knowledge service will be implemented in Go.",
		ProjectID: project.ID, Status: "accepted", Sources: []string{"https://example.com/review"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(runCommand("create", "--kind", "task", "--title", "Continue implementation", "--project", project.ID, "--owner", "agent"), &task); err != nil {
		t.Fatal(err)
	}
	var contextResult struct {
		Tasks     []store.Record `json:"tasks"`
		Knowledge []store.Record `json:"knowledge"`
	}
	if err := json.Unmarshal(runCommand("context", "--project", project.ID, "--format", "json"), &contextResult); err != nil {
		t.Fatal(err)
	}
	if len(contextResult.Tasks) != 1 || len(contextResult.Knowledge) != 0 || contextResult.Tasks[0].ID != task.ID {
		t.Fatalf("fresh agent did not retrieve project context: %#v", contextResult)
	}
	args := []string{"update", task.ID, "--version", "1", "--status", "in_progress", "--request-id", "claim-task"}
	first, replay := runCommand(args...), runCommand(args...)
	if !bytes.Equal(first, replay) {
		t.Fatal("CLI retry did not return the same record")
	}
	var updated store.Record
	if err := json.Unmarshal(first, &updated); err != nil || updated.Version != 2 || updated.UpdatedBy != "agent" {
		t.Fatalf("bad attributed update: %#v, %v", updated, err)
	}
	var stdout, stderr bytes.Buffer
	err = Run(context.Background(), []string{"update", task.ID, "--version", "1", "--status", "done"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "conflict") || !strings.Contains(err.Error(), "--request-id") || stdout.Len() != 0 {
		t.Fatalf("stale write did not report conflict with retry key: %v", err)
	}
}
