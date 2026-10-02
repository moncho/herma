package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moncho/herma/internal/backup"
	"github.com/moncho/herma/internal/client"
	"github.com/moncho/herma/internal/hooks"
	"github.com/moncho/herma/internal/project"
	"github.com/moncho/herma/internal/rules"
	"github.com/moncho/herma/internal/store"
)

const hookTimeout = 5 * time.Second
const maxHookInput = 64 << 10

const (
	claudeContextLimit    = 10000 // Claude Code's additionalContext cap, in characters
	legacyDefaultMaxBytes = 12288 // herma's default before 10000
	rulesLineAdvice       = 200
	legacyHookWarning     = "herma: this SessionStart hook predates compact context; rerun herma hook install --client claude|codex in this repository."
)

func hook(ctx context.Context, cfg config, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: herma hook install --client claude|codex|both [--dir PATH] | herma hook session-start [--client claude|codex]")
	}
	switch args[0] {
	case "install":
		return installHook(cfg, args[1:], stdout, stderr)
	case "session-start":
		fs := flags("hook session-start", stderr)
		requireToken := fs.Bool("require-token", false, "require HERMA_TOKEN; do not fall back to credentials")
		client := fs.String("client", "", "claude or codex; omitted means the legacy JSON packet")
		if err := parse(fs, args[1:]); err != nil {
			return err
		}
		if *client != "" && *client != "claude" && *client != "codex" {
			return errors.New("hook session-start --client must be claude or codex")
		}
		return sessionStart(ctx, cfg, *requireToken, *client, stdin, stdout)
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
	if os.Getenv("HERMA_TOKEN") == "" {
		role, err := cfg.identityRole()
		if err != nil {
			return err
		}
		if role == store.RoleReviewer {
			return fmt.Errorf("hook install refuses reviewer identity %q: session hooks run inside agent sessions. Use an agent identity, for example --identity local-agent", cfg.identity)
		}
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
	command := func(client string) (string, error) {
		args := append(append([]string{}, commandArgs...), "hook", "session-start", "--client", client)
		if os.Getenv("HERMA_TOKEN") != "" {
			args = append(args, "--require-token")
		}
		return hooks.QuoteCommand(args)
	}
	installations, err := hooks.Install(filepath.Dir(bindingPath), *agent, command)
	if err != nil {
		return err
	}
	result := map[string]any{
		"installed": installations,
		"message":   "Start a new session. In Codex, review and trust this hook through /hooks first. Token-based hooks require HERMA_TOKEN in the agent environment.",
	}
	if os.Getenv("HERMA_TOKEN") == "" {
		credentials, err := filepath.Abs(cfg.credentials)
		if err != nil {
			return err
		}
		loaded, err := loadCredentials(credentials)
		if err != nil {
			return err
		}
		if !hasReviewer(loaded) {
			return output(stdout, result)
		}
		// Claude Code writes absolute paths in permission rules with a leading //.
		result["permission_advice"] = "Keep agents from reading the reviewer token: in Claude Code, add \"Read(/" + credentials + ")\" to permissions.deny; in Codex, keep the credentials outside the writable workspace."
	}
	return output(stdout, result)
}

// A missing binding is a quiet no-op. Other failures are visible warnings but
// never block startup, request permission, or inject an unbounded error body.
func sessionStart(ctx context.Context, cfg config, requireToken bool, client string, stdin io.Reader, stdout io.Writer) error {
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
	binding, bindingPath, err := project.Discover(event.Cwd)
	if errors.Is(err, project.ErrNoBinding) {
		return nil
	}
	if err != nil {
		return hookWarning(stdout, "herma context unavailable: check this repository's .herma-project.json binding.")
	}
	if requireToken && os.Getenv("HERMA_TOKEN") == "" {
		return hookWarning(stdout, "herma context unavailable: this hook requires HERMA_TOKEN in the agent environment; credential-file fallback is disabled.")
	}
	if os.Getenv("HERMA_TOKEN") == "" {
		if role, err := cfg.identityRole(); err == nil && role == store.RoleReviewer {
			return hookWarning(stdout, "herma context unavailable: this hook uses the reviewer identity. Reinstall it with an agent identity: herma --identity local-agent hook install --client claude|codex|both. Session startup will continue.")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	data, warnings, err := sessionContext(ctx, cfg, client, binding, filepath.Dir(bindingPath))
	if err != nil {
		return hookWarning(stdout, "herma context unavailable: check the service (restart herma serve after upgrading herma) and credentials with herma context from this repository. Session startup will continue.")
	}
	// The decoded additionalContext string, including its scope/trust metadata,
	// stays within the hook's byte budget. JSON escaping is only transport.
	result := map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "SessionStart", "additionalContext": string(bytes.TrimSpace(data)),
	}}
	if warning := backupWarning(ctx, cfg); warning != "" {
		warnings = append(warnings, warning)
	}
	if len(warnings) > 0 {
		result["systemMessage"] = strings.Join(warnings, "\n")
	}
	return output(stdout, result)
}

// sessionContext selects the packet for the hook's client. Claude reads
// principles from the rules file; Codex and legacy hooks get them inline.
func sessionContext(ctx context.Context, cfg config, client string, binding project.Binding, checkout string) ([]byte, []string, error) {
	switch client {
	case "claude":
		return claudeContext(ctx, cfg, binding, checkout)
	case "codex":
		data, err := cfg.contextData(ctx, contextRequest{ID: binding.ProjectID, Budget: binding.MaxBytes, Format: "text", Principles: "include"})
		return data, nil, err
	default:
		// Legacy hooks are usually Claude's; its old 12288 default overflows the cap.
		budget := binding.MaxBytes
		if budget == legacyDefaultMaxBytes {
			budget = claudeContextLimit
		}
		data, err := cfg.contextData(ctx, contextRequest{ID: binding.ProjectID, Budget: budget, Format: "json", Principles: "include"})
		return data, []string{legacyHookWarning}, err
	}
}

// claudeContext keeps the rules file current. Claude Code reads it before this
// hook runs, so a changed file is also delivered once in the packet.
func claudeContext(ctx context.Context, cfg config, binding project.Binding, checkout string) ([]byte, []string, error) {
	var warnings []string
	budget := binding.MaxBytes
	if budget > claudeContextLimit {
		budget = claudeContextLimit
	}
	if binding.MaxBytes > claudeContextLimit && binding.MaxBytes != legacyDefaultMaxBytes {
		warnings = append(warnings, fmt.Sprintf("herma: this repository's max_bytes (%d) exceeds Claude Code's 10,000-character hook limit; using %d. Set max_bytes to %d in .herma-project.json to silence this.", binding.MaxBytes, claudeContextLimit, claudeContextLimit))
	}
	principles, err := cfg.principlesFile(ctx, binding.ProjectID)
	if err != nil {
		// Only a project_unavailable error (the project is archived or no
		// longer exists) removes the herma-generated file, so its principles stop
		// applying. Every other failure, including 401, 403, an older server's
		// 404 for the route, 5xx and network errors, keeps the file.
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "project_unavailable" {
			_, _ = rules.Sync(checkout, nil)
		}
		return nil, nil, err
	}
	mode := "omit"
	if isHomeDir(checkout) {
		// Rules under ~/.claude apply to every Claude session, not this project.
		mode = "include"
		warnings = append(warnings, "herma: this binding is in your home directory, where "+rules.Path+" would apply to every Claude session; principles are in the session context instead.")
	} else if changed, err := rules.Sync(checkout, principles); err != nil {
		// Claude may have loaded a stale file; say these principles replace it.
		mode = "replace"
		warnings = append(warnings, "herma: could not write "+rules.Path+" ("+err.Error()+"); principles are in the session context instead.")
	} else if changed {
		mode = "changed"
	}
	if lines := bytes.Count(principles, []byte{'\n'}); lines > rulesLineAdvice {
		warnings = append(warnings, fmt.Sprintf("herma: %s has %d lines; Claude Code follows rules files best under 200 lines. Consider fewer or shorter principles.", rules.Path, lines))
	}
	data, err := cfg.contextData(ctx, contextRequest{ID: binding.ProjectID, Budget: budget, Format: "text", Principles: mode})
	return data, warnings, err
}

// isHomeDir reports whether dir is the user's home directory after resolving
// symlinks. An unknown home directory is treated as not matching.
func isHomeDir(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	return filepath.Clean(home) == filepath.Clean(resolved)
}

func (cfg config) principlesFile(ctx context.Context, id string) ([]byte, error) {
	c, err := cfg.client()
	if err != nil {
		return nil, err
	}
	data, err := c.Text(ctx, "/v1/principles", url.Values{"project_id": {id}})
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && !bytes.HasPrefix(data, []byte(rules.Header)) {
		return nil, errors.New("server returned an incompatible principles response; update the herma service")
	}
	return data, nil
}

// backupWarning returns a one-line warning when automatic backups are enabled
// but stale. Any failure to read the status yields no warning, so startup
// context is never blocked by backup reporting.
func backupWarning(ctx context.Context, cfg config) string {
	c, err := cfg.client()
	if err != nil {
		return ""
	}
	data, err := c.Do(ctx, http.MethodGet, "/v1/backup", nil, nil, "")
	if err != nil {
		return ""
	}
	var status backup.Status
	if err := json.Unmarshal(data, &status); err != nil || !status.Enabled || !status.Stale {
		return ""
	}
	message := "herma: backups are enabled but none has succeeded yet"
	if status.LastSuccess != nil {
		message = "herma: no successful backup since " + status.LastSuccess.At.UTC().Format("2006-01-02 15:04 UTC")
	}
	if status.LastError != nil {
		message += " (last error: " + status.LastError.Message + ")"
	}
	return message
}

func hookWarning(stdout io.Writer, message string) error {
	return output(stdout, map[string]string{"systemMessage": message})
}
