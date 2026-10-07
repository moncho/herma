package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func TestInitCreatesReviewerAndLocalAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := initCredentials(path); err != nil {
		t.Fatal(err)
	}
	credentials, err := loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 2 || credentials["owner"].Role != store.RoleReviewer || credentials["local-agent"].Role != store.RoleAgent {
		t.Fatalf("init identities: %+v", credentials)
	}
	if len(credentials["owner"].Token) != 64 || credentials["owner"].Token == credentials["local-agent"].Token {
		t.Fatal("init tokens must be distinct 64-character secrets")
	}
}

func TestLoadCredentialsRejectsMalformedEntriesAndInvalidRoles(t *testing.T) {
	token := strings.Repeat("t", 64)
	for name, test := range map[string]struct{ content, want string }{
		"plain token": {`{"owner":"` + token + `"}`, "token and role"},
		"role typo":   {`{"owner":{"token":"` + token + `","role":"reviwer"}}`, "reviewer, agent, or read-only"},
		"extra field": {`{"owner":{"token":"` + token + `","role":"reviewer","admin":true}}`, "token and role"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			if err := os.WriteFile(path, []byte(test.content), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := loadCredentials(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want an error containing %q", err, test.want)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatal("error exposed a token")
			}
		})
	}
}

func TestIdentityAddDefaultsToAgentAndRevokeRemoves(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "credentials.json")
	run := func(args ...string) error {
		var stdout, stderr bytes.Buffer
		return Run(context.Background(), append([]string{"--credentials", path}, args...), &stdout, &stderr)
	}
	if err := run("init"); err != nil {
		t.Fatal(err)
	}
	if err := run("identity", "add", "laptop"); err != nil {
		t.Fatal(err)
	}
	if err := run("identity", "add", "cloud-ci", "--role", "read-only"); err != nil {
		t.Fatal(err)
	}
	if err := run("identity", "add", "bad-role", "--role", "admin"); err == nil {
		t.Fatal("accepted an unknown role")
	}
	credentials, err := loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if credentials["laptop"].Role != store.RoleAgent || credentials["cloud-ci"].Role != store.RoleReadOnly {
		t.Fatalf("roles: %+v", credentials)
	}
	if err := run("identity", "revoke", "laptop"); err != nil {
		t.Fatal(err)
	}
	if err := run("identity", "revoke", "missing"); err == nil {
		t.Fatal("revoked an unknown identity")
	}
	credentials, err = loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := credentials["laptop"]; ok || credentials["owner"].Role != store.RoleReviewer {
		t.Fatalf("after revoke: %+v", credentials)
	}
}

func TestIdentityAddWritesAClientFileWithOnlyThatIdentity(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	run := func(args ...string) error {
		var stdout, stderr bytes.Buffer
		return Run(context.Background(), append([]string{"--credentials", path}, args...), &stdout, &stderr)
	}
	if err := run("init"); err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(dir, "laptop.json")
	if err := run("identity", "add", "laptop", "--client-file", client); err != nil {
		t.Fatal(err)
	}
	server, err := loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loadCredentials(client)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["laptop"] != server["laptop"] || got["laptop"].Role != store.RoleAgent {
		t.Fatalf("client file holds %+v", got)
	}

	if err := run("identity", "add", "second", "--client-file", client); err == nil {
		t.Fatal("overwrote an existing client file")
	}
	if err := run("identity", "add", "boss", "--role", "reviewer", "--client-file", filepath.Join(dir, "boss.json")); err == nil {
		t.Fatal("wrote a client file for a reviewer")
	}
	if err := run("identity", "add", "nowhere", "--client-file", filepath.Join(dir, "missing", "c.json")); err == nil {
		t.Fatal("accepted a client file in a missing folder")
	}
	server, err = loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"second", "boss", "nowhere"} {
		if _, ok := server[name]; ok {
			t.Errorf("failed add still created identity %q", name)
		}
	}
}

func TestLoadCredentialsAcceptsAgentOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	token := strings.Repeat("a", 64)
	if err := os.WriteFile(path, []byte(`{"local-agent":{"token":"`+token+`","role":"agent"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	credentials, err := loadCredentials(path)
	if err != nil || credentials["local-agent"].Role != store.RoleAgent {
		t.Fatalf("agent-only file: %+v, %v", credentials, err)
	}
	cfg := config{credentials: path, identity: "local-agent"}
	if role, err := cfg.identityRole(); err != nil || role != store.RoleAgent {
		t.Fatalf("identity role from agent-only file: %q, %v", role, err)
	}
}

func TestRevokingAFilesLastReviewerWarns(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := initCredentials(path); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"--credentials", path, "identity", "revoke", "owner"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "no reviewer left") {
		t.Fatalf("revoke output did not warn: %s", stdout.String())
	}
	credentials, err := loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := credentials["owner"]; ok {
		t.Fatal("owner was not revoked")
	}
}

func TestLoadServerIdentitiesMergesFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	agentToken, reviewerToken := strings.Repeat("a", 64), strings.Repeat("r", 64)
	agents := write("agents.json", `{"local-agent":{"token":"`+agentToken+`","role":"agent"}}`)
	reviewers := write("reviewers.json", `{"owner":{"token":"`+reviewerToken+`","role":"reviewer"}}`)
	merged, err := loadServerIdentities([]string{agents, reviewers})
	if err != nil || len(merged) != 2 || merged["owner"].Role != store.RoleReviewer || merged["local-agent"].Role != store.RoleAgent {
		t.Fatalf("merged: %+v, %v", merged, err)
	}
	for name, test := range map[string]struct {
		paths []string
		want  string
	}{
		"no reviewer":    {[]string{agents}, "at least one reviewer"},
		"same name":      {[]string{reviewers, write("again.json", `{"owner":{"token":"`+strings.Repeat("x", 64)+`","role":"reviewer"}}`)}, "more than one credentials file"},
		"same token":     {[]string{agents, write("copy.json", `{"owner":{"token":"`+agentToken+`","role":"reviewer"}}`)}, "distinct token"},
		"invalid second": {[]string{agents, write("bad.json", `{"owner":`)}, "JSON object"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadServerIdentities(test.paths)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want an error containing %q", err, test.want)
			}
			if strings.Contains(err.Error(), agentToken) {
				t.Fatal("error exposed a token")
			}
		})
	}
}

func TestDefaultIdentityIsLocalAgent(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := initCredentials(path); err != nil {
		t.Fatal(err)
	}
	cfg := config{credentials: path, identity: defaultIdentity}
	role, err := cfg.identityRole()
	if err != nil || defaultIdentity != "local-agent" || role != store.RoleAgent {
		t.Fatalf("default identity %q has role %q (%v)", defaultIdentity, role, err)
	}
}
