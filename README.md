# herma

A shared memory service for people and agent sessions, written in Go. It stores
decisions, working principles, projects, tasks, handover notes, and feedback in
SQLite. Sessions use the same authenticated HTTP API through a small JSON CLI.

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

The example creates a tagged demo project, decision and task as `session-a`,
retrieves them as `session-b`, completes the task at its expected version, and
leaves a handover note. It prints the record IDs and authenticated revision
authors. Each run creates a new demo project. Restart persistence and competing
edits are also exercised by the integration tests.

## Records

| Kind | Purpose | Statuses, with the default first |
| --- | --- | --- |
| `knowledge` | Facts, research, decisions, preferences | `proposed`, `accepted`, `superseded` |
| `principle` | Working conventions and project rules | `proposed`, `accepted`, `superseded` |
| `project` | A shared unit of work | `planned`, `active`, `paused`, `completed` |
| `task` | An actionable item | `open`, `in_progress`, `blocked`, `done` |
| `note` | An immutable handover or observation | `published` |
| `feedback` | Friction or problems to triage | `open`, `triaged`, `resolved` |

Records have stable IDs, text bodies, optional project membership, priority
from 0 to 5 (5 is highest), tags, links to other records, and source references.
The server records the authenticated creator/editor, UTC timestamps and version.
Every change stores a complete revision in the same transaction.

`accepted` is an editorial status. All configured identities may set it; it is
**not** evidence of human approval or permission to execute an action.

## Working through the CLI

```sh
# Save the returned project ID for subsequent commands.
./bin/herma create --kind project --title 'Release planning' --status active

# Replace PROJECT_ID with that ID.
./bin/herma --identity session-a create --kind knowledge \
  --project PROJECT_ID --title 'Keep a source for every decision' \
  --body 'Include the evidence or discussion that led to the decision.' \
  --status accepted --tags decisions --sources 'meeting:2026-09-29'

./bin/herma --identity session-a create --kind task \
  --project PROJECT_ID --title 'Write the release checklist' --priority 4 \
  --request-id release-checklist-create-1

# A fresh session receives a bounded context packet.
./bin/herma --identity session-b context --project PROJECT_ID

# Replace TASK_ID and use the version returned by get/context.
./bin/herma --identity session-b update TASK_ID --version 1 \
  --status in_progress --owner session-b --request-id release-checklist-claim-1

./bin/herma history TASK_ID
./bin/herma list --project PROJECT_ID --kind task --status open
./bin/herma list --q 'release checklist' --tag decisions
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

`herma context --project ID` returns:

- The unarchived project.
- Accepted global and project-specific principles and knowledge.
- Project tasks in `open`, `in_progress` or `blocked` state.
- Project notes and unresolved feedback.

Each category includes at most 100 records, ranked by priority and recency. A
`truncated` flag tells the caller to use paginated `list` queries for more detail.
Other projects, archived records and proposed knowledge are excluded.

Search uses SQLite FTS5 over title/body. Query words are treated as plain text
and all must match. Filters are combined with AND. Lists default to 50 records
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
selection; `HERMA_TOKEN` overrides it. Tokens are never included in API responses.
Adding or rotating identities requires restarting the server.

The local default is HTTP on loopback. Put HTTPS and appropriate network access
controls in front of any remote deployment. This first version does not install
hooks, run agents, schedule jobs, or provision a public server.

The service owns one SQLite database on local disk. Writes and history are
transactional, and context/export hold off API writes while taking a consistent
view. Run one service process per database. Do not share the SQLite file over a
network filesystem or open competing service processes against it.

## Export and backup

```sh
./bin/herma export > knowledge-export.json
```

Export includes all records and their revision history, including archived data.
It excludes credentials and idempotency receipts. Treat the export as private.
JSON export is for inspection and portability; there is no JSON import command
yet. The CLI currently caps responses at 16 MiB and the server builds exports in
memory, so large histories need a database backup instead.

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
