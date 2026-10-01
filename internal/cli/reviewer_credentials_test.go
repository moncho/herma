package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// splitCredentials initializes credentials in dir and moves the owner into a
// separate reviewer file, returning the agent and reviewer file paths.
func splitCredentials(t *testing.T, dir string) (string, string) {
	t.Helper()
	agents := filepath.Join(dir, "credentials.json")
	if err := initCredentials(agents); err != nil {
		t.Fatal(err)
	}
	all, err := loadCredentials(agents)
	if err != nil {
		t.Fatal(err)
	}
	reviewers := filepath.Join(t.TempDir(), "reviewer.json")
	data, err := json.Marshal(map[string]credential{"owner": all["owner"]})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewers, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := revokeIdentity(agents, "owner"); err != nil {
		t.Fatal(err)
	}
	return agents, reviewers
}

func TestServeLoadsSeparateReviewerCredentials(t *testing.T) {
	previous := credentialsCheckInterval
	credentialsCheckInterval = 20 * time.Millisecond
	t.Cleanup(func() { credentialsCheckInterval = previous })
	cleanEnv(t)
	dir := shortDir(t)
	agents, reviewers := splitCredentials(t, dir)
	socket := filepath.Join(dir, "herma.sock")
	address := freeAddress(t)
	endpoint := "http://" + address
	log := &safeBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"--credentials", agents, "serve", "--db", filepath.Join(dir, "herma.sqlite3"), "--listen", address, "--reviewer-credentials", reviewers}, log, log)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	waitForServe(t, endpoint, socket)
	t.Setenv("HERMA_URL", endpoint)

	whoami := func(args ...string) (map[string]string, error) {
		data, err := runCLI(t, append(args, "whoami")...)
		if err != nil {
			return nil, err
		}
		var got map[string]string
		err = json.Unmarshal(data, &got)
		return got, err
	}
	if got, err := whoami("--credentials", agents, "--identity", "local-agent"); err != nil || got["listener"] != "tcp" {
		t.Fatalf("agent from agent-only file: %v, %v", got, err)
	}
	if got, err := whoami("--credentials", reviewers, "--socket", socket, "--identity", "owner"); err != nil || got["listener"] != "socket" || got["role"] != "reviewer" {
		t.Fatalf("reviewer from separate file: %v, %v", got, err)
	}

	// A new agent in the agent file is picked up.
	if _, err := runCLI(t, "--credentials", agents, "identity", "add", "laptop"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := whoami("--credentials", agents, "--identity", "laptop"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("identity added to the agent file was not loaded")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Removing the only reviewer is rejected and the previous identities stay.
	if _, err := runCLI(t, "--credentials", reviewers, "identity", "revoke", "owner"); err == nil {
		t.Fatal("revoking the only identity in the reviewer file left it empty")
	}
	if err := os.WriteFile(reviewers, []byte(`{"spare":{"token":"`+strings.Repeat("s", 64)+`","role":"agent"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), "at least one reviewer") {
		if time.Now().After(deadline) {
			t.Fatalf("reload without a reviewer was not rejected: %q", log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, err := whoami("--credentials", agents, "--identity", "local-agent"); err != nil || got["identity"] != "local-agent" {
		t.Fatalf("agent after rejected reload: %v, %v", got, err)
	}
}

func TestServeRequiresAReviewerAndDistinctFiles(t *testing.T) {
	cleanEnv(t)
	dir := shortDir(t)
	agents, reviewers := splitCredentials(t, dir)
	db := filepath.Join(dir, "herma.sqlite3")
	for name, test := range map[string]struct {
		args []string
		want string
	}{
		"no reviewer file": {nil, "at least one reviewer"},
		"same file":        {[]string{"--reviewer-credentials", agents}, "different file"},
		"missing file":     {[]string{"--reviewer-credentials", reviewers + ".missing"}, "read credentials"},
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"--credentials", agents, "serve", "--db", db, "--listen", "127.0.0.1:0"}, test.args...)
			_, err := runCLI(t, args...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want an error containing %q", err, test.want)
			}
		})
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("serve created a database before rejecting its credentials: %v", err)
	}
}
