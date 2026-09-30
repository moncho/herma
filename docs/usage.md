# Using herma

## Records

| Kind | Purpose | Statuses, with the default first |
| --- | --- | --- |
| `project` | Repository or shared session coordination scope | `planned`, `active`, `paused`, `completed` |
| `task` | Session working intent, ownership and blockers; link the real Linear issue | `open`, `in_progress`, `blocked`, `done` |
| `note` | An immutable handoff: changes, checks, next action and references | `published` |
| `feedback` | Unresolved coordination friction or blocker | `open`, `triaged`, `resolved` |
| `knowledge` | Durable knowledge; reviewed before it counts | `proposed`, `accepted`, `rejected`, `superseded` |
| `principle` | Durable conventions; reviewed before they count | `proposed`, `accepted`, `rejected`, `superseded` |

Records have stable IDs, text bodies, optional project membership, priority
from 0 to 5 (5 is highest), tags, links to other records, and source references.
Self-links are rejected. Links to archived records remain valid because those
records and their histories are still readable. The server records the
authenticated creator/editor, UTC timestamps and version. Every change stores a
complete revision in the same transaction.

## Roles and review

Each identity in `.herma/credentials.json` has a role:

| Role | Can do |
| --- | --- |
| `reviewer` | Everything, including accepting, rejecting and superseding knowledge and principles. Reviewer tokens work only on the local Unix socket. |
| `agent` | Read; write tasks, notes, feedback and projects; propose knowledge and principles and edit them while proposed. |
| `read-only` | Read, search, context and history. No writes. |

Agents create knowledge and principles as `proposed`. Proposed records are found
by `list` and search, but never loaded into session context. To approve one, the
reviewer reads it and updates it at that version:

```sh
./bin/herma review
./bin/herma --identity owner update RECORD_ID --version N --status accepted
```

Accepting requires at least one `--sources` entry. The server records
`reviewed_by` and `reviewed_at`. Agents cannot change accepted, rejected or
superseded records; to improve one, create a new proposed record that links to
it.

Manage identities with `herma identity add NAME [--role agent|read-only|reviewer]`
and `herma identity revoke NAME`. A running server applies changes within about two
seconds, or immediately on `SIGHUP`. The last reviewer cannot be revoked.

The CLI sends reviewer commands over the socket next to the credentials file
(override with `--socket` or `HERMA_SOCKET`) and everything else over TCP.
`herma whoami` shows the identity, role and listener in use. The default identity is
`local-agent`, so commands run without `--identity` can never approve anything.
`herma hook install` and `herma hook session-start` refuse reviewer identities.

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

Global flags (`--url`, `--credentials`, `--identity`, `--socket`) go **before**
the command. Record/filter flags go after it; for updates, flags go after the
record ID. Use `--body-file notes.md` for multiline text, and comma-separated
`--tags`, `--links` and `--sources`. An explicitly empty update flag clears that
field. The API accepts arrays directly when a value itself contains a comma.

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

Other projects, archived records and knowledge/principles are excluded by
default. `context --include-durable` adds accepted knowledge and principles;
proposed, rejected and superseded ones never enter context and are available
through `get`, `list`, search and export. The automatic startup hook always uses
coordination-only context.

## Search and lists

Search uses SQLite FTS5 over title/body. Query words are treated as plain text
and all must match. Matching is by tokenizer terms, not arbitrary substrings:
Chinese or Japanese text without separators can be indexed as a whole run, so
searching for an embedded word may miss it. Language-aware segmentation is not
implemented yet. Filters are combined with AND. Lists default to 50 records
and support up to 200 per page with `--limit` and `--offset`.

See [the agent workflow](agent-workflow.md) for a session handover pattern.
