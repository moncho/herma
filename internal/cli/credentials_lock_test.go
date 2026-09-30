package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func TestStaleCredentialsLockExplainsSafeRecovery(t *testing.T) {
	cleanEnv(t)
	t.Chdir(t.TempDir())
	path := filepath.Join("private credentials", "credentials.json")
	if err := initCredentials(path); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	lockPath, err := filepath.Abs(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	staleLock := []byte("lock left by an interrupted identity update")
	if err := os.WriteFile(lockPath, staleLock, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--credentials", path, "identity", "add", "research-agent"}
	var stdout, stderr bytes.Buffer
	err = Run(context.Background(), args, &stdout, &stderr)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected existing-lock error, got %v", err)
	}
	for _, detail := range []string{lockPath, "remove that file only after ensuring no herma identity command is running", "then retry"} {
		if !strings.Contains(err.Error(), detail) {
			t.Fatalf("lock error lacks recovery detail %q: %v", detail, err)
		}
	}
	if strings.Contains(err.Error()+stdout.String()+stderr.String(), before["owner"].Token) {
		t.Fatal("lock failure exposed the owner token")
	}
	unchanged, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(unchanged, original) {
		t.Fatalf("failed identity update changed credentials: %v", readErr)
	}
	lockContents, readErr := os.ReadFile(lockPath)
	if readErr != nil || !bytes.Equal(lockContents, staleLock) {
		t.Fatalf("failed identity update removed or changed the existing lock: %v", readErr)
	}
	// This test created the stale lock and has no other identity-add process.
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := Run(context.Background(), args, &stdout, &stderr); err != nil {
		t.Fatalf("identity update failed after safe lock removal: %v", err)
	}
	after, err := loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 3 || after["owner"] != before["owner"] || len(after["research-agent"].Token) != 64 || after["research-agent"].Token == before["owner"].Token {
		t.Fatal("identity retry did not preserve the owner and add one distinct identity")
	}
	for _, token := range after {
		if strings.Contains(stdout.String()+stderr.String(), token.Token) {
			t.Fatal("successful identity retry exposed a token")
		}
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful identity retry left its lock behind: %v", err)
	}
}

func TestCredentialsLockFileFailureDoesNotSuggestRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing directory", "credentials.json")
	err := addIdentity(path, "research-agent", store.RoleAgent)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing-parent failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "lock credentials for update") {
		t.Fatalf("missing lock-operation context: %v", err)
	}
	for _, unsafeDetail := range []string{"remove", "already exists", "process is running", "then retry"} {
		if strings.Contains(err.Error(), unsafeDetail) {
			t.Fatalf("unrelated filesystem failure suggests stale-lock recovery: %v", err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed lock attempt created credentials: %v", err)
	}
}
