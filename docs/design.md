# Design notes

```text
SessionStart hook / agent CLI / HTTP client
          │ authenticated JSON over HTTP(S)
          ▼
one Go service process
          │ transactions, revision checks, replay keys
          ▼
SQLite on local disk
  records + FTS5 index + immutable revisions + idempotency receipts
```

`cmd/herma` supplies process cancellation and exit status. `internal/cli` manages
commands, local credentials and server startup. `internal/client` supplies a
bounded HTTP transport that refuses redirects. `internal/api` handles auth,
strict input parsing, context assembly and export. `internal/store` owns
validation, references, search and all transactional writes.
`internal/project` resolves a repository's nonsecret binding; `internal/hooks`
merges client hook configurations while preserving other settings.

The product scope is memory that shapes agent behavior: principles first,
reviewed knowledge second. Agents propose principle and knowledge records and the
reviewer accepts them; the issue tracker owns real tasks. herma also holds
coordination records that agents may use freely: task records describe
temporary session working intent, ownership and blockers with source links, and
notes describe handoffs. Agents pull them with `herma context` when they judge it
helps. Session start loads only reviewed principles and a one-line count of the
coordination records; knowledge is found with `herma recall` or
`--include-durable`.

Only reviewed principles are meant to shape behavior, and agents copy what they
see in context, so unreviewed records are never delivered as standing
instructions.

Principles travel in instruction files (Claude user and project rules, a managed
block in Codex's AGENTS.md) rather than in hook context. Hook context is capped
(Claude: 10,000 characters) and cannot guarantee the full set arrives.

## Data and concurrency

Each record has indexed metadata and a full JSON snapshot. Every successful
write updates the record, search index, revision and optional idempotency receipt
in one SQLite transaction. The expected version is checked inside that
transaction. The database uses WAL, foreign keys and a busy timeout; one database
connection serializes this first version's work.

The API holds a read lock only while building context and export snapshots, and
an exclusive lock only around committed create/update operations. It reads and
validates request bodies before locking and encodes/writes responses after
unlocking. Slow uploads or downloads therefore cannot hold that lock. This
guarantee assumes one service process owns the file. Source references are intentionally distinct from the
authenticated identity and cannot override authorship.

Search rows are keyed by an FTS rowid stored on each record, so writes update
one index row directly.

Versioned edits prevent lost updates. They are not task leases, distributed
locks, or guarantees about what an agent does outside the service.

## Roles and review

Permissions are split by what each check needs. The API refuses every write from
a read-only token before reading the body, and refuses reviewer tokens unless the
request arrived on the Unix socket. The store receives the author's name and role
and enforces the durable-record rules inside the write transaction, next to the
version check (kind definitions, field validation and uniqueness are checked in
that same transaction): agents create knowledge and principles only as proposed and cannot
change them once judged, and accepting requires a source. A reviewer's judgement
sets `reviewed_by` and `reviewed_at`. Kind records follow proposed, accepted,
retired, and only a reviewer moves them.

`herma serve` runs one handler on two listeners: loopback TCP for everyone and a
private Unix socket that marks its requests. Credentials reload on `SIGHUP` or
when the file changes; an invalid file keeps the previous identities.

## Deliberate first-version limits

- Record kinds are definitions: six built-in coordination and memory kinds and the built-in kind `kind` compiled into herma, plus kinds agents propose and the reviewer accepts. Fields are flat and typed; changes to an accepted kind are additive unless no live record breaks; there is no data migration for renamed or retyped fields.
- One reviewer; roles separate the reviewer from agents and read-only sessions.
- Plain-text full-text search and structured filters; no embeddings or model
  calls are required. Recall ranks accepted knowledge with FTS5 `bm25`, any-word
  matching and four-rune stems. A trigram index for substrings and Chinese or
  Japanese segmentation is deferred.
- Field filters and sorts are evaluated over the record's JSON with
  `json_extract`; indexes cover the kind and kind names, not individual fields,
  so a query scans one kind's rows.
- One user-level SessionStart hook per client runs in every folder in Claude Code
  and Codex. It syncs principles to the instruction files, adds the coordination
  summary in bound checkouts, refreshes at session boundaries, and fails open
  after a short deadline. It does not write a handoff automatically or watch
  other sessions.
- No automatic ingestion, orchestration, issue-tracker synchronization, job
  scheduling or web interface. Agents record coordination changes explicitly.
- No hard deletion or retention policy; history and retry receipts accumulate.
  Receipts reference the revision they returned rather than copying the record.
- Durability comes from `VACUUM INTO` snapshots that `herma serve` writes into a
  synced folder on an interval; `herma restore` installs the newest valid one.
  Snapshots exclude credentials. JSON export has no import counterpart.
- List offset pagination can shift between separate calls as records change.
  Context and export are consistent within one request.
- Context is capped by the exact bytes of the response in its format (compact
  text or JSON), including metadata and newline: 10 KiB by default,
  configurable between 2 and 64 KiB, so Claude Code's 10,000-character hook
  limit holds. A separate
  100-record candidate limit per category bounds query work. Summaries mark
  clipped bodies/fields and count omitted records. This is not a token estimate.
- Exports are still assembled in memory, with a separate history query per
  record. Preparation has a 20-second budget, and the API rejects exports above
  16 MiB before committing a success response. Exact Content-Length lets clients
  detect interrupted transfers, but slow downloads can still hit the server's
  write timeout. Large archives will need streaming export and retention work.

SQLite's WAL mode allows concurrent readers but only one writer at a time; the
database does not eliminate application-level edit conflicts. See
[SQLite's concurrency documentation](https://sqlite.org/wal.html#concurrency).
