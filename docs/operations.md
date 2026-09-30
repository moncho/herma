# Operating herma

## Identity and deployment boundary

Each bearer token maps to one named identity and role. Source references and task
ownership are separate from authenticated authorship. This is one reviewer's
knowledge base shared with their agents, not a multi-user service.

`herma serve` listens only on loopback addresses. To reach it from other machines or
cloud sessions, put a tunnel or proxy in front of the TCP port, for example
Tailscale Serve, and give each remote session its own agent or read-only token
through `HERMA_URL` and `HERMA_TOKEN`. Never distribute the credentials file. A tunnel
forwards only TCP, and the server refuses reviewer tokens over TCP, so a reviewer
token copied to another machine is useless there.

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
shutdown, skipping it when nothing changed. It keeps the newest 14 snapshots
(`--backup-keep`, minimum `1`) and never touches other files in the folder.
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
