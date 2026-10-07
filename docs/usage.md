# Using herma

## Records

| Kind | Purpose | Statuses, with the default first |
| --- | --- | --- |
| `project` | Repository or shared session coordination scope | `planned`, `active`, `paused`, `completed` |
| `task` | Session working intent, ownership and blockers; link the real issue | `open`, `in_progress`, `blocked`, `done` |
| `note` | An immutable handoff: changes, checks, next action and references | `published` |
| `feedback` | Unresolved coordination friction or blocker | `open`, `triaged`, `resolved` |
| `knowledge` | Durable knowledge; reviewed before it counts | `proposed`, `accepted`, `rejected`, `superseded` |
| `principle` | Durable conventions; reviewed before they count | `proposed`, `accepted`, `rejected`, `superseded` |
| `kind` | A record kind definition: typed fields, statuses and policy | `proposed`, `accepted`, `retired` |

Records have stable IDs, text bodies, optional project membership, priority
from 0 to 5 (5 is highest), tags, links to other records, and source references.
Self-links are rejected. Links to archived records remain valid because those
records and their histories are still readable. The server records the
authenticated creator/editor, UTC timestamps and version. Every change stores a
complete revision in the same transaction.

## Roles and review

Each identity in `credentials.json` has a role:

| Role | Can do |
| --- | --- |
| `reviewer` | Everything, including accepting, rejecting and superseding knowledge and principles. Reviewer tokens work only on the local Unix socket. |
| `agent` | Read; write tasks, notes, feedback, projects and records of accepted custom kinds; propose knowledge, principles and kinds and edit them while proposed. |
| `read-only` | Read, search, context and history. No writes. |

Agents create knowledge and principles as `proposed`. Proposed records are found
by `list` and search, but never loaded into session context. To approve one, the
reviewer reads it and updates it at that version:

```sh
herma review
herma --identity owner update RECORD_ID --version N --status accepted
```

Kind definitions follow `proposed`, `accepted`, `retired`, and only the reviewer
moves them (see Custom kinds). `herma review` lists proposed kinds under `kinds` and
accepted kinds with a pending change under `pending_kind_changes`.

Accepting requires at least one `--sources` entry. The server records
`reviewed_by` and `reviewed_at`. Agents cannot change accepted, rejected or
superseded records; to improve one, create a new proposed record that links to
it.

Manage identities with `herma identity add NAME [--role agent|read-only|reviewer]`
and `herma identity revoke NAME`. A running server applies changes within about two
seconds, or immediately on `SIGHUP`. The server always keeps at least one
reviewer: it rejects a change that would leave none and keeps the previous
identities.

The CLI sends reviewer commands over the socket next to the credentials file
(override with `--socket` or `HERMA_SOCKET`) and everything else over TCP.
`herma whoami` shows the identity, role and listener in use. The default identity is
`local-agent`, so commands run without `--identity` can never approve anything.
`herma hook install` and `herma hook session-start` refuse reviewer identities.

## Custom kinds

Beyond the six built-in coordination and memory kinds and the built-in kind
`kind`, an agent can propose a new kind and you accept it.
A kind is a definition file in JSON. This one describes a bookmark log:

```json
{
  "schema": {
    "url":      {"type": "url", "required": true, "unique": true},
    "platform": {"type": "enum", "values": ["web", "x", "youtube", "arxiv"]},
    "read_at":  {"type": "date"},
    "rating":   {"type": "integer", "min": 1, "max": 5},
    "authors":  {"type": "string-list"}
  },
  "statuses": ["unread", "reading", "done"],
  "policy": {"review": false, "writers": "agent", "recall": true}
}
```

Field types:

| Type | Value |
| --- | --- |
| `string` | One line, 1-300 characters |
| `text` | Up to 16 KiB |
| `integer`, `number` | Optional `min` and `max` |
| `boolean` | `true` or `false` |
| `date` | `YYYY-MM-DD` |
| `datetime` | RFC 3339, stored in UTC |
| `url` | Absolute `http` or `https` URL, up to 2,048 bytes |
| `enum` | One of `values` (1-100 names) |
| `string-list` | Up to 50 distinct strings, each 1-300 characters |

