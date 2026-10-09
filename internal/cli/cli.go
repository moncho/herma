// Package cli implements herma's JSON-oriented command line interface.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/moncho/herma/internal/client"
	"github.com/moncho/herma/internal/store"
)

// defaultIdentity is an agent so that commands run without flags, including
// by agent sessions, can never approve knowledge.
const defaultIdentity = "local-agent"

const usage = `Usage: herma [--url URL] [--credentials PATH] [--identity NAME] [--socket PATH] COMMAND [options]

Global flags must come before the command. Environment defaults:
  HERMA_URL          http://127.0.0.1:8765
  HERMA_DIR          ~/.config/herma; holds credentials, database, socket and plugin
  HERMA_CREDENTIALS  credentials.json in HERMA_DIR
  HERMA_IDENTITY     local-agent
  HERMA_SOCKET       herma.sock next to the credentials file
  HERMA_TOKEN        optional bearer token; replaces credential-file authentication
  HERMA_REVIEWER_CREDENTIALS
                  second credentials file herma serve reads (--reviewer-credentials)

For client commands, HERMA_TOKEN cannot be combined with --identity or a nonempty
HERMA_IDENTITY. Unset HERMA_TOKEN to use a named identity from the credentials file.

Commands:
  init                         Create owner (reviewer) and local-agent credentials without overwriting
  identity add NAME [--role agent|read-only|reviewer] [--client-file PATH]
                               Add credentials; the role defaults to agent. --client-file also
                               writes a file with only this identity for another machine
  identity revoke NAME         Remove an identity; herma serve keeps at least one reviewer
  whoami                       Show the authenticated identity, role and listener
  serve [--db PATH] [--listen HOST:PORT] [--reviewer-credentials PATH] [--backup-dir DIR] [--backup-every 6h] [--backup-keep 14]
  backup status                Show automatic snapshot status
  restore SNAPSHOT|DIR [--db PATH] [--replace]
                               Restore a snapshot with the service stopped
  kind propose NAME --file PATH [--project ID] [--body TEXT]
                               Propose a record kind from a JSON definition
  kind change ID --file PATH   Propose a change to an accepted kind (fields.pending)
  kind list                    List kinds with status and pending changes
  create --kind KIND --title TITLE [--body TEXT | --body-file PATH] [fields]
  list [--project ID] [--kind KIND] [--q TEXT] [--where 'FIELD>=VALUE']... [--sort -FIELD,KEY] [filters]
  get ID
  update ID --version N [fields] [--archived true|false]
  history ID
  project bind --project ID [--dir PATH] [--max-bytes N]
  hook install --client claude|codex|both [--dir CHECKOUT]   Install the user-level session hook; --dir also removes that checkout's old project-level hook
  hook session-start [--client claude|codex]  Load bounded project context for a SessionStart hook
  review [--limit N] [--offset N]  List proposed knowledge, principles and kinds, and pending kind changes
  status                       Binding, identity, review queue and backup age as JSON (for status lines)
  context [--project ID] [--max-bytes N] [--include-durable] [--format text|json] [--principles include|omit]
                               Compact session context (text by default)
  recall "words" [--project ID] [--include-proposed] [--limit N] [--max-bytes N]
                               Search reviewed knowledge and principles by relevance
  principles [--project ID]    Accepted principles as markdown: global, or one project's
  schema
  export

Fields: --title, --body, --body-file, --project, --status, --priority,
        --owner, --tags, --links, --sources, --field NAME=VALUE (repeatable),
        --fields-file PATH. Lists are comma-separated.
The human reviewer accepts or rejects with update ID --version N --status accepted|rejected (--identity owner),
and accepts a kind's pending change with update ID --version N --accept-pending.
Writes accept --request-id KEY for safe retries; write errors include the key used.
Updates send only supplied fields. Record bodies may contain up to 64 KiB of UTF-8.
Use COMMAND --help for command options; for update use update ID --help.
Successful results are JSON on stdout, except context, which prints compact text within its byte budget (--format json for JSON).
herma holds session coordination and reviewed durable knowledge (proposed by
agents, accepted by the reviewer); real tasks stay in your issue tracker. Errors and server logs go to stderr.
`

type config struct {
	endpoint, credentials, identity, socket string
	dir                                     string // data folder; see dataDir
	identitySelected                        bool
}

