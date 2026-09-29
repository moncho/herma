# herma

A coordination and handoff service for agent sessions working on the same
project, written in Go. It answers: who is working where, what is blocked, what
changed, and what should the next session know? Sessions share an authenticated
HTTP API and a small CLI, backed by SQLite.

Durable knowledge belongs in memory files and the wiki. Real tasks and their
status stay in Linear. herma stores short-lived working intent and handoffs, with
links to those sources, rather than duplicating them.

This is a new, independent project inspired by the
[external knowledge base described in The Ground Truth](https://thegroundtruth.media/i/217789967/external-knowledge-base).

## Start locally

Requires **Go 1.26 or later**. The SQLite driver is pure Go; no C compiler,
separate database server, model API key, or hosted account is required.

Run these commands from this project's directory:

```sh
go build -trimpath -buildvcs=false -o bin/herma ./cmd/herma
./bin/herma init
./bin/herma identity add session-a
./bin/herma identity add session-b
./bin/herma serve
```

The service listens on `127.0.0.1:8765`. Initialization creates a private
`.herma/credentials.json` file without overwriting an existing one. The database
lives at `.herma/knowledge.sqlite3`. Keep the server terminal running and use
another terminal in the same directory for client commands.

### Keeping the local service running on macOS

Once credentials have been initialized, use these commands to run the service
independently of a terminal or chat. Stop any foreground `herma serve` process
before switching to the background service.

```sh
make service-start
make service-status
./bin/herma list
```

macOS manages the process and restarts it if it fails. Logs are in
`.herma/server.log`. To stop it, run `make service-stop`; to reload credentials or
deploy a rebuilt binary, stop it and run `make service-start` again. This job is
registered for the current login session, so run `make service-start` again after
logging out or rebooting. It does not install an automatic login item.

If a client reports `connection refused`, no service is accepting connections at
that address. Start it with `make service-start` on macOS, or keep
`./bin/herma serve` running in another terminal. The database and credentials remain
on disk when the server stops; do not reinitialize them.

### Try the API and example

```sh
./bin/herma schema
./bin/herma list
go run -buildvcs=false ./examples/handover
```

The example creates a demo project, handoff and coordination record as `session-a`,
retrieves them as `session-b`, completes the coordination record at its expected version, and
leaves a handover note. It prints the record IDs and authenticated revision
authors. Each run creates a new demo project. Restart persistence and competing
edits are also exercised by the integration tests.

## Automatically load context in each session

Create one herma project for the repository, then bind it from the repository root.
Use an absolute path to your built herma executable and credentials if they live
elsewhere. Replace `PROJECT_ID` with the ID returned by the first command:

```sh
herma create --kind project --title 'My repository' --status active
herma project bind --project PROJECT_ID --max-bytes 12288
herma --credentials /absolute/path/credentials.json --identity session-a \
  hook install --client both
```

If the service's credentials live elsewhere, set `HERMA_CREDENTIALS` to their
absolute path before the create/bind commands too. `herma` above means the built
executable on your PATH; otherwise use its full path.
Install hooks using a built executable, not `go run`: the hook must be able to
find the same executable in future sessions.

Use `--client claude` or `--client codex` to install for one client. Installation
merges a synchronous SessionStart hook into `.claude/settings.local.json` and/or
`.codex/hooks.json`, preserving existing settings and other hooks. Repeating
installation updates herma's own hook without duplicating it. Commands capture the
executable, service URL and credential-file path, never a token. Keep these
machine-specific hook files local; the generated `.herma-project.json` contains only
the project ID and budget and can be committed for other sessions/worktrees.

In Codex, review and trust the installed hook through `/hooks`; the project must
also be trusted. This is Codex's normal hook setup requirement. Start a new
session after installation. The hook runs on every SessionStart, including
startup, resume, clear, compaction and Claude session forks. See the official [Codex hook documentation](https://learn.chatgpt.com/docs/hooks)
and [Claude Code hook documentation](https://code.claude.com/docs/en/hooks).

The hook discovers the nearest `.herma-project.json` using the session's actual
working directory. Nested folders work; lookup stops at a Git repository or
worktree boundary. Track the binding in each worktree where it is needed, and
install local hooks there. An invalid nearby binding produces a warning instead
of silently falling back. To change a binding, edit its project ID/budget
explicitly; `project bind` will not overwrite a different existing configuration.

The startup hook is read-only, has a five-second request deadline, and continues
with a short warning if the service, credentials or binding are unavailable.
Unbound projects are a quiet no-op. Token-based installs inherit `HERMA_TOKEN` from
the agent environment and warn if it is missing, without falling back to another
identity. Named-identity installs require `HERMA_TOKEN` to be unset.
No MCP server is needed for this automatic loading path.

After binding, `herma context` works without repeating the project ID. Context
refreshes at session boundaries; run it again during a long session before
coordinating an edit with another agent.

## Records

| Kind | Purpose | Statuses, with the default first |
| --- | --- | --- |
| `project` | Repository or shared session coordination scope | `planned`, `active`, `paused`, `completed` |
| `task` | Session working intent, ownership and blockers; link the real Linear issue | `open`, `in_progress`, `blocked`, `done` |
| `note` | An immutable handoff: changes, checks, next action and references | `published` |
| `feedback` | Unresolved coordination friction or blocker | `open`, `triaged`, `resolved` |
| `knowledge` | Legacy durable content; excluded from default context | `proposed`, `accepted`, `superseded` |
| `principle` | Legacy conventions; excluded from default context | `proposed`, `accepted`, `superseded` |

Records have stable IDs, text bodies, optional project membership, priority
from 0 to 5 (5 is highest), tags, links to other records, and source references.
Self-links are rejected. Links to archived records remain valid because those
records and their histories are still readable.
The server records the authenticated creator/editor, UTC timestamps and version.
Every change stores a complete revision in the same transaction.

`accepted` is an editorial status. All configured identities may set it; it is
**not** evidence of human approval or permission to execute an action.

## Working through the CLI

```sh
# Save the returned project ID for subsequent commands.
./bin/herma create --kind project --title 'Repository sessions' --status active

# Replace PROJECT_ID with that ID.
./bin/herma --identity session-a create --kind task \
  --project PROJECT_ID --title 'Session A is editing the API' --priority 4 \
  --body 'Working in internal/api; please coordinate overlapping edits here.' \
  --status in_progress --owner session-a --sources LINEAR_ISSUE_URL \
  --request-id api-session-a-start-1

# A fresh session receives a bounded context packet.
./bin/herma --identity session-b context --project PROJECT_ID

# Leave a handoff; use returned coordination IDs in --links.
./bin/herma --identity session-a create --kind note \
  --project PROJECT_ID --title 'API handoff' --tags handoff \
  --body 'Changed request validation. Race tests pass. Next: review the retry path.' \
  --links COORDINATION_ID --sources LINEAR_ISSUE_URL,COMMIT_URL

./bin/herma history COORDINATION_ID
./bin/herma list --project PROJECT_ID --kind task --status open
./bin/herma list --project PROJECT_ID --kind note --tag handoff
```

Global flags (`--url`, `--credentials`, `--identity`) go **before** the command.
Record/filter flags go after it; for updates, flags go after the record ID.
Use `--body-file notes.md` for multiline text, and comma-separated `--tags`,
`--links` and `--sources`. An explicitly empty update flag clears that field.
The API accepts arrays directly when a value itself contains a comma.

Commands return JSON to standard output and errors to standard error with a
nonzero exit code. `herma help` lists all commands, and `herma schema` describes the
live API, record fields, filters and limits.

### Conflicts, retries and archival

An update must include the version you read. A stale update returns HTTP 409
without changing the record. Fetch the current record, reconcile your changes,
then submit the new version. Do not blindly overwrite another session's work.

Use the **same** `--request-id` to retry an uncertain write with identical input.
The server returns the original result without duplicating it, even after a
restart. Reusing that key with different input returns 409. Keys are scoped to
the authenticated identity and cover both creates and updates. The CLI generates
a key if none is supplied; use explicit keys in resumable agent workflows.

```sh
./bin/herma update RECORD_ID --version 3 --archived true
./bin/herma list --include-archived
./bin/herma update RECORD_ID --version 4 --archived false
```

Notes allow archival/restoration only. To correct a note, append a new note
linking to the earlier record. Archived records remain readable by ID and in
history and export. There is no hard-delete endpoint.

## Session context

`herma context` (or `herma context --project ID`) returns:

- The unarchived project.
- Session coordination records (`task`) in `open`, `in_progress` or `blocked` state.
- Recent handoff notes and unresolved coordination feedback.

The entire compact JSON response, including metadata and newline, is capped at
**12 KiB by default**. Use `--max-bytes N` (2–64 KiB), or set `max_bytes` in the
project binding. The API accepts the same `max_bytes` query parameter. This is an
exact byte budget, not an estimated token count; CLI formatting does not expand it.
Each individual record preview uses at most 2 KiB or a quarter of the packet
budget, whichever is smaller, so one long body cannot crowd out every other item.

Coordination records and blockers take priority over handoff notes; recent notes
come before optional durable content. Context contains summaries: `truncated`,
per-category `omitted` counts, `body_truncated` and `truncated_fields` report what
was clipped or left out. Use `get ID` to retrieve a complete, fresh record before
editing, or filtered/paginated `list` for omitted items. A 100-record candidate
limit per category also bounds query work; it does not define the text budget.

Other projects, archived records and durable knowledge/principles are excluded
by default. Existing durable data is preserved and remains available through
`get`, `list`, export, or explicit `context --include-durable`. The automatic
startup hook always uses coordination-only context.

Search uses SQLite FTS5 over title/body. Query words are treated as plain text
and all must match. Matching is by tokenizer terms, not arbitrary substrings:
Chinese or Japanese text without separators can be indexed as a whole run, so
searching for an embedded word may miss it. Language-aware segmentation is not
implemented yet. Filters are combined with AND. Lists default to 50 records
and support up to 200 per page with `--limit` and `--offset`.

See [the agent workflow](docs/agent-workflow.md) for a session handover pattern.

## Identity and deployment boundary

Each bearer token maps to one named writer. Source references and task ownership
are separate from authenticated authorship. All identities currently share the
same read/write access: this is one trusted user's knowledge base, not a
multi-tenant service.

For a remote client, set `HERMA_URL` and `HERMA_TOKEN` to that session's endpoint and
individual token. Do not distribute the server's complete credentials file to
remote agents. `HERMA_CREDENTIALS` and `HERMA_IDENTITY` configure local credential-file
selection. Use `HERMA_TOKEN` alone for token authentication. Combining it with an
explicit `--identity` or a nonempty `HERMA_IDENTITY` is rejected before a request is
sent, so the token cannot silently change the selected writer. For local named
identities, use `env -u HERMA_TOKEN ./bin/herma --identity NAME ...`. Tokens are never
included in API responses.
Adding or rotating identities requires restarting the server.

If an interrupted `identity add` leaves a lock behind, the error names the exact
lock file. Confirm no other `herma identity add` is running before removing that
file and retrying; do not remove an active writer's lock.

The local default is HTTP on loopback. Put HTTPS and appropriate network access
controls in front of any remote deployment. The service does not run agents,
schedule jobs, or provision a public server.

The service owns one SQLite database on local disk. Writes and history are
transactional, and context/export hold off API writes while reading a consistent
snapshot. Request-body reads, response encoding and network writes happen
outside that lock, so a stalled client does not freeze other sessions. Run one
service process per database. Do not share the SQLite file over a
network filesystem or open competing service processes against it.

## Export and backup

```sh
./bin/herma export > .herma/knowledge-export.json.tmp &&
  mv .herma/knowledge-export.json.tmp .herma/knowledge-export.json
```

Export includes all records and their revision history, including archived data.
It excludes credentials and idempotency receipts. Treat the export as private.
JSON export is for inspection and portability; there is no JSON import command
yet. The command above replaces the saved export only after a successful
download. The CLI buffers and validates the complete response before writing
stdout, so a truncated transfer fails without printing a partial export.

The server builds exports in memory and reads each record's history separately.
It rejects JSON exports larger than 16 MiB with `export_too_large` (HTTP 413) and
limits snapshot preparation to 20 seconds, reporting `export_timeout` (HTTP 503)
if that budget expires. Successful responses include their exact Content-Length,
so clients can detect an interrupted download. A slow network can still hit the
server's 30-second write timeout; streaming is deferred, and large histories
should use a database backup instead.

For a restorable backup, stop the service cleanly, then copy the entire private
`.herma` directory to a protected location. Restore that directory with the service
stopped and preserve its private permissions. Do not copy only the database file
while the service is running: uncheckpointed changes may still be in its WAL.

## Verification

```sh
go test -race ./...
go vet ./...
```

Tests cover the full handover after restart, authenticated provenance, competing
updates, idempotent retries, transaction rollback, project context isolation,
search, validation, archival, client behavior, and credential handling.

See [design notes](docs/design.md) for the package structure and current limits.