Any field may set `required: true`. A `string`, `url`, `integer`, `date` or
`datetime` field may set `unique: true`. A schema has at most 30 fields and the
definition is at most 16 KiB.

Policy keys:

| Key | Meaning |
| --- | --- |
| `review` | `true` gives records the `proposed`, `accepted`, `rejected`, `superseded` lifecycle, judged only by the reviewer, and the definition must not list `statuses`. `false` requires `statuses` (1-20 names); the first is the default. |
| `writers` | `agent` or `reviewer`: the lowest role that may create and update records. |
| `recall` | Whether `herma recall` searches the kind. |
| `context` | Optional `{"statuses": [...], "order": "priority"\|"recent", "max_records": N}` (1-20) to list records in session context. |

### Lifecycle

```sh
# Agent: propose the kind. NAME matches ^[a-z][a-z0-9_]{0,39}$; built-in names
# and the reserved name custom are refused.
herma kind propose bookmark --file def.json --body 'Links worth reading later.'

# Reviewer: see it under "kinds" in the queue, then accept it.
herma --identity owner review
herma --identity owner update KIND_ID --version N --status accepted

# Agent: propose a change to the accepted kind (a full replacement document).
herma kind change KIND_ID --file def2.json

# Reviewer: "pending_kind_changes" in the queue lists these. Apply one.
herma --identity owner update KIND_ID --version N --accept-pending

# Reviewer: stop new records; existing ones stay readable and editable.
herma --identity owner update KIND_ID --version N --status retired
```

`herma kind list` shows the kinds. A kind is `proposed`, `accepted` or `retired`;
you can move a retired kind back to `accepted`. Only a proposed kind can be
archived. The name and project never change. A kind may be tied to a project
with `--project ID`, and then its records must belong to that project; otherwise
it is global. No records can be written until the kind is accepted.

On an accepted kind, agents change only `fields.pending`. `--accept-pending`
applies it in one transaction. Adding optional fields, enum values or statuses,
loosening limits, or changing `recall`, `context` or `writers` is accepted
directly. For any other change, the server checks every live record of the kind
against the new definition and refuses, naming up to five, if any would break.
A change of `review` is refused while any record of the kind exists, archived
ones included.

### Writing records

```sh
herma create --kind bookmark --title 'Raft paper' \
  --field url=https://raft.github.io/raft.pdf --field rating=5 \
  --field authors='Ongaro,Ousterhout'
herma update RECORD_ID --version 2 --field rating=4 --field read_at=
```

`--field name=value` is repeatable. On update, `name=` removes the field; on
create it is refused. `--fields-file path.json` takes a JSON object instead and
cannot be combined with `--field`. On the command line, `integer`, `number` and
`boolean` fields accept their string forms, and a `string-list` accepts one
comma-separated string; a plain `string` keeps its commas. Through the API,
`fields` is a JSON object, and on update it is a patch: a value sets a field,
`null` removes it and absent keys stay as they are.

A `unique` field refuses a second unarchived record with the same value:

```json
{"error":{"code":"duplicate","message":"fields.url: already used by rec_…","field":"url","existing_id":"rec_…"}}
```

This is HTTP 409. Archived records do not count, and restoring one is checked
like a create. Values are compared exactly, with no URL canonicalization, so
`https://a.com` and `https://a.com/` differ.

Session context ends its header with a line naming the accepted custom kinds
usable in the project, so agents reuse a kind instead of proposing another.
`herma schema` describes each kind's fields.

Context and recall previews show a record's fields, one `name: value` line each
in text context. When they do not fit the budget, they are left out whole and
`fields` is listed in the record's `truncated_fields`.

## Working through the CLI

