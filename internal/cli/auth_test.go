package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTokenAndSelectedIdentityFailBeforeSendingRequest(t *testing.T) {
	for _, test := range []struct {
		name     string
		identity string
		flags    []string
	}{
		{name: "explicit identity", flags: []string{"--identity", "worker"}},
		{name: "explicit default", flags: []string{"--identity=owner"}},
		{name: "explicit empty", flags: []string{"--identity", ""}},
		{name: "single dash", flags: []string{"-identity=worker"}},
		{name: "repeated identity", flags: []string{"--identity", "worker", "--identity", "owner"}},
		{name: "environment identity", identity: "worker"},
		{name: "environment default", identity: "owner"},
		{name: "environment identity with explicit empty", identity: "worker", flags: []string{"--identity="}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cleanEnv(t)
			token := strings.Repeat("secret-token-", 4)
			t.Setenv("HERMA_TOKEN", token)
			t.Setenv("HERMA_IDENTITY", test.identity)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			args := append([]string{"--url", server.URL}, test.flags...)
			args = append(args, "create", "--kind", "task", "--title", "Must not be attributed to the wrong writer")
			var stdout, stderr bytes.Buffer
			err := Run(context.Background(), args, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "HERMA_TOKEN cannot be combined") || !strings.Contains(err.Error(), "unset HERMA_TOKEN") {
				t.Fatalf("expected actionable identity ambiguity error, got %v", err)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(stdout.String()+stderr.String(), token) {
				t.Fatal("identity error exposed token")
			}
			if requests.Load() != 0 || stdout.Len() != 0 {
				t.Fatalf("ambiguous identity sent %d requests or printed successful output", requests.Load())
			}
		})
	}
}

func TestTokenWithoutSelectedIdentityUsesToken(t *testing.T) {
	cleanEnv(t)
	token := strings.Repeat("t", 64)
	t.Setenv("HERMA_TOKEN", token)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("did not authenticate with token")
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"--url", server.URL, "--credentials", filepath.Join(t.TempDir(), "absent.json"), "schema"}, &stdout, &stderr)
	if err != nil || requests.Load() != 1 || !json.Valid(stdout.Bytes()) {
		t.Fatalf("token-only request failed: %v, requests %d, stdout %s", err, requests.Load(), stdout.String())
	}
}

func TestSelectedIdentityWithoutTokenUsesCredentials(t *testing.T) {
	for _, test := range []struct {
		name     string
		identity string
		flags    []string
	}{
		{name: "explicit", flags: []string{"--identity", "worker"}},
		{name: "environment", identity: "worker"},
		{name: "explicit overrides environment", identity: "owner", flags: []string{"--identity", "worker"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("HERMA_IDENTITY", test.identity)
			path := filepath.Join(t.TempDir(), "credentials.json")
			if err := initCredentials(path); err != nil {
				t.Fatal(err)
			}
			if err := addIdentity(path, "worker"); err != nil {
				t.Fatal(err)
			}
			credentials, err := loadCredentials(path)
			if err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "Bearer "+credentials["worker"] {
					t.Error("did not authenticate as the selected identity")
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			args := append([]string{"--url", server.URL, "--credentials", path}, test.flags...)
			args = append(args, "schema")
			var stdout, stderr bytes.Buffer
			if err := Run(context.Background(), args, &stdout, &stderr); err != nil || requests.Load() != 1 {
				t.Fatalf("selected identity failed: %v, requests %d", err, requests.Load())
			}
		})
	}
}

func TestLocalCommandsIgnoreClientAuthenticationSettings(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HERMA_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HERMA_IDENTITY", "different-identity")
	path := filepath.Join(t.TempDir(), "credentials.json")
	for _, command := range [][]string{{"init"}, {"identity", "add", "worker"}} {
		args := append([]string{"--credentials", path, "--identity", "owner"}, command...)
		var stdout, stderr bytes.Buffer
		if err := Run(context.Background(), args, &stdout, &stderr); err != nil {
			t.Fatalf("local command %v rejected client-only settings: %v", command, err)
		}
	}
	// A canceled context starts and immediately shuts down a server, exercising
	// server credential loading without leaving a service running after the test.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	err := Run(ctx, []string{"--credentials", path, "--identity", "owner", "serve", "--db", filepath.Join(t.TempDir(), "knowledge.sqlite3"), "--listen", "127.0.0.1:0"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("serve rejected client-only settings: %v", err)
	}
}
