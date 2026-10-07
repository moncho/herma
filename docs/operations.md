# Operating herma

## Identity and deployment boundary

Each bearer token maps to one named identity and role. Source references and task
ownership are separate from authenticated authorship. This is one reviewer's
knowledge base shared with their agents, not a multi-user service.

`herma serve` listens only on loopback addresses. To reach it from other machines or
cloud sessions, put a tunnel or proxy in front of the TCP port, for example
Tailscale Serve, and give each client its own agent or read-only identity (see
[remote clients](#remote-clients)). Never copy the server's credentials file. A
tunnel forwards only TCP, and the server refuses reviewer tokens over TCP, so a
reviewer token copied to another machine is useless there.

`HERMA_TOKEN` cannot be combined with `--identity` or a nonempty `HERMA_IDENTITY`;
unset it to use a named identity. Tokens are never included in API responses.

## What the roles protect against

What this protects against: theft of any token from a remote machine, agents
approving knowledge by accident, and stolen agent or read-only tokens. What it
does not: an agent process running as your own user on the server machine can
read the credentials file or connect to the socket, and with shell access it can
run reviewer commands itself (for example `herma --identity owner update ...`), so
that case is out of scope. Agent-client permission rules reduce accidental access
but cannot reliably stop a process from connecting to the socket.

## Remote clients

A machine that reaches the server through the tunnel gets what the server
machine gets, except reviewing: session context, the generated principles file
and, in the Claude Code CLI, the plugin's status line and tools. Give each
machine its own identity and a client credentials file holding only that
identity:

```sh
# On the server machine
herma identity add laptop --client-file ~/laptop-credentials.json
```

The client file is private (0600), is never written over an existing file, and
cannot hold a reviewer. Move it privately to the other machine, for example to
`~/.config/herma/credentials.json` with the same permissions, and delete the
copy on the server machine. On the other machine, build herma from a checkout
(`go build -o bin/herma ./cmd/herma`), then bind each repository and install the
hooks from its root, naming the tunnel URL:

```sh
herma --url https://herma.example.ts.net \
  --credentials ~/.config/herma/credentials.json --identity laptop \
  project bind --project PROJECT_ID
herma --url https://herma.example.ts.net \
  --credentials ~/.config/herma/credentials.json --identity laptop \
  hook install --client claude
```

`herma` above is the absolute path to that build. To end a machine's access, run
`herma identity revoke laptop` on the server. Sessions that cannot keep a file,
such as cloud sessions, use `HERMA_URL` and `HERMA_TOKEN` instead; they get
session context but not the plugin.

The server can run on any machine with a local disk. To move it, stop the
service, [restore](#backups-and-restore) the newest snapshot on the new machine,
copy the server's credentials files there privately (snapshots hold no
credentials), start the service and point the tunnel at it. Clients keep their
files.

## Keeping the reviewer token out of the agents' file

By default `.herma/credentials.json` holds every identity, so anything that can read
it, including the session hook, also holds the reviewer token. To keep that token
elsewhere, move reviewer identities into a second private (0600) file outside the
repository and pass it to the server:

```sh
herma serve --reviewer-credentials ~/.config/herma/reviewer.json ...
```

`HERMA_REVIEWER_CREDENTIALS` sets the same path. The server merges both files and
reloads either one when it changes; a name or token appearing in both is an
error, and the merged set must contain a reviewer. Reviewer commands then name
that file and the server's socket:

```sh
herma --credentials ~/.config/herma/reviewer.json --socket /path/to/.herma/herma.sock --identity owner whoami
```

To move an existing reviewer, copy its entry into the new file, restart the
server with `--reviewer-credentials`, then `herma identity revoke owner` from the
main file. Agents still run as your user and can read the new file if they look
for it, so this keeps the token out of the hook and the repository rather than
out of reach.

If an interrupted identity command leaves a lock behind, the error names the
exact lock file. Confirm no other `herma identity` command is running before
removing that file and retrying.

## One database, one process

The service owns one SQLite database on local disk. Writes and history are
transactional, and context/export hold off API writes while reading a consistent
snapshot. Request-body reads, response encoding and network writes happen
outside that lock, so a stalled client does not freeze other sessions. Run one
service process per database. Do not share the SQLite file over a network
filesystem or open competing service processes against it.

## Export, backups and restore

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

### Backups and restore

`herma serve --backup-dir DIR` writes a snapshot of the database into `DIR` at
startup, every six hours (`--backup-every`, minimum `5m`) and on a graceful
shutdown, skipping it when nothing changed. The interval is wall-clock time: a
machine that slept past it takes a snapshot within a minute of waking. It keeps
the newest 14 snapshots (`--backup-keep`, minimum `1`) and never touches other
files in the folder.
`--backup-every` and `--backup-keep` require `--backup-dir`, and `herma serve`
refuses to start if `DIR` is missing, not a directory or not writable. Point
`DIR` at a folder your sync tool already copies off the machine: herma uploads
nothing itself.

Each snapshot, `herma-YYYYMMDDTHHMMSSZ.sqlite3` (UTC), is a complete, checked copy
of records and history written while the service keeps running. Snapshots
contain no credentials and are not encrypted, so choose a folder you would trust
with the project's notes.

`herma backup status` shows whether backups are enabled, the folder, interval and
keep count, the last success, the last error and whether backups are `stale` (no
success for twice the interval). An idle store whose snapshot is current is not
stale. Sessions also get a one-line warning at startup when backups are enabled
and stale.

Give each database its own backup folder. On a new machine, run `herma restore`
before starting `herma serve --backup-dir` (or `make service-start BACKUP_DIR=…`):
herma refuses to serve an empty database against a folder holding snapshots with
more revisions than the database, and tells you to restore or pick another
folder. If the macOS service runs on the machine you are restoring on, stop it
with `make service-stop` first.

To restore after losing the machine:

```sh
./bin/herma init
./bin/herma restore /path/to/synced/herma-backups
./bin/herma serve --backup-dir /path/to/synced/herma-backups
# or, on macOS: make service-start BACKUP_DIR=/path/to/synced/herma-backups
./bin/herma identity add session-a
```

`herma restore SNAPSHOT|DIR [--db PATH] [--replace]` picks the newest snapshot that
passes its checks when given a folder. It refuses to run while a server answers
on the socket next to the credentials file (pass the same `--socket` if the
server used a custom one). It refuses to overwrite an existing database, or a
leftover `-wal` or `-shm` file, unless you pass `--replace`, which moves the
current files aside as `*.before-restore-YYYYMMDDTHHMMSSZ` instead of deleting
them. If a restore fails after moving files aside, the error lists them. Restore
never touches credentials: issue new agent tokens and give them to remote
sessions.
