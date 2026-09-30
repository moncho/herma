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