// dataDir returns the folder holding herma's credentials, database, socket and
// Claude Code plugin: HERMA_DIR, or ~/.config/herma.
func dataDir() (string, error) {
	if dir := os.Getenv("HERMA_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home folder for herma's data (or set HERMA_DIR): %w", err)
	}
	return filepath.Join(home, ".config", "herma"), nil
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func flags(name string, out io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(out)
	return f
}

// Run executes one command. The caller prints returned errors and supplies a
// cancellation context; serve shuts down gracefully when that context ends.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return RunWithInput(ctx, args, os.Stdin, stdout, stderr)
}

// RunWithInput also accepts hook event input, making the same command usable by
// both agent clients without a shell script or another runtime dependency.
func RunWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	err := run(ctx, args, stdin, stdout, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	var cfg config
	var err error
	if cfg.dir, err = dataDir(); err != nil {
		return err
	}
	f := flags("herma", stderr)
	f.StringVar(&cfg.endpoint, "url", envDefault("HERMA_URL", "http://127.0.0.1:8765"), "knowledge base server URL")
	f.StringVar(&cfg.credentials, "credentials", envDefault("HERMA_CREDENTIALS", filepath.Join(cfg.dir, "credentials.json")), "credentials JSON path")
	f.StringVar(&cfg.identity, "identity", envDefault("HERMA_IDENTITY", defaultIdentity), "authenticated identity")
	f.StringVar(&cfg.socket, "socket", envDefault("HERMA_SOCKET", ""), "Unix socket path; defaults to herma.sock next to the credentials file")
	f.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := f.Parse(args); err != nil {
		return err
	}
	cfg.identitySelected = os.Getenv("HERMA_IDENTITY") != ""
	f.Visit(func(f *flag.Flag) {
		if f.Name == "identity" {
			cfg.identitySelected = true
		}
	})
	args = f.Args()
	if len(args) == 0 || args[0] == "help" {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	command, rest := args[0], args[1:]
	switch command {
	case "init":
		if err := noOptions(command, rest, stderr); err != nil {
			return err
		}
		if err := initCredentials(cfg.credentials); err != nil {
			return err
		}
		return output(stdout, map[string]string{"status": "created", "reviewer": "owner", "agent": "local-agent", "credentials": cfg.credentials})
	case "identity":
		return identityCommand(cfg, rest, stdout, stderr)
	case "whoami":
		if err := noOptions(command, rest, stderr); err != nil {
			return err
		}
		return cfg.request(ctx, stdout, http.MethodGet, "/v1/whoami", nil, nil, "")
	case "backup":
		return backupCommand(ctx, cfg, rest, stdout, stderr)
	case "restore":
		return restore(cfg, rest, stdout, stderr)
	case "serve":
		return serve(ctx, cfg, rest, stderr)
	case "create":
		return create(ctx, cfg, rest, stdout, stderr)
	case "update":
		return update(ctx, cfg, rest, stdout, stderr)
	case "list":
		return list(ctx, cfg, rest, stdout, stderr)
	case "project":
		return bindProject(ctx, cfg, rest, stdout, stderr)
	case "kind":
		return kindCommand(ctx, cfg, rest, stdout, stderr)
	case "review":
		return review(ctx, cfg, rest, stdout, stderr)
	case "status":
		return statusCommand(ctx, cfg, rest, stdout, stderr)
	case "hook":
		return hook(ctx, cfg, rest, stdin, stdout, stderr)
	case "get", "history":
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return fmt.Errorf("usage: herma [global flags] %s ID", command)
		}
		path := "/v1/records/" + url.PathEscape(rest[0])
		if command == "history" {
			path += "/history"
		}
		return cfg.request(ctx, stdout, http.MethodGet, path, nil, nil, "")
	case "context":
		return projectContextCommand(ctx, cfg, rest, stdout, stderr)
	case "recall":
		return recallCommand(ctx, cfg, rest, stdout, stderr)
	case "principles":
		return principlesCommand(ctx, cfg, rest, stdout, stderr)
	case "schema", "export":
		if err := noOptions(command, rest, stderr); err != nil {
			return err
		}
		return cfg.request(ctx, stdout, http.MethodGet, "/v1/"+command, nil, nil, "")
	default:
		return fmt.Errorf("unknown command %q; run herma help", command)
	}
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; command flags must follow the command and global flags must precede it", fs.Arg(0))
	}
	return nil
}

