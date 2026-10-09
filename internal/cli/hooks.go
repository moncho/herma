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
	"github.com/moncho/herma/plugins"
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
	if *agent == "claude" || *agent == "both" {
		if os.Getenv("HERMA_TOKEN") != "" {
			result["plugin"] = "skipped: the Claude Code plugin needs a credentials file, not HERMA_TOKEN"
		} else {
			installation, err := installClaudePlugin(executable, cfg)
			if err != nil {
				return err
			}
			result["plugin"] = installation
		}
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

// installClaudePlugin writes herma's built-in Claude Code plugin into the data
// folder and registers it in the user's settings, so every session loads
// it; it stays silent where no checkout is bound. HERMA_CLAUDE_PLUGIN_DIR
// registers that folder instead, for working on the plugin in a checkout.
func installClaudePlugin(executable string, cfg config) (hooks.Installation, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return hooks.Installation{}, err
	}
	dir := os.Getenv("HERMA_CLAUDE_PLUGIN_DIR")
	if dir == "" {
		if dir, err = hooks.WritePlugin(cfg.dir, plugins.Claude()); err != nil {
			return hooks.Installation{}, err
		}
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return hooks.Installation{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
		return hooks.Installation{}, fmt.Errorf("Claude Code plugin not found at %s; unset HERMA_CLAUDE_PLUGIN_DIR to use the built-in plugin", dir)
	}
	credentials, err := filepath.Abs(cfg.credentials)
	if err != nil {
		return hooks.Installation{}, err
	}
	return hooks.InstallUserPlugin(home, hooks.UserPlugin{Dir: dir, Binary: executable, Credentials: credentials, Identity: cfg.identity, URL: cfg.endpoint})
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
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	binding, bindingPath, err := project.Discover(event.Cwd)
	bound := err == nil
	if err != nil && !errors.Is(err, project.ErrNoBinding) {
		return hookWarning(stdout, "herma context unavailable: check this repository's .herma-project.json binding.")
	}
	if requireToken && os.Getenv("HERMA_TOKEN") == "" {
		if !bound {
			return nil
		}
		return hookWarning(stdout, "herma context unavailable: this hook requires HERMA_TOKEN in the agent environment; credential-file fallback is disabled.")
	}
	if os.Getenv("HERMA_TOKEN") == "" {
		if role, err := cfg.identityRole(); err == nil && role == store.RoleReviewer {
			if !bound {
				return nil
			}
			return hookWarning(stdout, "herma context unavailable: this hook uses the reviewer identity. Reinstall it with an agent identity: herma --identity local-agent hook install --client claude|codex|both. Session startup will continue.")
		}
	}
	if client == "" {
		if !bound {
			return nil
		}
		return legacySessionStart(ctx, cfg, binding, stdout)
	}
	// Unbound folders get only the global principles, and never a warning.
	global, globalPath, globalWarning := globalPrinciples(ctx, cfg, client)
	if !bound {
		if global == "" {
			return nil
		}
		if client == "claude" && len(global) > claudeContextLimit {
			global = tooLongNote("Global", globalPath)
		}
		return output(stdout, map[string]any{"hookSpecificOutput": map[string]string{
			"hookEventName": "SessionStart", "additionalContext": strings.TrimSpace(global),
		}})
	}
	checkout := filepath.Dir(bindingPath)
	projectSection, projectPath, warnings, err := projectPrinciples(ctx, cfg, client, binding, checkout)
	if err != nil {
		return hookWarning(stdout, "herma context unavailable: check the service (restart herma serve after upgrading herma) and credentials with herma context from this repository. Session startup will continue.")
	}
	summary := coordinationSummary(ctx, cfg, binding.ProjectID)
	if client == "claude" {
		fits := func() bool { return len(joinSections(global, projectSection, summary)) <= claudeContextLimit }
		if !fits() && global != "" {
			global = tooLongNote("Global", globalPath)
		}
		if !fits() && projectSection != "" {
			projectSection = tooLongNote("Project", projectPath)
		}
	}
	if globalWarning != "" {
		warnings = append(warnings, globalWarning)
	}
	if warning := backupWarning(ctx, cfg); warning != "" {
		warnings = append(warnings, warning)
	}
	result := map[string]any{}
	if text := joinSections(global, projectSection, summary); text != "" {
		result["hookSpecificOutput"] = map[string]string{"hookEventName": "SessionStart", "additionalContext": text}
	}
	if len(warnings) > 0 {
		result["systemMessage"] = strings.Join(warnings, "\n")
	}
	if len(result) == 0 {
		return nil
	}
	return output(stdout, result)
}

// legacySessionStart answers a hook installed before --client existed with the
// JSON packet, principles included, and asks for a reinstall.
func legacySessionStart(ctx context.Context, cfg config, binding project.Binding, stdout io.Writer) error {
	// Legacy hooks are usually Claude's; its old 12288 default overflows the cap.
	budget := binding.MaxBytes
	if budget == legacyDefaultMaxBytes {
		budget = claudeContextLimit
	}
	data, err := cfg.contextData(ctx, contextRequest{ID: binding.ProjectID, Budget: budget, Format: "json", Principles: "include"})
	if err != nil {
		return hookWarning(stdout, "herma context unavailable: check the service (restart herma serve after upgrading herma) and credentials with herma context from this repository. Session startup will continue.")
	}
	// The decoded additionalContext string, including its scope/trust metadata,
	// stays within the hook's byte budget. JSON escaping is only transport.
	result := map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "SessionStart", "additionalContext": string(bytes.TrimSpace(data)),
	}}
	warnings := []string{legacyHookWarning}
	if warning := backupWarning(ctx, cfg); warning != "" {
		warnings = append(warnings, warning)
	}
	result["systemMessage"] = strings.Join(warnings, "\n")
	return output(stdout, result)
}

