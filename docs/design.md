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

The product scope is coordination between concurrent sessions and handoffs to
their successors, plus reviewed durable knowledge. Agents propose knowledge and
principle records and the reviewer accepts them; Linear owns real tasks. Task
records describe temporary session working intent, ownership and blockers with
source links; notes describe handoffs. Accepted principles load into every
session's context; knowledge is found with `herma recall` or `--include-durable`.

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
version check: agents create knowledge and principles only as proposed and cannot
change them once judged, and accepting requires a source. A reviewer's judgement
sets `reviewed_by` and `reviewed_at`.

`herma serve` runs one handler on two listeners: loopback TCP for everyone and a
private Unix socket that marks its requests. Credentials reload on `SIGHUP` or
when the file changes; an invalid file keeps the previous identities.

## Deliberate first-version limits

- Six validated record kinds, with no arbitrary schema creation.
- One reviewer; roles separate the reviewer from agents and read-only sessions.
- Plain-text full-text search and structured filters; no embeddings or model
  calls are required. Recall ranks accepted knowledge with FTS5 `bm25`, any-word
  matching and four-rune stems. A trigram index for substrings and Chinese or
  Japanese segmentation is deferred.
- SessionStart hooks load context for bound projects in Claude Code and Codex.
  They read only, refresh at session boundaries, and fail open after a short
  deadline. They do not write a handoff automatically or watch other sessions.
- No automatic ingestion, orchestration, Linear synchronization, job scheduling
  or web interface. Agents record coordination changes explicitly.
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
