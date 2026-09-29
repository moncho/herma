# Design notes

```text
agent CLI / HTTP client
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

## Data and concurrency

Each record has indexed metadata and a full JSON snapshot. Every successful
write updates the record, search index, revision and optional idempotency receipt
in one SQLite transaction. The expected version is checked inside that
transaction. The database uses WAL, foreign keys and a busy timeout; one database
connection serializes this first version's work.

The API also holds a read lock for context and export so an API write cannot
interleave with their component queries. This guarantee assumes one service
process owns the file. Source references are intentionally distinct from the
authenticated identity and cannot override authorship.

Versioned edits prevent lost updates. They are not task leases, distributed
locks, or guarantees about what an agent does outside the service.

## Deliberate first-version limits

- Six validated record kinds, with no arbitrary schema creation.
- One shared trust domain; identities distinguish authors, not permissions.
- Plain-text full-text search and structured filters; no embeddings or model
  calls are required.
- No automatic ingestion, session hooks, orchestration, job scheduling, or web
  interface. Agents invoke the CLI or API explicitly.
- No hard deletion or retention policy; history and retry receipts accumulate.
- JSON export has no import counterpart yet; use a stopped-service directory
  backup to preserve the full database and credentials.
- List offset pagination can shift between separate calls as records change.
  Context and export are consistent within one request.
- A context category is bounded to 100 records, not a model token budget.
  Bodies may each be up to 64 KiB. Clients should retrieve only relevant content.
- The 16 MiB client response cap and in-memory export suit a small personal
  knowledge base. Large archives will need streaming export and retention work.

SQLite's WAL mode allows concurrent readers but only one writer at a time; the
database does not eliminate application-level edit conflicts. See
[SQLite's concurrency documentation](https://sqlite.org/wal.html#concurrency).
