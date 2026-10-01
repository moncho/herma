# Setting up herma

## Build and start the service

Requires **Go 1.26 or later**. The SQLite driver is pure Go; no C compiler,
separate database server, model API key, or hosted account is required.

Run these commands from the project's directory:

```sh
go build -trimpath -buildvcs=false -o bin/herma ./cmd/herma
./bin/herma init
./bin/herma identity add session-a
./bin/herma identity add session-b
./bin/herma serve
```

The service listens on `127.0.0.1:8765` and on a private Unix socket at
`.herma/herma.sock`. Initialization creates `.herma/credentials.json` with two
identities, `owner` (the reviewer) and `local-agent` (an agent), without
overwriting an existing file. New identities are agents unless you pass
`--role`. The database lives at `.herma/knowledge.sqlite3`. Keep the server
terminal running and use another terminal in the same directory for client
commands. Stop a foreground server with Ctrl-C; closing its terminal leaves it
running, because the server treats `SIGHUP` as a request to reload credentials.

## Keep the service running on macOS

Once credentials have been initialized, use these commands to run the service
independently of a terminal or chat. Stop any foreground `herma serve` process
before switching to the background service.

```sh
make service-start
make service-status
./bin/herma list
```

To take automatic backups, pass a folder that your sync tool copies off the
machine, such as iCloud Drive or Dropbox. Use an absolute path, because launchd
starts the service from another directory:

```sh
make service-start BACKUP_DIR="$HOME/Library/Mobile Documents/com~apple~CloudDocs/herma-backups"
```

Give each database its own folder. On a new machine, run `herma restore` before
starting the service with `BACKUP_DIR`: herma refuses to serve an empty database
against a folder with newer snapshots.

To keep the reviewer token out of `.herma/credentials.json`, add
`REVIEWER_CREDENTIALS=/absolute/path/reviewer.json`; see
[keeping the reviewer token out of the agents' file](operations.md#keeping-the-reviewer-token-out-of-the-agents-file).

In the foreground, use `./bin/herma serve --backup-dir DIR`. See
[backups and restore](operations.md#backups-and-restore).

macOS manages the process and restarts it if it fails. Logs are in
`.herma/server.log`. To stop it, run `make service-stop`; to deploy a rebuilt
binary, stop it and run `make service-start` again. Credential changes are
picked up automatically. This job is registered for the current login session,
so run `make service-start` again after logging out or rebooting. It does not
install an automatic login item.

If a client reports `connection refused`, or a reviewer command reports that
nothing is listening on the socket, no service is running for these credentials.
Start it with `make service-start` on macOS, or keep `./bin/herma serve` running in
another terminal. The database and credentials remain on disk when the server
stops; do not reinitialize them.

## Try the API and example

```sh
./bin/herma schema
./bin/herma list
go run -buildvcs=false ./examples/handover
```

The example creates a demo project, handoff and coordination record as
`session-a`, retrieves them as `session-b`, completes the coordination record at
its expected version, and leaves a handover note. It prints the record IDs and
authenticated revision authors. Each run creates a new demo project. Restart
persistence and competing edits are also exercised by the integration tests.

## Load context automatically in each session

Create one herma project for the repository, then bind it from the repository root.
Use an absolute path to your built herma executable and credentials if they live
elsewhere. Replace `PROJECT_ID` with the ID returned by the first command:

```sh
herma create --kind project --title 'My repository' --status active
herma project bind --project PROJECT_ID --max-bytes 12288
herma --credentials /absolute/path/credentials.json --identity local-agent \
  hook install --client both
```

Use an agent identity; hook installation refuses the reviewer.

If the service's credentials live elsewhere, set `HERMA_CREDENTIALS` to their
absolute path before the create/bind commands too. `herma` above means the built
executable on your PATH; otherwise use its full path. Install hooks using a
built executable, not `go run`: the hook must be able to find the same
executable in future sessions.

Use `--client claude` or `--client codex` to install for one client. Installation
merges a synchronous SessionStart hook into `.claude/settings.local.json` and/or
`.codex/hooks.json`, preserving existing settings and other hooks. Repeating
installation updates herma's own hook without duplicating it. Commands capture the
executable, service URL and credential-file path, never a token. Keep these
machine-specific hook files local; the generated `.herma-project.json` contains only
the project ID and budget and can be committed for other sessions/worktrees.

In Codex, review and trust the installed hook through `/hooks`; the project must
also be trusted. This is Codex's normal hook setup requirement. Start a new
session after installation. The hook runs on every SessionStart, including
startup, resume, clear, compaction and Claude session forks. See the official
[Codex hook documentation](https://learn.chatgpt.com/docs/hooks) and
[Claude Code hook documentation](https://code.claude.com/docs/en/hooks).

The hook discovers the nearest `.herma-project.json` using the session's actual
working directory. Nested folders work; lookup stops at a Git repository or
worktree boundary. Track the binding in each worktree where it is needed, and
install local hooks there. An invalid nearby binding produces a warning instead
of silently falling back. To change a binding, edit its project ID/budget
explicitly; `project bind` will not overwrite a different existing configuration.

The startup hook is read-only, has a five-second request deadline, and continues
with a short warning if the service, credentials or binding are unavailable.
Unbound projects are a quiet no-op. Token-based installs inherit `HERMA_TOKEN` from
the agent environment and warn if it is missing, without falling back to another
identity. Named-identity installs require `HERMA_TOKEN` to be unset. No MCP server
is needed for this automatic loading path.

Each session also receives the project's accepted principles and a hint to use
`herma recall` for other reviewed knowledge.

After binding, `herma context` works without repeating the project ID. Context
refreshes at session boundaries; run it again during a long session before
coordinating an edit with another agent.

Next: [using herma](usage.md) for records, roles and the CLI, and
[operating herma](operations.md) for remote access, security and backups.
