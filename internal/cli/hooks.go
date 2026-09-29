package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/moncho/herma/internal/hooks"
	"github.com/moncho/herma/internal/project"
)

const hookTimeout = 5 * time.Second
const maxHookInput = 64 << 10

func hook(ctx context.Context, cfg config, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: herma hook install --client claude|codex|both [--dir PATH] | herma hook session-start")
	}
	switch args[0] {
	case "install":
		return installHook(cfg, args[1:], stdout, stderr)
	case "session-start":
		fs := flags("hook session-start", stderr)
		requireToken := fs.Bool("require-token", false, "require HERMA_TOKEN; do not fall back to credentials")
		if err := parse(fs, args[1:]); err != nil {
			return err
		}
		return sessionStart(ctx, cfg, *requireToken, stdin, stdout)
	default:
		return fmt.Errorf("unknown hook command %q", args[0])
	}
}

func installHook(cfg config, args []string, stdout, stderr io.Writer) error {
	fs := flags("hook install", stderr)
	agent := fs.String("client", "", "claude, codex, or both (required)")
	dir := fs.String("dir", ".", "bound repository directory")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *agent != "claude" && *agent != "codex" && *agent != "both" {
		return errors.New("hook install requires --client claude, codex, or both")
	}
	absoluteDir, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	_, bindingPath, err := project.Discover(absoluteDir)
	if err != nil {
		return fmt.Errorf("find hook project: %w; first run herma project bind --project ID in the repository root", err)
	}
	// Validate authentication now; never write a bearer token into hook config.
	if _, err := cfg.client(); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	commandArgs := []string{executable, "--url", cfg.endpoint}
	if os.Getenv("HERMA_TOKEN") == "" {
		credentials, err := filepath.Abs(cfg.credentials)
		if err != nil {
			return err
		}
		commandArgs = append(commandArgs, "--credentials", credentials, "--identity", cfg.identity)
	}
	commandArgs = append(commandArgs, "hook", "session-start")
	if os.Getenv("HERMA_TOKEN") != "" {
		commandArgs = append(commandArgs, "--require-token")
	}
	command, err := hooks.QuoteCommand(commandArgs)
	if err != nil {
		return err
	}
	installations, err := hooks.Install(filepath.Dir(bindingPath), *agent, command)
	if err != nil {
		return err
	}
	return output(stdout, map[string]any{
		"installed": installations,
		"message":   "Start a new session. In Codex, review and trust this hook through /hooks first. Token-based hooks require HERMA_TOKEN in the agent environment.",
	})
}

// A missing binding is a quiet no-op. Other failures are visible warnings but
// never block startup, request permission, or inject an unbounded error body.
func sessionStart(ctx context.Context, cfg config, requireToken bool, stdin io.Reader, stdout io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(stdin, maxHookInput+1))
	if err != nil || len(data) > maxHookInput {
		return hookWarning(stdout, "herma context unavailable: could not read the SessionStart event.")
	}
	var event struct {
		Event string `json:"hook_event_name"`
		Cwd   string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.Event != "SessionStart" || !filepath.IsAbs(event.Cwd) {
		return hookWarning(stdout, "herma context unavailable: expected a SessionStart event with an absolute cwd.")
	}
	binding, _, err := project.Discover(event.Cwd)
	if errors.Is(err, project.ErrNoBinding) {
		return nil
	}
	if err != nil {
		return hookWarning(stdout, "herma context unavailable: check this repository's .herma-project.json binding.")
	}
	if requireToken && os.Getenv("HERMA_TOKEN") == "" {
		return hookWarning(stdout, "herma context unavailable: this hook requires HERMA_TOKEN in the agent environment; credential-file fallback is disabled.")
	}
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	data, err = cfg.contextData(ctx, binding.ProjectID, binding.MaxBytes, false)
	if err != nil {
		return hookWarning(stdout, "herma context unavailable: check the service and credentials with herma context from this repository. Session startup will continue.")
	}
	// The decoded additionalContext string, including its scope/trust metadata,
	// stays within the binding's byte budget. JSON escaping is only transport.
	return output(stdout, map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "SessionStart", "additionalContext": string(bytes.TrimSpace(data)),
	}})
}

func hookWarning(stdout io.Writer, message string) error {
	return output(stdout, map[string]string{"systemMessage": message})
}