```sh
# Save the returned project ID for subsequent commands.
herma create --kind project --title 'Repository sessions' --status active

# Replace PROJECT_ID with that ID.
herma --identity session-a create --kind task \
  --project PROJECT_ID --title 'Session A is editing the API' --priority 4 \
  --body 'Working in internal/api; please coordinate overlapping edits here.' \
  --status in_progress --owner session-a --sources ISSUE_URL \
  --request-id api-session-a-start-1

# A fresh session receives a bounded context packet.
herma --identity session-b context --project PROJECT_ID

# Leave a handoff; use returned coordination IDs in --links.
herma --identity session-a create --kind note \
  --project PROJECT_ID --title 'API handoff' --tags handoff \
  --body 'Changed request validation. Race tests pass. Next: review the retry path.' \
  --links COORDINATION_ID --sources ISSUE_URL,COMMIT_URL

herma history COORDINATION_ID
herma list --project PROJECT_ID --kind task --status open
herma list --project PROJECT_ID --kind note --tag handoff
```

Inside a checkout with a `.herma-project.json`, `create` puts tasks, feedback,
notes and custom-kind records in the bound project when you omit `--project`;
pass `--project ''` to leave one without a project. Knowledge and principles stay
global unless you pass `--project`.

Global flags (`--url`, `--credentials`, `--identity`, `--socket`) go **before**
the command. Record/filter flags go after it; for updates, flags go after the
record ID. Use `--body-file notes.md` for multiline text, and comma-separated
`--tags`, `--links` and `--sources`. An explicitly empty update flag clears that
field. The API accepts arrays directly when a value itself contains a comma.

