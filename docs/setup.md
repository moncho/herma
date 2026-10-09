# Setting up herma

## Build and start the service

Requires **Go 1.27 or later**. The SQLite driver is pure Go; no C compiler,
separate database server, model API key, or hosted account is required.

Install herma from a checkout, then initialize and start it:

```sh
make install          # copies bin/herma to /usr/local/bin; BINDIR=~/.local/bin picks another folder
herma init
herma identity add session-a
herma identity add session-b
herma serve
```

`go install github.com/moncho/herma/cmd/herma@latest` works as well. The binary
carries the Claude Code plugin, so it needs no checkout beside it.

herma keeps its data in one folder, `~/.config/herma` unless `HERMA_DIR` names
another. Initialization creates the folder (`0700`) and `credentials.json` in it
with two identities, `owner` (the reviewer) and `local-agent` (an agent), without
overwriting an existing file. New identities are agents unless you pass
`--role`. The database is `knowledge.sqlite3` in the same folder, and the
service listens on `127.0.0.1:8765` and on a private Unix socket, `herma.sock`,
next to the credentials. Because nothing depends on the working folder, client
commands work from any directory. Keep the server terminal running and use
another terminal for client commands. Stop a foreground server with Ctrl-C;
closing its terminal leaves it running, because the server treats `SIGHUP` as a
request to reload credentials.

## Keep the service running on macOS

Once credentials have been initialized, use these commands to run the service
independently of a terminal or chat. Stop any foreground `herma serve` process
before switching to the background service.

```sh
make service-start
make service-status
herma list
```

To take automatic backups, pass a folder that your sync tool copies off the
machine, such as iCloud Drive or Dropbox. Use an absolute path, because launchd
starts the service from another directory:

```sh
make service-start BACKUP_DIR="$HOME/Library/Mobile Documents/com~apple~CloudDocs/herma-backups"
```

macOS keeps background services out of iCloud Drive until you allow it: add the
installed `herma` to System Settings → Privacy & Security → Full Disk Access, or
the service waits on the folder and logs that it is still waiting. macOS ties
the permission to the binary, so a newly built herma may need it again.

Give each database its own folder. On a new machine, run `herma restore` before
starting the service with `BACKUP_DIR`: herma refuses to serve an empty database
against a folder with newer snapshots.