// principlesSection announces principles that changed after the session
// loaded them from path; empty data means none apply any more.
func principlesSection(scope, path string, data []byte) string {
	if len(data) == 0 {
		return "## " + scope + " principles changed\nNo herma " + strings.ToLower(scope) + " principles apply now; disregard those loaded earlier from " + path + ".\n"
	}
	return "## " + scope + " principles changed\nherma " + strings.ToLower(scope) + " principles changed after this session loaded them; this version replaces " + path + ".\n" + string(data)
}

// tooLongNote stands in for a changed section that cannot fit.
func tooLongNote(scope, path string) string {
	return "## " + scope + " principles changed\nherma " + strings.ToLower(scope) + " principles changed after this session loaded them and are too long to repeat here; read " + path + " before relying on them.\n"
}

// joinSections joins the non-empty sections with a blank line.
func joinSections(sections ...string) string {
	var kept []string
	for _, s := range sections {
		if s = strings.TrimSpace(s); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "\n\n")
}

// globalPrinciples syncs the agent's standing global principles: Claude's
// global rules file or the managed block in Codex's AGENTS.md. It returns the
// changed section (or ""), the file path, and a warning meant only for bound
// checkouts; unbound folders stay silent.
func globalPrinciples(ctx context.Context, cfg config, agent string) (section, path, warning string) {
	c, err := cfg.client()
	if err != nil {
		return "", "", ""
	}
	data, err := c.Text(ctx, "/v1/principles", nil)
	if err != nil {
		// Servers before global principles require project_id.
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest {
			return "", "", "herma: global principles unavailable; update the herma service to a version that serves them."
		}
		return "", "", ""
	}
	if len(data) > 0 && !bytes.HasPrefix(data, []byte(rules.Header)) {
		return "", "", "herma: server returned an incompatible principles response; update the herma service."
	}
	var changed bool
	switch agent {
	case "claude":
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", "", ""
		}
		path = filepath.Join(home, rules.GlobalPath)
		changed, err = rules.SyncGlobal(home, data)
	case "codex":
		dir, herr := rules.CodexHome()
		if herr != nil {
			return "", "", ""
		}
		path = filepath.Join(dir, "AGENTS.md")
		changed, err = rules.SyncCodexBlock(dir, data)
	default:
		return "", "", ""
	}
	if err != nil {
		return "", path, "herma: could not update " + path + " (" + err.Error() + "); global principles there may be stale."
	}
	if !changed {
		return "", path, ""
	}
	return principlesSection("Global", path, data), path, ""
}

// projectPrinciples returns the project section: Claude keeps the checkout's
// rules file and hears only of changes; Codex gets the principles inline.
func projectPrinciples(ctx context.Context, cfg config, agent string, binding project.Binding, checkout string) (section, path string, warnings []string, err error) {
	data, err := cfg.principlesFile(ctx, binding.ProjectID)
	if err != nil {
		// Only a project_unavailable error (the project is archived or no
		// longer exists) removes the herma-generated file, so its principles stop
		// applying. Every other failure, including 401, 403, an older server's
		// 404 for the route, 5xx and network errors, keeps the file.
		var apiErr *client.APIError
		if agent == "claude" && errors.As(err, &apiErr) && apiErr.Code == "project_unavailable" {
			_, _ = rules.Sync(checkout, nil)
		}
		return "", "", nil, err
	}
	if agent == "codex" {
		if len(data) == 0 {
			return "", "", nil, nil
		}
		return "## Project principles\n" + string(data), "", nil, nil
	}
	path = filepath.Join(checkout, rules.Path)
	if lines := bytes.Count(data, []byte{'\n'}); lines > rulesLineAdvice {
		warnings = append(warnings, fmt.Sprintf("herma: %s has %d lines; Claude Code follows rules files best under 200 lines. Consider fewer or shorter principles.", rules.Path, lines))
	}
	if isHomeDir(checkout) {
		// Rules under ~/.claude apply to every Claude session, not this project.
		warnings = append(warnings, "herma: this binding is in your home directory, where "+rules.Path+" would apply to every Claude session; principles are in the session context instead.")
		if len(data) == 0 {
			return "", path, warnings, nil
		}
		return "## Project principles\n" + string(data), path, warnings, nil
	}
	changed, serr := rules.Sync(checkout, data)
	if serr != nil {
		// Claude may have loaded a stale file; say these principles replace it.
		warnings = append(warnings, "herma: could not write "+rules.Path+" ("+serr.Error()+"); principles are in the session context instead.")
		return "## Project principles\nherma could not update " + path + "; these principles replace it.\n" + string(data), path, warnings, nil
	}
	if !changed {
		return "", path, warnings, nil
	}
	return principlesSection("Project", path, data), path, warnings, nil
}

// coordinationSummary is the one-line count of open coordination, or "" when
// there is none or the server cannot produce it. Anything else, such as a
// packet from a server that ignores format=summary, is dropped so session
// start never loads record text.
func coordinationSummary(ctx context.Context, cfg config, projectID string) string {
	c, err := cfg.client()
	if err != nil {
		return ""
	}
	data, err := c.Text(ctx, "/v1/context", url.Values{"project_id": {projectID}, "format": {"summary"}})
	if err != nil {
		return ""
	}
	line := string(data)
	if !strings.HasPrefix(line, "herma coordination: ") || strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		return ""
	}
	return line
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