func noOptions(name string, args []string, stderr io.Writer) error {
	return parse(flags(name, stderr), args)
}

func output(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func (cfg config) request(ctx context.Context, stdout io.Writer, method, path string, query url.Values, input any, requestID string) error {
	c, err := cfg.client()
	if err != nil {
		return err
	}
	if (method == http.MethodPost || method == http.MethodPatch) && requestID == "" {
		requestID, err = randomID()
		if err != nil {
			return err
		}
	}
	data, err := c.Do(ctx, method, path, query, input, requestID)
	if err != nil {
		return writeError(err, requestID)
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, data, "", "  "); err != nil {
		return err
	}
	formatted.WriteByte('\n')
	_, err = stdout.Write(formatted.Bytes())
	return writeError(err, requestID)
}

// socketPath is shared by herma serve and reviewer clients so that both find the
// same socket beside the credentials file by default.
func (cfg config) socketPath() (string, error) {
	path := cfg.socket
	if path == "" {
		path = filepath.Join(filepath.Dir(cfg.credentials), "herma.sock")
	}
	return filepath.Abs(path)
}

func (cfg config) client() (*client.Client, error) {
	token := os.Getenv("HERMA_TOKEN")
	if token != "" && cfg.identitySelected {
		return nil, errors.New("HERMA_TOKEN cannot be combined with --identity or HERMA_IDENTITY; unset HERMA_TOKEN to use a named identity, or remove the identity settings to use the token")
	}
	if token == "" {
		credentials, err := loadCredentials(cfg.credentials)
		if err != nil {
			return nil, err
		}
		entry, ok := credentials[cfg.identity]
		if !ok {
			return nil, fmt.Errorf("identity %q is not in the credentials file", cfg.identity)
		}
		if entry.Role == store.RoleReviewer {
			socket, err := cfg.socketPath()
			if err != nil {
				return nil, err
			}
			return client.NewSocket(socket, entry.Token)
		}
		token = entry.Token
	}
	return client.New(cfg.endpoint, token)
}

func writeError(err error, requestID string) error {
	if err != nil && requestID != "" {
		return fmt.Errorf("%w; request ID %q (reuse with --request-id and the same arguments when retrying this write)", err, requestID)
	}
	return err
}

type fields struct {
	title, body, bodyFile, project, status, owner, tags, links, sources, requestID string
	priority                                                                       int
	fieldValues                                                                    fieldFlags
	fieldsFile                                                                     string
}

// repeatedFlag collects every value of a repeatable string flag.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ", ") }

func (r *repeatedFlag) Set(value string) error {
	*r = append(*r, value)
	return nil
}

// fieldFlags collects repeated --field name=value flags. An empty value
// removes the field on update.
type fieldFlags map[string]any

func (f *fieldFlags) String() string { return "" }

func (f *fieldFlags) Set(value string) error {
	name, v, ok := strings.Cut(value, "=")
	if !ok || name == "" {
		return errors.New("--field must be name=value")
	}
	if *f == nil {
		*f = fieldFlags{}
	}
	if v == "" {
		(*f)[name] = nil
	} else {
		(*f)[name] = v
	}
	return nil
}

func (v *fields) register(fs *flag.FlagSet) {
	fs.StringVar(&v.title, "title", "", "record title")
	fs.StringVar(&v.body, "body", "", "record text, including multiline text")
	fs.StringVar(&v.bodyFile, "body-file", "", "read UTF-8 body from this file")
	fs.StringVar(&v.project, "project", "", "project record ID; create defaults to the binding for tasks, feedback, notes and custom kinds; empty clears it on update")
	fs.StringVar(&v.status, "status", "", "record status; see herma schema")
	fs.IntVar(&v.priority, "priority", 0, "record priority; see herma schema")
	fs.StringVar(&v.owner, "owner", "", "task owner; empty clears it on update")
	fs.StringVar(&v.tags, "tags", "", "comma-separated tags; empty clears them on update")
	fs.StringVar(&v.links, "links", "", "comma-separated related record IDs")
	fs.StringVar(&v.sources, "sources", "", "comma-separated source references or URLs")
	fs.StringVar(&v.requestID, "request-id", "", "idempotency key; defaults to a new random key")
	fs.Var(&v.fieldValues, "field", "typed field as name=value (repeatable); lists are comma-separated; name= removes it on update")
	fs.StringVar(&v.fieldsFile, "fields-file", "", "JSON object of typed fields (create) or a field patch (update)")
}

// readFields returns the typed fields from --field or --fields-file.
func (v *fields) readFields(set map[string]bool) (map[string]any, error) {
	if set["field"] && set["fields-file"] {
		return nil, errors.New("use either --field or --fields-file, not both")
	}
	if set["fields-file"] {
		data, err := os.ReadFile(v.fieldsFile)
		if err != nil {
			return nil, fmt.Errorf("read fields file: %w", err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
			return nil, errors.New("fields file must hold one JSON object")
		}
		return fields, nil
	}
	if len(v.fieldValues) == 0 {
		return nil, nil
	}
	return map[string]any(v.fieldValues), nil
}

func supplied(fs *flag.FlagSet) map[string]bool {
	result := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { result[f.Name] = true })
	return result
}

func (v *fields) readBody(set map[string]bool) error {
	if set["body-file"] {
		if set["body"] {
			return errors.New("use either --body or --body-file, not both")
		}
		f, err := os.Open(v.bodyFile)
		if err != nil {
			return fmt.Errorf("open body file: %w", err)
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
		if err != nil {
			return fmt.Errorf("read body file: %w", err)
		}
		v.body = string(data)
	}
	if len(v.body) > 64<<10 || !utf8.ValidString(v.body) {
		return errors.New("body must be valid UTF-8 and at most 64 KiB")
	}
	return nil
}

func csv(value string) []string {
	result := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func create(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	fs := flags("create", stderr)
	v := fields{}
	v.register(fs)
	kind := fs.String("kind", "", "record kind (required); see herma schema")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *kind == "" || v.title == "" {
		return errors.New("create requires --kind and --title")
	}
	if err := v.readBody(supplied(fs)); err != nil {
		return err
	}
	fieldValues, err := v.readFields(supplied(fs))
	if err != nil {
		return err
	}
	for name, value := range fieldValues {
		if value == nil {
			return fmt.Errorf("--field %s= has no value; omit the field on create", name)
		}
	}
	if !supplied(fs)["project"] && store.ProjectByDefault(*kind) {
		if v.project, err = boundProject(); err != nil {
			return err
		}
	}
	input := store.CreateInput{Kind: *kind, Title: v.title, Body: v.body, ProjectID: v.project, Status: v.status, Priority: v.priority, Owner: v.owner, Tags: csv(v.tags), Links: csv(v.links), Sources: csv(v.sources), Fields: fieldValues}
	return cfg.request(ctx, stdout, http.MethodPost, "/v1/records", nil, input, v.requestID)
}

func update(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: herma [global flags] update ID --version N [fields]")
	}
	id := args[0]
	fs := flags("update", stderr)
	v := fields{}
	v.register(fs)
	version := fs.Int64("version", 0, "current record version (required)")
	archived := fs.String("archived", "", "true to archive, false to restore")
	acceptPending := fs.Bool("accept-pending", false, "reviewer: accept a kind's pending definition change")
	if err := parse(fs, args[1:]); err != nil {
		return err
	}
	set := supplied(fs)
	if !set["version"] || *version < 1 {
		return errors.New("update requires --version with the record's current positive version")
	}
	if err := v.readBody(set); err != nil {
		return err
	}
	input := store.UpdateInput{Version: *version}
	if set["title"] {
		input.Title = &v.title
	}
	if set["body"] || set["body-file"] {
		input.Body = &v.body
	}
	if set["project"] {
		input.ProjectID = &v.project
	}
	if set["status"] {
		input.Status = &v.status
	}
	if set["priority"] {
		input.Priority = &v.priority
	}
	if set["owner"] {
		input.Owner = &v.owner
	}
	if set["tags"] {
		value := csv(v.tags)
		input.Tags = &value
	}
	if set["links"] {
		value := csv(v.links)
		input.Links = &value
	}
	if set["sources"] {
		value := csv(v.sources)
		input.Sources = &value
	}
	if set["archived"] {
		if *archived != "true" && *archived != "false" {
			return errors.New("--archived must be true or false")
		}
		value := *archived == "true"
		input.Archived = &value
	}
	if set["field"] || set["fields-file"] {
		patch, err := v.readFields(set)
		if err != nil {
			return err
		}
		input.Fields = patch
	}
	if *acceptPending {
		input.AcceptPending = true
	}
	mutable := false
	for name := range set {
		if name != "version" && name != "request-id" {
			mutable = true
		}
	}
	if !mutable {
		return errors.New("update requires at least one field to change")
	}
	return cfg.request(ctx, stdout, http.MethodPatch, "/v1/records/"+url.PathEscape(id), nil, input, v.requestID)
}

func list(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	fs := flags("list", stderr)
	stringsByQuery := make(map[string]*string)
	for _, key := range []string{"kind", "status", "owner", "tag", "q"} {
		stringsByQuery[key] = fs.String(key, "", "filter by "+key)
	}
	stringsByQuery["project_id"] = fs.String("project", "", "project record ID")
	global := fs.Bool("global", false, "only records without a project")
	archived := fs.Bool("include-archived", false, "include archived records")
	limit := fs.Int("limit", 50, "maximum records to return (1–200)")
	offset := fs.Int("offset", 0, "number of records to skip")
	var where repeatedFlag
	fs.Var(&where, "where", "field filter such as 'rating>=4' (repeatable; requires --kind); quote it in the shell")
	sortSpec := fs.String("sort", "", "sort keys such as -read_at,title (up to 3; '-' for descending)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *global && *stringsByQuery["project_id"] != "" {
		return errors.New("--global and --project cannot be combined")
	}
	if *limit < 1 || *limit > 200 || *offset < 0 {
		return errors.New("--limit must be 1–200 and --offset must be nonnegative")
	}
	query := url.Values{"limit": {strconv.Itoa(*limit)}, "offset": {strconv.Itoa(*offset)}}
	for key, value := range stringsByQuery {
		if *value != "" {
			query.Set(key, *value)
		}
	}
	if len(where) > 0 {
		query["where"] = where
	}
	if *sortSpec != "" {
		query.Set("sort", *sortSpec)
	}
	if *global {
		query.Set("global", "true")
	}
	if *archived {
		query.Set("include_archived", "true")
	}
	return cfg.request(ctx, stdout, http.MethodGet, "/v1/records", query, nil, "")
}

func identityCommand(cfg config, args []string, stdout, stderr io.Writer) error {
	usage := errors.New("usage: herma [global flags] identity add NAME [--role agent|read-only|reviewer] [--client-file PATH] | identity revoke NAME")
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		return usage
	}
	name := args[1]
	switch args[0] {
	case "add":
		fs := flags("identity add", stderr)
		role := fs.String("role", string(store.RoleAgent), "reviewer, agent, or read-only")
		clientFile := fs.String("client-file", "", "also write a new credentials file holding only this identity, for a client on another machine")
		if err := parse(fs, args[2:]); err != nil {
			return err
		}
		if err := addIdentity(cfg.credentials, name, store.Role(*role), *clientFile); err != nil {
			return err
		}
		result := map[string]string{"status": "created", "identity": name, "role": *role, "credentials": cfg.credentials, "message": "The running server loads new identities within a few seconds."}
		if *clientFile != "" {
			result["client_file"] = *clientFile
			result["message"] += " Move the client file to the other machine privately and delete it here."
		}
		return output(stdout, result)
	case "revoke":
		if len(args) != 2 {
			return usage
		}
		lastReviewer, err := revokeIdentity(cfg.credentials, name)
		if err != nil {
			return err
		}
		message := "The running server stops accepting this token within a few seconds."
		if lastReviewer {
			message = "This file has no reviewer left. The running server applies the change only if another credentials file it reads (--reviewer-credentials) holds a reviewer; otherwise it keeps the previous identities and logs why."
		}
		return output(stdout, map[string]string{"status": "revoked", "identity": name, "credentials": cfg.credentials, "message": message})
	default:
		return usage
	}
}

// identityRole returns the role of the selected named identity.
func (cfg config) identityRole() (store.Role, error) {
	credentials, err := loadCredentials(cfg.credentials)
	if err != nil {
		return "", err
	}
	entry, ok := credentials[cfg.identity]
	if !ok {
		return "", fmt.Errorf("identity %q is not in the credentials file", cfg.identity)
	}
	return entry.Role, nil
}