To keep the reviewer token out of the agents' `credentials.json`, add
`REVIEWER_CREDENTIALS=/absolute/path/reviewer.json`; see
[keeping the reviewer token out of the agents' file](operations.md#keeping-the-reviewer-token-out-of-the-agents-file).

In the foreground, use `herma serve --backup-dir DIR`. See
[backups and restore](operations.md#backups-and-restore).

`make service-start` installs herma first and runs the installed binary on the
data folder (`HERMA_DIR=…` picks another). macOS manages the process and restarts
it if it fails. Logs are in `server.log` in the data folder. To stop it, run
`make service-stop`; to deploy a new version, stop it and run `make
service-start` again. Credential changes are
picked up automatically. This job is registered for the current login session,
so run `make service-start` again after logging out or rebooting. It does not
install an automatic login item.

If a client reports `connection refused`, or a reviewer command reports that
nothing is listening on the socket, no service is running for these credentials.
Start it with `make service-start` on macOS, or keep `herma serve` running in
another terminal. The database and credentials remain on disk when the server
stops; do not reinitialize them.

## Try the API and example

```sh
herma schema
herma list
go run -buildvcs=false ./examples/handover
```

The example creates a demo project, handoff and coordination record as
`session-a`, retrieves them as `session-b`, completes the coordination record at
its expected version, and leaves a handover note. It prints the record IDs and
authenticated revision authors. Each run creates a new demo project. Restart
persistence and competing edits are also exercised by the integration tests.

## Load principles automatically in each session

Install the hook once per user. It runs in every folder, so you do not install
it per repository or per worktree:

```sh
herma --identity local-agent hook install --client both
```

Use an agent identity; hook installation refuses the reviewer. To tell Claude
and Codex sessions apart in herma's history, give each client its own agent
identity and install them separately instead:

```sh
herma --identity <claude-identity> hook install --client claude
herma --identity <codex-identity> hook install --client codex
```

Prefer this when both clients work on the same projects and you want each
record to show which one wrote it; `--client both` is simpler when one identity
is enough. Installation needs no binding. To also give a repository its own principles and a coordination
summary, create one herma project for it and bind it from the repository root.
Replace `PROJECT_ID` with the ID returned by the first command:

```sh
herma create --kind project --title 'My repository' --status active
herma project bind --project PROJECT_ID --max-bytes 10000
```

The hook records the absolute paths of the herma executable and the credentials
file. Install the hook using an installed executable, not `go run`: the hook must be
able to find the same executable in future sessions.

Use `--client claude` or `--client codex` to install for one client. Installation
merges a synchronous SessionStart hook into `~/.claude/settings.json` and/or
`~/.codex/hooks.json`, preserving existing settings and other hooks. Repeating
installation updates herma's own hook without duplicating it. Each client's hook
runs `herma hook session-start --client <client>`; rerun `herma hook install` after
upgrading herma so existing hooks gain the flag. For this release, upgrade the
clients before the server: on every client machine install the new binary and
rerun `herma hook install`, then restart `herma serve` on the new binary. An old
client talking to a new server gets only project principles and loses the global
ones; a new client talking to an old server only lacks the coordination summary
line. The hook loads no record text, so it ignores the binding's `max_bytes`;
that budget applies to `herma context`. Commands capture the
executable, service URL and credential-file path, never a token. Keep these
machine-specific hook files out of repositories; the generated `.herma-project.json`
contains only the project ID and budget and can be committed for other
sessions and worktrees.

`hook install` also removes herma's old project-level hook entries
(`.claude/settings.local.json`, `.codex/hooks.json`) from the checkout named by
`--dir`, which defaults to the current folder, so running it inside a checkout
set up before the user-level hook keeps that checkout from running herma twice.
The output lists the files it changed under `removed`; if a checkout's files
cannot be changed, it reports `remove_warning` and still installs the plugin. The Codex
home is `$CODEX_HOME` when set, else `~/.codex`; `hook install` does not handle a
`CODEX_HOME` other than `~/.codex`.

In Codex, review and trust the installed hook once through `/hooks`. This is
Codex's normal hook setup requirement. Start a new session after installation.
The hook runs on every SessionStart, including startup, resume, clear,
compaction and Claude session forks. See the official
[Codex hook documentation](https://learn.chatgpt.com/docs/hooks) and
[Claude Code hook documentation](https://code.claude.com/docs/en/hooks).

In every folder the hook syncs your accepted global principles to the client's
standing instructions: `~/.claude/rules/herma/global-principles.md` for Claude
Code and a managed block in `AGENTS.md` in the Codex home. A client's file is
written only when its home folder already exists. In a bound checkout it also
adds that project's principles (Claude: `.claude/rules/herma/principles.md`;
Codex: inline in the hook context) and one line counting open coordination
records. It never loads task, feedback or note text. In an unbound folder it is
silent unless global principles changed. See
[principles](usage.md#principles) for the files.

The hook discovers the nearest `.herma-project.json` using the session's actual
working directory. Nested folders work; lookup stops at a Git repository or
worktree boundary. Track the binding in each worktree where it is needed; the
hook itself is already installed. An invalid nearby binding produces a warning
instead of silently falling back. To change a binding, edit its project ID/budget
explicitly; `project bind` will not overwrite a different existing configuration.

The hook writes and removes only herma's generated rules files and the Codex
block (the project rules file gets a `.gitignore` when absent). Every hook has a
five-second request deadline and continues with a short warning if the service,
credentials or a bound checkout's binding are unavailable. Token-based installs
inherit `HERMA_TOKEN` from the agent environment and warn if it is missing,
without falling back to another identity. Named-identity installs require
`HERMA_TOKEN` to be unset. No MCP server is needed for this automatic loading
path.

Both clients also get a hint to use `herma recall` for other reviewed
knowledge. After binding, `herma context` works without repeating the project ID;
it prints the coordination records the summary line counts.

## Claude Code plugin

In the Claude Code CLI, a plugin built into herma adds a herma status line and
two read-only tools. The status line shows, in every folder:

| State | Text |
| --- | --- |
| Healthy, nothing to review | `herma ✓ · backup 3h` |
| Proposals waiting | `herma ✓ · 2 to review · backup 3h` |
| Backup stale | `herma ✓ · backup 2d ⚠` |
| Backups disabled | `herma ✓ · backup off` |
| No successful backup yet | `herma ✓ · backup none` |
| `herma status` failed | `herma ✗` and the first line of the error |
| Identity is a reviewer | `herma ✗ reviewer identity refused` |
| Options not from user settings | `herma ✗ herma options must come from user settings` |

Backup ages show as minutes under an hour, hours under two days, then days. The
line refreshes at session start, every 60 seconds and after each herma tool call, and
a toast appears when herma becomes unreachable, its backup turns stale, or the
number of proposals to review rises (`herma: N new proposal(s) to review`).

The tools are `mcp__herma__recall` (a query of up to 500 characters, up to 20
results, optionally including proposals) and `mcp__herma__get` (one record by ID).
They are registered only in a bound checkout when the identity is an agent,
unlike the status line. If
herma cannot be launched, the tool call returns an error.

`herma hook install --client claude` (or `both`) installs the plugin after the
hook. It writes the plugin built into herma to `plugins/claude` in the data folder,
adds that folder to `env.CLAUDE_CODE_PLUGIN_DIRS` in `~/.claude/settings.json`
(dropping any other folder holding a herma plugin, so only one loads), writes
`pluginConfigs.herma.options` (`herma`, the herma executable; `credentials`, the
credentials path; `identity`; and `url`, the server URL from `--url` or
`HERMA_URL`), and allows `mcp__herma__recall` and `mcp__herma__get` under
`permissions.allow`. Everything else in the file is kept, and repeating the
command adds nothing twice. Run it again after upgrading herma to refresh the
plugin. To work on the plugin itself, set `HERMA_CLAUDE_PLUGIN_DIR` to a
checkout's `plugins/claude` and herma registers that folder instead; if it is
missing, the command fails after installing the hook. With `HERMA_TOKEN`
set, the plugin step is skipped, because the plugin needs a credentials file;
on another machine, use a client file instead (see
[remote clients](operations.md#remote-clients)).
A symlinked `~/.claude` or `settings.json` is refused.

The plugin reads its options from user settings only. If project or local
settings give different ones, or the `herma` or `credentials` path is not absolute,
it shows `herma ✗ herma options must come from user settings` and runs nothing. It
runs herma with exactly these options, clearing `HERMA_TOKEN`, `HERMA_IDENTITY`,
`HERMA_CREDENTIALS`, `HERMA_SOCKET` and `HERMA_URL` from its environment, so it uses the
agent identity and server you installed with. It shows an error instead of
registering tools if that identity turns out to be a reviewer. It
works only in the CLI: in the desktop app it shows `herma ✗` and offers no tools.

To remove the plugin, delete the herma path from `env.CLAUDE_CODE_PLUGIN_DIRS`, the
`pluginConfigs.herma` entry (with its four options `herma`, `credentials`, `identity`
and `url`) and the two `mcp__herma__` rules from
`~/.claude/settings.json`, and delete `~/.config/herma/plugins/claude`. To remove
the hook as well, delete herma's SessionStart entry (its command ends in
`# herma-managed:session-start:v2`) from `~/.claude/settings.json` and
`~/.codex/hooks.json`, delete `~/.claude/rules/herma/global-principles.md`, and
delete the lines from `<!-- herma:principles:start` to `<!-- herma:principles:end -->`
in `AGENTS.md` in the Codex home.

The status line reads `herma status`, which you can also run yourself, in a
bound checkout or not:

```sh
herma status
```

```json
{
  "backup": {
    "age_seconds": 10800,
    "enabled": true,
    "last_success_at": "2026-10-05T09:00:00Z",
    "stale": false
  },
  "bound": true,
  "identity": "local-agent",
  "project_id": "PROJECT_ID",
  "review": {"knowledge": 1, "principles": 1, "kinds": 0, "pending_kind_changes": 0, "total": 2},
  "role": "agent",
  "server": "ok"
}
```

Outside a bound checkout it reports the same fields with `"bound": false` and no
`project_id`. With backups disabled,
`backup` is `{"enabled": false}`; before the first successful backup,
`age_seconds` and `last_success_at` are absent.

Next: [using herma](usage.md) for records, roles and the CLI, and
[operating herma](operations.md) for remote access, security and backups.