Commands return JSON to standard output and errors to standard error with a
nonzero exit code. `herma help` lists all commands, and `herma schema` describes the
live API, record fields, filters and limits. Its `kinds` are grouped as
`builtin`, `accepted`, `proposed` and `retired`; each entry lists the kind's
name, statuses, default status, fields and policy, and custom kinds add their
definition `record_id`, `description`, `pending` change and `project_id`.

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
herma update RECORD_ID --version 3 --archived true
herma list --include-archived
herma update RECORD_ID --version 4 --archived false
```

Notes allow archival/restoration only. To correct a note, append a new note
linking to the earlier record. Archived records remain readable by ID and in
history and export. There is no hard-delete endpoint.

## Session context

`herma context` (or `herma context --project ID`) prints compact text: a header line,
the trust and recall lines, a `Custom kinds: a, b (herma schema for fields)` line
when the project has accepted custom kinds, then accepted principles and a
section for each kind whose policy puts it in context. Built-in sections are
open tasks, unresolved feedback, recent handoff notes and (with
`--include-durable`) accepted knowledge. Custom kinds follow by name, each
capped by its `max_records`. Empty sections are left out, and a custom kind's
section appears only when at least one of its records fits the budget. A
closing line lists omitted and clipped records; `custom N` counts records of
custom kinds whose sections are not listed. `--principles omit` leaves
principles out.

`--format json` prints the API's packet. Besides `project`, `scope` and
`recall`, it has `kinds` (at most 20 names) with `kinds_more` for the rest,
`principles` with `principles_omitted`, `sections` as a list of
`{kind, heading, records, total, omitted}`, and `custom_omitted`.

The whole response is capped at **10,000 bytes by default** in either format.
Use `--max-bytes N` (2–64 KiB), or set `max_bytes` in the project binding.
Principles come first and may use at most half of the budget; coordination gets
whatever they leave. Each record preview uses at most 2 KiB or a quarter of the
budget, whichever is smaller.

In Claude Code, the SessionStart hook writes accepted principles to
`.claude/rules/herma/principles.md` in the checkout (next to a `.gitignore`
containing `*`), so they load as project rules on every start, resume and
compaction without using hook space. The hook's packet then carries only
coordination, plus the principles once when they have changed since the session
loaded the file. Claude Code caps hook context at 10,000 characters, so the
Claude hook never asks for more than 10,000 bytes.

If the binding is in your home directory, Claude receives the principles in the
hook context instead of a rules file. If the rules file cannot be updated, the
hook context carries them marked as replacing it. Either way a warning explains
why.

To stop using herma in a checkout, delete `.herma-project.json` and
`.claude/rules/herma/`; an archived project's rules file is removed at the next
session start.

Other projects, archived records and accepted knowledge are excluded by
default. `context --include-durable` adds accepted knowledge after the
principles; proposed, rejected and superseded records never enter context and are
available through `get`, `list`, search and export. The automatic startup hook
does not include knowledge: it carries the `recall` hint (and, for Codex or
when they changed, principles), and `herma recall` finds the rest.

## Recall

`herma recall "words"` searches accepted knowledge and principles, and records of
any kind whose policy sets `recall: true`, and returns the best matches first.
Each result carries `"reviewed": true` or `false`, so you can weigh
unreviewed content:

```sh
herma recall "snapshot pruning"
herma recall "retry backoff" --include-proposed --limit 5
```

A record matches when it contains any of the words; longer words also match
their stem, so `retries` finds `retry`. Title matches count more than body
matches, and the checkout's project comes before global records. Without
`--project`, recall uses the nearest `.herma-project.json`, or searches every
project when there is none. `--include-proposed` adds unreviewed records,
marked by `"status": "proposed"`.

The compact JSON response fits `--max-bytes` (default 8 KiB, 2–64 KiB). Long
bodies are cut and marked with `body_truncated`; `omitted` counts matches that
did not fit. Use `herma get ID` for the complete record. Search does not segment
Chinese or Japanese text into words.

## Search and lists

Search uses SQLite FTS5 over title, body and the textual fields of custom kinds
(`string`, `text`, `url` and `string-list`). Query words are treated as plain text
and all must match. Matching is by tokenizer terms, not arbitrary substrings:
Chinese or Japanese text without separators can be indexed as a whole run, so
searching for an embedded word may miss it. Language-aware segmentation is not
implemented yet. Filters are combined with AND. Lists default to 50 records
and support up to 200 per page with `--limit` and `--offset`.

### Filtering and sorting

On a list with `--kind`, `--where` filters by a typed field and `--sort` orders
the result. `--where` can repeat; filters combine with AND and with the other
filters.

```sh
herma list --kind bookmark --where 'rating>=4' --sort -read_at
herma list --kind bookmark --where 'platform=youtube' --where 'read_at missing'
herma list --kind bookmark --where 'authors has Hipp' --sort title
```

Quote each expression for the shell. A filter is `<field><op><value>`,
`<field> has <item>`, `<field> exists` or `<field> missing`. The field name
ends at the first operator and the rest, trimmed, is the value, so values may
contain spaces and operator characters.

| Operator | Field types | Meaning |
| --- | --- | --- |
| `=`, `!=` | string, text, integer, number, boolean, date, datetime, url, enum | equal, not equal |
| `<`, `<=`, `>`, `>=` | integer, number, date, datetime, string, url | numbers compare numerically, dates by calendar day, datetimes by instant to the millisecond, strings and urls by bytes |
| `has` | string-list | the list contains this exact item |
| `exists`, `missing` | every type | the field is set, or not |

Values follow the field's write rules, so `rating>=four` on an integer field is
an error, as is an unknown field or an operator the field's type does not
accept. Because the value is trimmed, a text value with leading or trailing
spaces can't be matched exactly. `has` matches one whole list item:
`tags has a,b` looks for the item `a,b`, not for `a` and `b`. `!=` also matches
records without the field. You can give at most 10 filters.

`--sort` takes up to 3 comma-separated keys, such as `--sort -read_at,title`.
A leading `-` sorts descending. Records without a value come last, and ties
break by record ID. Keys are a scalar field of the kind, or one of `priority`,
`created_at`, `updated_at`, `title` and `status`; the core keys win over a field of
the same name. Without `--sort`, lists keep the default order: priority, then
most recent update.

Field filters and field sort keys need `--kind`; only the core sort keys work
without it. Through the API the same query is
`GET /v1/records?kind=bookmark&where=rating%3E%3D4&sort=-read_at`. URL-encode
`where`, because a raw `+` in a UTC offset such as `+02:00` decodes to a
space. Repeat `where` for more filters.

See [the agent workflow](agent-workflow.md) for a session handover pattern.
