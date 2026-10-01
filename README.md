# herma

A shared memory for AI agent sessions working on the same project.

Several Claude Code or Codex sessions on one codebase keep stepping on each
other: two edit the same package, a blocker found in one never reaches the next,
and every new session starts from zero. herma gives them one place to say who is
working where, what is blocked, what changed and what the next session should
know, and a reviewed store of knowledge that outlives any single session.

- **Coordination that loads itself.** A SessionStart hook hands every new session
  a compact, byte-capped packet of open work, blockers and recent handoffs.
- **Memory you approve.** Agents propose knowledge and principles; only you, the
  reviewer, can accept them. Accepted principles load into every session, and
  `herma recall "words"` finds the rest by relevance. Nothing unreviewed is loaded.
- **Safe to share across machines.** Agents on other machines connect through a
  tunnel with their own tokens. Your reviewer token works only on a local socket,
  so a copied token can't approve anything.
- **Small and local, with backups.** One Go binary and one SQLite file. No
  database server, hosted account or model API key. `herma serve` can snapshot the
  database into a folder your sync tool already copies off the machine.

Real tasks stay in Linear; herma links to them rather than duplicating them. herma is
a new, independent project inspired by the
[external knowledge base described in The Ground Truth](https://thegroundtruth.media/i/217789967/external-knowledge-base).

## How it works

Sessions write short records through a CLI or an authenticated HTTP API:

- **tasks** say what a session is doing and what blocks it,
- **notes** are immutable handoffs for whoever comes next,
- **feedback** captures friction between sessions,
- **knowledge** and **principles** are lasting conclusions that stay `proposed`
  until you accept them.

Every change is versioned, so a stale edit gets a conflict instead of
overwriting another session's work, and every revision is kept.

## Quick start

Requires Go 1.26 or later.

```sh
go build -trimpath -buildvcs=false -o bin/herma ./cmd/herma
./bin/herma init      # creates `owner` (you, the reviewer) and `local-agent`
./bin/herma serve
```

In another terminal, an agent proposes something and you review it:

```sh
./bin/herma create --kind knowledge --title 'SQLite runs in WAL mode' \
  --sources https://sqlite.org/wal.html
./bin/herma review
./bin/herma --identity owner update RECORD_ID --version 1 --status accepted
```

## Documentation

- [Setting up herma](docs/setup.md): building, running the service, and loading
  context automatically in Claude Code and Codex sessions.
- [Using herma](docs/usage.md): records, roles and review, the CLI, session context
  and search.
- [Operating herma](docs/operations.md): remote access, what the roles protect
  against, backups and restore.
- [Agent workflow](docs/agent-workflow.md): a session handover pattern.
- [Design notes](docs/design.md): package structure and current limits.

## Development

```sh
go test -race ./...
go vet ./...
```
