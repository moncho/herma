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
		"no reviewer": {`{"worker":{"token":"` + token + `","role":"agent"}}`, "at least one reviewer"},
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

func TestIdentityAddDefaultsToAgentAndRevokeKeepsAReviewer(t *testing.T) {
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
	if err := run("identity", "revoke", "owner"); err == nil || !strings.Contains(err.Error(), "last reviewer") {
		t.Fatalf("revoking the last reviewer: %v", err)
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
