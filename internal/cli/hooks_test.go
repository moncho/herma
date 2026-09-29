package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/api"
	"github.com/moncho/herma/internal/project"
	"github.com/moncho/herma/internal/store"
)

func runInput(t *testing.T, ctx context.Context, input string, args ...string) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := RunWithInput(ctx, args, strings.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatalf("%v: %v (%s)", args, err, stderr.String())
	}
	return stdout.Bytes()
}

func hookEvent(t *testing.T, cwd string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{"cwd": cwd, "hook_event_name": "SessionStart", "source": "startup", "session_id": "test-session"})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBoundContextAndSessionStartLoadFreshCoordination(t *testing.T) {
	cleanEnv(t)
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(api.NewHandler(db, map[string]string{"worker": "test-token"}))
	defer server.Close()
	t.Setenv("HERMA_URL", server.URL)
	t.Setenv("HERMA_TOKEN", "test-token")
	create := func(in store.CreateInput) store.Record {
		t.Helper()
		r, _, err := db.Create(context.Background(), "worker", "", in)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	p := create(store.CreateInput{Kind: "project", Title: "Session coordination"})
	create(store.CreateInput{Kind: "knowledge", Title: "Durable wiki fact", ProjectID: p.ID, Status: "accepted"})
	create(store.CreateInput{Kind: "task", Title: "Session A edits the API", ProjectID: p.ID, Status: "in_progress", Owner: "session-a", Sources: []string{"https://linear.app/example/issue/EX-123"}})
	note := create(store.CreateInput{Kind: "note", Title: "Latest handoff", Body: strings.Repeat("引き継ぎ🙂", 500), ProjectID: p.ID})
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "src")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	runInput(t, context.Background(), "", "project", "bind", "--project", p.ID, "--dir", root, "--max-bytes", "4096")
	t.Chdir(nested)
	check := func(data []byte) {
		t.Helper()
		if len(data) > 4096 || !json.Valid(data) {
			t.Fatalf("unbounded/invalid context: %d bytes", len(data))
		}
		var packet struct {
			Project   store.Record   `json:"project"`
			Tasks     []store.Record `json:"tasks"`
			Notes     []store.Record `json:"notes"`
			Knowledge []store.Record `json:"knowledge"`
			Scope     string         `json:"scope"`
		}
		if err := json.Unmarshal(data, &packet); err != nil {
			t.Fatal(err)
		}
		if packet.Project.ID != p.ID || len(packet.Tasks) != 1 || len(packet.Knowledge) != 0 || len(packet.Notes) != 1 || packet.Notes[0].ID != note.ID {
			t.Fatalf("unexpected coordination packet: %s", data)
		}
		if !strings.Contains(packet.Scope, "untrusted") {
			t.Fatal("context must distinguish record data from instructions")
		}
	}
	check(runInput(t, context.Background(), "", "context"))
	var envelope struct {
		Hook struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	data := runInput(t, context.Background(), hookEvent(t, nested), "hook", "session-start")
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Hook.Event != "SessionStart" {
		t.Fatalf("wrong hook response: %s", data)
	}
	check([]byte(envelope.Hook.Context))
	create(store.CreateInput{Kind: "note", Title: "Fresh handoff after first startup", ProjectID: p.ID})
	data = runInput(t, context.Background(), hookEvent(t, nested), "hook", "session-start")
	if !bytes.Contains(data, []byte("Fresh handoff after first startup")) {
		t.Fatal("hook reused stale context")
	}
	records, err := db.List(context.Background(), store.ListOptions{Archived: true, Limit: 200})
	if err != nil || records.Total != 5 {
		t.Fatalf("read-only startup changed records: %+v, %v", records, err)
	}
}

func TestContextCLIRespectsBudgetAndDurableOptIn(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HERMA_TOKEN", "token")
	id := "rec_" + strings.Repeat("a", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("include_durable") != "true" || r.URL.Query().Get("max_bytes") != "3072" {
			t.Errorf("wrong context query: %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"project":{"id":"`+id+`"},"max_bytes":3072,"body":"`+strings.Repeat("x", 2800)+`"}`)
	}))
	defer server.Close()
	data := runInput(t, context.Background(), "", "--url", server.URL, "context", "--project", id, "--max-bytes", "3072", "--include-durable")
	if len(data) > 3072 || bytes.Count(data, []byte("\n")) != 1 {
		t.Fatal("CLI expanded the server's bounded JSON")
	}
	for _, budget := range []int{0, 2047, 65537} {
		var out, errs bytes.Buffer
		err := Run(context.Background(), []string{"context", "--project", id, "--max-bytes", strconv.Itoa(budget)}, &out, &errs)
		if err == nil || !strings.Contains(err.Error(), "max-bytes") || out.Len() > 0 {
			t.Fatalf("accepted invalid context budget %d: %v", budget, err)
		}
	}
}

func TestSessionStartFailuresAreBoundedAndDoNotBlock(t *testing.T) {
	cleanEnv(t)
	id := "rec_" + strings.Repeat("b", 32)
	root := t.TempDir()
	if _, err := project.Bind(root, project.Binding{ProjectID: id}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMA_TOKEN", "private-test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"private-test-token `+strings.Repeat("remote failure ", 1000)+`"}}`)
	}))
	defer server.Close()
	t.Setenv("HERMA_URL", server.URL)
	for _, input := range []string{hookEvent(t, root), `{`, `{ "hook_event_name":"SessionStart", "cwd":"relative" }`, strings.Repeat("x", maxHookInput+1)} {
		data := runInput(t, context.Background(), input, "hook", "session-start")
		if !json.Valid(data) || !bytes.Contains(data, []byte("systemMessage")) || bytes.Contains(data, []byte("additionalContext")) || bytes.Contains(data, []byte("private-test-token")) || len(data) > 512 {
			t.Fatalf("bad fail-open warning: %s", data)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := runInput(t, ctx, hookEvent(t, root), "hook", "session-start")
	if !bytes.Contains(data, []byte("systemMessage")) {
		t.Fatal("canceled request did not fail open")
	}
	unbound := t.TempDir()
	if err := os.Mkdir(filepath.Join(unbound, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	data = runInput(t, context.Background(), hookEvent(t, unbound), "hook", "session-start")
	if len(data) != 0 {
		t.Fatalf("unbound projects should be a quiet no-op: %s", data)
	}
}

func TestHookInstallUsesAbsoluteCredentialsWithoutTokens(t *testing.T) {
	cleanEnv(t)
	root := t.TempDir()
	if _, err := project.Bind(root, project.Binding{ProjectID: "rec_" + strings.Repeat("c", 32)}); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentials, []byte(`{"session-a":"do-not-embed-this-token-in-hook-configuration"}`), 0600); err != nil {
		t.Fatal(err)
	}
	runInput(t, context.Background(), "", "--credentials", credentials, "--identity", "session-a", "hook", "install", "--client", "both", "--dir", root)
	for _, relative := range []string{".claude/settings.local.json", ".codex/hooks.json"} {
		data, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("do-not-embed-this-token")) || !bytes.Contains(data, []byte(credentials)) || !bytes.Contains(data, []byte("session-a")) || !bytes.Contains(data, []byte("session-start")) {
			t.Fatalf("wrong generated command: %s", data)
		}
	}
}

func TestHookRefusesOverBudgetServerResponse(t *testing.T) {
	cleanEnv(t)
	id := "rec_" + strings.Repeat("d", 32)
	root := t.TempDir()
	if _, err := project.Bind(root, project.Binding{ProjectID: id, MaxBytes: 2048}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMA_TOKEN", "token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"project":{"id":"`+id+`"},"max_bytes":2048,"body":"`+strings.Repeat("x", 5000)+`"}`)
	}))
	defer server.Close()
	t.Setenv("HERMA_URL", server.URL)
	data := runInput(t, context.Background(), hookEvent(t, root), "hook", "session-start")
	if !bytes.Contains(data, []byte("systemMessage")) || len(data) > 512 {
		t.Fatal("over-budget response was injected")
	}
}

func TestTokenHookNeverFallsBackToCredentialFile(t *testing.T) {
	cleanEnv(t)
	root := t.TempDir()
	if _, err := project.Bind(root, project.Binding{ProjectID: "rec_" + strings.Repeat("e", 32)}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMA_TOKEN", "private-token-which-must-not-be-written")
	runInput(t, context.Background(), "", "hook", "install", "--client", "claude", "--dir", root)
	config, err := os.ReadFile(filepath.Join(root, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(config, []byte("private-token")) || !bytes.Contains(config, []byte("--require-token")) {
		t.Fatalf("token mode was not pinned: %s", config)
	}
	t.Setenv("HERMA_TOKEN", "")
	data := runInput(t, context.Background(), hookEvent(t, root), "hook", "session-start", "--require-token")
	if !bytes.Contains(data, []byte("requires HERMA_TOKEN")) || bytes.Contains(data, []byte("additionalContext")) {
		t.Fatalf("missing token fell back to a different identity: %s", data)
	}
}
