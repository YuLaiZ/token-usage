# CLI Reference

> [简体中文](cli.zh-CN.md) | English

This document is the authoritative reference for the `token-usage` command-line interface: command tree, positional arguments, flags, exit codes, and examples. Source-code behavior takes precedence over this document.

## Command Tree

```text
token-usage
├── version                               # show version and build metadata (five-line detailed output)
├── completion [bash|zsh|fish|powershell] # generate a shell completion script
├── collect [YYYYMMDD|YYYYMMDD-YYYYMMDD]  # incremental collection for today or a specified date (includes router)
│   ├── all                               # two-phase full collection: all historical messages + full router backfill
│   ├── router --client X                 # full router backfill only (does not touch messages)
│   └── retry                             # retry unresolved groups in collection_errors
├── query [YYYYMMDD|YYYYMMDD-YYYYMMDD]
│   ├── client [YYYYMMDD|YYYYMMDD-YYYYMMDD]  # group by client (default view)
│   ├── model [YYYYMMDD|YYYYMMDD-YYYYMMDD]   # group by model
│   ├── project [YYYYMMDD|YYYYMMDD-YYYYMMDD] # group by project
│   ├── sessions [YYYYMMDD|YYYYMMDD-YYYYMMDD]# session details
│   └── summary [YYYYMMDD|YYYYMMDD-YYYYMMDD] # overview summary
├── errors [YYYYMMDD]
├── config                                # no arguments: open the interactive configuration TUI
│   ├── show                              # output complete effective TOML (read-only, pure TOML)
│   ├── get <key>
│   ├── set <key> <value>
│   └── init
├── start
├── restart
├── status
├── stop
├── update                                # self-update from official GitHub Releases (--check / --version)
└── _run                                  # hidden; started by start/launchd/the Registry; do not invoke directly
```

Design points:

- There is no top-level `router` subcommand. Router attribution is reached through `collect all` (included) or `collect router` (attribution layer only).
- Dates are **positional arguments**: a single day is `YYYYMMDD`; an inclusive range is `YYYYMMDD-YYYYMMDD`. There is no `--date` flag. `errors` accepts only a single `YYYYMMDD`.
- `query` has no `--format` or `--by-*` flag. A subcommand selects the view and output is always a table.
- Running `token-usage` with no arguments only prints help; it starts neither the TUI nor the daemon.
- The root command has a `-v, --version` flag for one-line short output and a `version` subcommand for multi-line detailed output; see [version](#version).
- `completion` is Cobra's built-in command. It writes bash/zsh/fish/PowerShell completion scripts to standard output and reads neither configuration nor the database.
- `update` is a top-level self-update command (flags `--check` and `--version`); it is the only command that rewrites the running binary, and only when the current binary is an official Release asset. See [update](#update).

## General Conventions

### Date argument format

| Command | Accepted form | Default |
|------|----------|------|
| `collect` / `query` and their subcommands | `YYYYMMDD` or `YYYYMMDD-YYYYMMDD` (inclusive) | Today |
| `errors` | A single `YYYYMMDD` (ranges are not accepted) | With neither a date nor `--source`, only unresolved errors are shown. |

`YYYYMMDD` is an eight-digit compact format (for example, `20260701`). `YYYY-MM-DD`, extra positional arguments, range endpoints shorter than eight digits, and an end date before the start date all fail with an error and command examples. A range expands into an inclusive per-day list.

### Exit codes

`token-usage` maps a command error to an exit code in `main`:

- `0`: success, including idempotent results such as `start` when the daemon is already running and `stop` when it is not.
- `1`: any error (argument validation failure, collection/query failure, daemon-control failure, revision conflict, partial failure, and so on).

The stdout/stderr contract for success and failure is described in each command section.

### Flag scope

- `--client`: a **PersistentFlag** of `collect`, inherited by its `all`, `router`, and `retry` subcommands.
- `--force`: a **LocalFlag** of `collect`, **not** inherited by subcommands (passing it to a subcommand returns an unknown-flag error).
- `errors` `--source` / `--unresolved`: LocalFlags of `errors`.
- Root `-v, --version`: a root-level flag that outputs the one-line short version.

## version

Shows version and build metadata. The `internal/buildinfo` package normalizes version/build metadata once, and the `--version` flag and `version` subcommand share one `buildinfo.Info` snapshot.

```text
token-usage --version        # equivalent to -v; one-line short output
token-usage version          # multi-line detailed output
```

| Form | Output |
|------|------|
| `--version` (`-v`) | One line: `token-usage <version>\n`, for example `token-usage v0.1.0`; local development shows `token-usage dev`. |
| `version` | Strict five-line detailed output (with a trailing newline): `token-usage <version>` / `commit: <hash>` / `build_time: <time>` / `go: <go-version>` / `platform: <os>/<arch>`. |

Example detailed output from a release build:

```text
token-usage v0.1.0
commit: 59a8d55a1b2c
build_time: 2026-07-30T10:00:00Z
go: go1.26.4
platform: darwin/arm64
```

- `commit` displays the first 12 characters of the full revision; a modified worktree (`vcs.modified=true`) appends `-dirty`.
- **Version-source precedence**: (1) Makefile `ldflags -X` injected `Version` → (2) `debug.ReadBuildInfo().Main.Version` under `go install @version` → (3) local default `dev`.
- **Commit source**: (1) injected `Commit` → (2) `vcs.revision` in `debug.BuildInfo` → (3) `unknown`. **`build_time` does not use `vcs.time`**, because that is commit time rather than build time; it only uses the injected value and is `unknown` when none is injected.
- For a direct `go build` without injected ldflags, version is `dev`, build time is `unknown`, and commit falls back to the Go VCS revision when it is embedded. The temporary cached executable used by direct `go run` may not contain VCS metadata, in which case commit is `unknown`; use a Makefile target to validate complete build metadata.
- This is a purely static command: it does not read configuration, open the database, initialize logging, or access the network.
- Root `--help` shows both the `version` subcommand and the visible `-v, --version` flag.

Examples:

```bash
token-usage --version        # one-line short output
token-usage -v               # same as above
token-usage version          # multi-line detailed output
```

## completion

Generates a shell completion script. The script is written to standard output and can be saved or loaded using the installation method for the selected shell.

```text
token-usage completion <bash|zsh|fish|powershell>
```

For example, load it in the current zsh session:

```bash
source <(token-usage completion zsh)
```

For persistent installation instructions for each shell, run `token-usage completion <shell> --help`. This command reads no configuration, database, or data source.

## collect

Collects token-usage data. Before opening the database, `collect` and all of its subcommands run a **daemon-conflict precheck**: if the daemon is running and holds the daemon lock, collection is rejected to avoid concurrent database writes.

```text
token-usage collect [YYYYMMDD|YYYYMMDD-YYYYMMDD]
token-usage collect all
token-usage collect router --client <name>
token-usage collect retry
```

| Form | Purpose | Inherited flags |
|------|------|----------|
| `collect [date]` | Incrementally collects all enabled clients for today or a specified date; reads router logs and backfills attribution during collection. | `--client`, `--force` |
| `collect all` | Two-phase full collection: phase A collects all historical `messages` client by client (failure of one client does not stop the others); phase B fully backfills attribution for clients with a router configured. | `--client` |
| `collect router --client <name>` | Full router backfill only: does not call client collectors, write `collection_log`/`collection_errors`, or advance a cursor. | `--client` (**required**) |
| `collect retry` | Retries unresolved `collection_errors` records, recollecting each `(date, source)` group. | `--client` |

Flags:

- `--client <name>`: limits work to one client. The client must exist in configuration and have `enabled=true`; an unknown client and a disabled client each produce a distinct error. Valid values are the configured client-section names: `claude`, `opencode`, `codex`, `workbuddy`, `zcode`, and `autoclaw`.
- `--force` (only on `collect [date]` itself): forces recollection and ignores `collection_log` deduplication. Subcommands do not accept this flag.

Key points:

- `collect all` already includes router backfill, so normally there is no need to run `collect router` separately.
- The `--client` passed to `collect router` must have a configured router (`clients.<name>.router` is non-empty), otherwise it fails as “router not configured.”
- `collect [date]` with no date only collects today; use `collect all` for all history.
- If the precheck detects a running daemon, the command reports that the daemon is maintaining the data and exits nonzero.
- Collection failures are summarized by client/phase, and any failure yields a nonzero exit. When some sources fail, successfully parsed data is still stored, but `collection_log` is not written, old errors are not resolved, and incremental cursors are not advanced; a later normal collection or retry idempotently replays the range.

Examples:

```bash
# Collect today for every enabled client, including router processing
token-usage collect

# Collect a specified date range
token-usage collect 20260701-20260721

# Full historical collection, including the router phase
token-usage collect all
token-usage collect all --client claude

# Backfill router attribution for one client only
token-usage collect router --client claude

# Retry failed groups
token-usage collect retry
token-usage collect retry --client codex
```

## query

Queries token-usage statistics. Output is always a table (there is no `--format`) and is aggregated directly from `messages`, without a materialized summary table.

```text
token-usage query [date]
token-usage query client [date]     # default view; bare query is equivalent
token-usage query model [date]
token-usage query project [date]
token-usage query sessions [date]
token-usage query summary [date]
```

The default date is today. If the queried date range has unresolved entries in `collection_errors`, the results end with a collection-error notice and list the affected entries. Use `errors` for details and `collect retry` to retry them.

Examples:

```bash
token-usage query                    # today, grouped by client
token-usage query model              # today, grouped by model
token-usage query 20260701-20260721  # date range, grouped by client
token-usage query summary 20260701   # single-day overview
```

## errors

Displays collection errors.

```text
token-usage errors [YYYYMMDD]
```

- With neither a date nor `--source`, only **unresolved** errors are shown by default.
- With a date or `--source`, **all states** (including resolved) are shown by default.
- `--unresolved` explicitly requests unresolved errors only and always takes effect.

Flags:

- `--source <name>`: filters by data source (`claude`, `opencode`, `codex`, `workbuddy`, `zcode`, or `autoclaw`).
- `--unresolved`: shows unresolved errors only.

Examples:

```bash
token-usage errors                     # unresolved errors
token-usage errors 20260721            # all errors for one date
token-usage errors --source codex      # all errors for one source
token-usage errors --unresolved        # explicitly unresolved only
```

## config

Configuration management.

```text
token-usage config                     # open the interactive configuration TUI
token-usage config show                # output complete effective TOML (read-only, pure TOML)
token-usage config get <key>           # read one configuration value (dotted key, raw user-layer value)
token-usage config set <key> <value>   # write one configuration value
token-usage config init                # initialize the configuration file and database
```

> `config get` and `config show` have distinct roles. The former reads a raw user-configuration value without expanding `~` or filling defaults; the latter outputs complete effective TOML after expanding `~` and filling defaults/default paths. Prefer `config show` to inspect runtime-effective configuration; `status` and the TUI are human-readable summaries only.

### config (TUI)

With no arguments, opens the interactive configuration TUI (`bubbletea`). If no configuration file exists, it first writes the default template, then opens the UI. You can edit clients, routers, daemon settings, logs, and provider aliases; `data_dir` is read-only in the TUI. Saving always goes through `ApplyConfig` (see [config set](#config-set)).

### config show

Outputs the complete **effective configuration** (read-only, pure TOML).

```text
token-usage config show
```

- **effective**: runtime-effective values after expanding a `~` prefix; filling core defaults for `data_dir`, `daemon`, and `log`; and filling registry default paths for clients and routers. These are the values the daemon actually uses.
- **pure TOML**: the first character of stdout is TOML content, with no title/prompt/warning prefix. It can be piped directly to a TOML parser or redirected to a file for scripts.
- **read-only, zero runtime side effects**: does not modify the user configuration on disk; creates no configuration/database/log/daemon metadata; acquires no process lock; and does not synchronize autostart.
- **single parsing path**: reuses `cli.loadConfig()` → `runtimecfg.LoadEffectiveConfig`; it does not duplicate defaulting logic.
- A missing, empty, corrupted, or invalid configuration returns a clear error and nonzero exit code.
- **Path privacy**: output contains local paths. `~` is expanded; explicitly relative paths and their derived defaults remain relative (for example, `log.dir` derived from `data_dir` and `sessions_dir` derived from `state_dir`); other home-based defaults are absolute. Check for sensitive information before sharing.
- **Do not overwrite configuration with it directly**: the output is not a template intended to replace user configuration. It contains populated defaults, so writing it back would freeze default paths and discard comments.

### config get

Reads one configuration value by dotted key, such as `daemon.poll_interval` or `clients.claude.enabled`.

It reads the **raw user-configuration value**: the value explicitly written in the configuration file, without expanding `~`, filling default paths, or clamping numeric values. Therefore, fields that are not explicitly written return their zero value (for example, an absent `poll_interval` returns `0`). Use `config show` to inspect the full effective runtime configuration with expanded paths and defaults; `status` and the TUI are human-readable summaries only.

### config set

Writes one configuration value by dotted key, designed for scripts. `configapp.ApplyConfig` completes the write atomically **under the process-control lock**.

```
token-usage config set <key> <value>
token-usage config set <key> <value> --confirm-migrate   # only when migrating data_dir
```

**Output contract (for scripts):**

- The stable success line `✓ <key> = <value>` goes to **stdout**.
- Action suggestions (restart / collect), explanations, and warnings go to **stderr**.
- Exit code: `0` for success; `1` for any failure.

**Revision-conflict protection:** the configuration revision read at command start must match the disk revision reread under the lock. A mismatch means “configuration was changed by another process; this operation did not write”; no success line is written to stdout and the command exits nonzero. **Run the command again directly after a conflict**: it automatically rereads the latest configuration and recalculates the revision, so no manual intervention is needed.

**Partial failure:** if configuration has been persisted but autostart synchronization or stale cleanup fails, stdout still receives the stable success line, stderr reports the exact failure, and the command exits nonzero. A persisted result is never described as a complete failure.

**Full rewrite:** when configuration actually changes, both `config set` and the TUI serialize the entire user configuration file; existing comments and map-key ordering are not preserved. Back up handwritten notes first.

**`data_dir` migration:** changing `data_dir` requires `--confirm-migrate`, and the old daemon **must be stopped** (the command rejects a running daemon before writing). Move `usage.db` and `logs` manually; PID/lock/runtime-state are not migrated and are cleaned by the stale protocol.

### Supported dotted keys

| Area | Writable keys |
|------|----------|
| Data directory | `data_dir` (requires `--confirm-migrate`) |
| Daemon | `daemon.poll_interval`, `daemon.autostart` |
| Logging | `log.level`, `log.dir`, `log.max_days` |
| Client | `clients.<name>.enabled`, `clients.<name>.router`, `clients.<name>.paths.<path-key>` |
| Router | `routers.cc_switch.db_path` |
| Provider aliases | `provider_aliases.<raw-provider-name>` |

Supported clients are `claude`, `opencode`, `codex`, `workbuddy`, `zcode`, and `autoclaw`. Their path keys are: Claude `projects_dir`; OpenCode `db`; Codex `state_dir`/`sessions_dir`; WorkBuddy `db`/`projects_dir`; ZCode `db`; AutoClaw `sessions_dir`.

`provider_aliases` normalizes the raw provider name backfilled by CC Switch to a display name. After changing one, follow the command suggestion to run router backfill again. When a name contains `.`, use a quoted segment, for example:

```bash
token-usage config set 'provider_aliases."Zhipu AI Coding Plan"' 'Zhipu GLM'
```

### Autostart semantic boundary (important)

`config set daemon.autostart <bool>` (or the TUI toggle) only **synchronizes the autostart service definition** (a macOS plist or Windows Registry Run key). It **never starts or stops the current daemon**:

- Enabling autostart writes the definition and leaves the current daemon unchanged; the new definition loads at the next login/boot.
- Disabling autostart deletes the definition and leaves the current daemon running; it no longer starts at the next login/boot.

To apply it in the current session, manually run `stop` then `start` (or `restart`). See [Daemon Lifecycle](#daemon-lifecycle) for the full explanation of this decoupling.

### config init

Initializes the configuration file at the fixed path `~/.token-usage/config.toml` and the database at `<data_dir>/usage.db`.

- Configuration file: writes the default template only if the file does not exist (idempotent; does not overwrite existing configuration).
- Database: always initializes `usage.db`, even when configuration already exists.
- `data_dir` uses the value in existing configuration; when the field is not explicitly configured, the default directory is `~/.token-usage`. If existing configuration cannot be parsed or fails validation, the command fails rather than silently overwriting it.

Examples:

```bash
token-usage config init
token-usage config get daemon.poll_interval
token-usage config set daemon.autostart true
token-usage config set clients.zcode.enabled true
```

## Daemon Lifecycle

`start` / `stop` / `restart` / `status` manage only the **currently running daemon** (the real-time collection/analysis monitor) and are fully decoupled from the **autostart definition** (whether it starts automatically at the next login/boot).

| Command | Purpose | Touches the autostart definition? |
|------|------|--------------------------|
| `start` | Starts the daemon in the background and returns after the monitor-ready handshake; if already running, idempotently returns the current PID. | No |
| `stop` | Stops the current daemon without deleting the plist/Registry definition; idempotent when not running. | No |
| `restart` | Stops the old daemon and starts a new one under one process-control lock; fails and suggests `start` when none is running. | No |
| `status` | Read-only runtime inspection plus five-state autostart drift detection. | No (read-only) |

> None of these commands modifies configuration, a plist, or the Registry. The autostart definition converges through `config set daemon.autostart` or a TUI save.

### start

```text
token-usage start
```

Through `control.Manager.Start`: load configuration under the process-control lock → determine liveness from the daemon lock → if already running, return the current PID without spawning again (exit code 0) → otherwise detached-spawn `_run` → wait up to five seconds for six readiness conditions (PID/instanceID in the PID file, the daemon lock, PID/instanceID/`monitor_ready=true` in runtime-state) → print a success line containing the PID. On timeout, it tries to terminate only the new child and cleans metadata only when the daemon lock is released and the metadata still belongs to this generation, avoiding deletion of a live process or another generation's files.

stdout contains success lines, including the idempotent already-running result; stderr contains actual failures.

### stop

```text
token-usage stop
```

Through `control.Manager.Stop`: load configuration under the process-control lock → determine liveness from the daemon lock → if not running, return the idempotent not-running result → if running, stop by platform (macOS always first idempotently tries `bootout` for the current label; if the daemon lock remains held, sends SIGTERM to the exact read PID; Windows uses `taskkill` on the exact PID) → define success as **daemon lock released** (polling for five seconds), never by deleting a PID file to simulate success.

`stop` **does not delete** the plist/Registry definition: the current session stops, while the next login follows the autostart configuration. Disable autostart with `config set daemon.autostart false`.

### restart

```text
token-usage restart
```

Through `control.Manager.Restart`, it stops the old daemon and starts a new one under one process-control lock. If the daemon is **not running**, it returns `ErrRestartNotRunning`, writes a suggestion to use `token-usage start` to stderr, and exits nonzero.

macOS tradeoff: `stop` attempts to `bootout` the current job and then `start` runs it detached; the plist definition remains, but launchd KeepAlive no longer manages it for the current login session. Because saving configuration only maintains the definition file and does not proactively bootstrap it, KeepAlive resumes when the definition is loaded at the next login.

### status

```text
token-usage status
```

Read-only: `Inspect` does not acquire the process-control lock and determines liveness only from the daemon lock. It returns a consistent snapshot containing:

- Runtime state: running with a PID, or not running.
- Startup phase (an extra line when running): monitor initialization / monitoring ready and catch-up in progress / partial catch-up failure with a count and a suggestion to run `token-usage errors`. A successful catch-up adds no extra line; unavailable PID metadata or phase mismatch degrades to an unknown startup phase.
- Data directory and polling interval.
- Five-state autostart drift detection: enabled / autostart on but definition missing / content differs / autostart off but definition remains / not enabled. Drift only suggests saving configuration again; it triggers no writes.

Autostart expresses only whether the daemon starts at the next login/reboot and is independent from whether the current daemon is running. The current runtime state is displayed separately; neither is inferred from the other.

### Startup catch-up (closes the stop → collect → start data window)

After monitoring is established by `start`, the daemon performs **startup catch-up** to collect data created between the last manual `collect`/`collect all` and monitor readiness, closing the stop → collect → start data window.

Ordering contract (`daemon.startupCoordinator`):

1. Wait for every analyzer monitor to be ready (ready barrier); if the context is canceled, write no state and perform no catch-up.
2. Write ready state (`monitor_ready=true, catch_up=pending`).
3. Write running state (`catch_up=running`); if the write fails, log it and continue without stopping the daemon.
4. Submit catch-up requests in order: enabled client names ascend; each client first gets a client-source request (opencode/zcode use incremental cursors; claude/workbuddy/autoclaw scan existing JSONL without a date; Codex does state incremental collection first and then a full rollout scan), then receives its router incremental request if configured.
5. Write final state: zero failures means `succeeded`; otherwise `failed` with the exact failure count.

Catch-up is submitted through the analyzer serialization lock (the same path as real-time triggers, guaranteeing ordering and mutual exclusion). Therefore, if the daemon starts successfully and completes catch-up, incremental data generated between stop → collect → start is collected and is not missed because monitoring was not ready. Partial catch-up failures appear in `status` and `errors`.

### _run (hidden)

An internal command started by `start` through detached spawn or directly by launchd / a Windows Registry Run key. It executes the daemon main loop and must not be invoked by users (it is absent from `--help`). Both startup paths satisfy the invariant that “a control lease exists continuously from reading effective configuration through acquiring the daemon lock”:

- Parent-lease path (`_run` spawned by `start`): the parent holds the process-control lock and authorizes the child through a pipe lease; the child does not acquire the lock.
- Independent path (started directly by launchd/the Registry): without a valid parent lease, it acquires the process-control lock itself (15-second timeout). On timeout it exits successfully with code 0 rather than entering the main loop, avoiding conflict with an in-progress control operation and preventing launchd KeepAlive from immediately relaunching it on macOS.

## update

Updates the `token-usage` binary in place from official GitHub Releases. The CLI only parses flags, assembles dependencies, and formats results; the self-update core lives in `internal/update` (see [Architecture](architecture.md)).

```text
token-usage update
token-usage update --check
token-usage update --version <tag>
```

| Form | Purpose |
|------|---------|
| `update` | Updates to the latest stable release. If a restricted transaction journal from an interrupted POSIX update exists beside this binary, it is recovered first; a new replacement then proceeds only when the target is strictly higher than the current version and the current source is trusted. It downloads the asset, verifies its SHA256 against the `SHA256SUMS` manifest, stages a `--version` second check, replaces the binary, and restores the daemon to its previous run state. |
| `update --check` | Read-only check; creates no local files (no configuration directory, lock, log, database, or service definition). |
| `update --version vX.Y.Z` / `update --version vX.Y.Z-rc.N` | Updates (or, with `--check`, only checks) the specified exact release tag. `--version` accepts a strict release tag (`v` prefix, `MAJOR.MINOR.PATCH`, optional `-rc.N`, no leading zeros); an invalid value errors before any network request. |

`--check` and `--version` may be combined, for example `update --check --version v0.1.0-rc.1` checks a release candidate only.

Flags:

- `--check` (bool): read-only check; writes no local files.
- `--version` (string): target release tag. Accepts `vMAJOR.MINOR.PATCH` and `vMAJOR.MINOR.PATCH-rc.N` (no leading zeros, `N >= 1`, no build metadata).

`update` takes no positional arguments (`Args: NoArgs`).

### Stable / release-candidate selection

By default `update` resolves only the latest **stable** release and never selects a prerelease. A release candidate is consulted or installed only when you pass its tag explicitly with `--version` (for example `--version v0.1.0-rc.1`).

### Trust and source verification

`update` only replaces the current binary when the target is strictly higher and the current source is trusted. The current source is treated as **untrusted** (and `update` prints manual-install guidance instead of overwriting) when any of the following holds:

- the current `Version` is `dev` or a pseudo-version (e.g. from `make build`, `make build-all`, or `go install`);
- the current binary is not a regular file, or is a symlink;
- the current binary's SHA256 does not match the official asset hash for the current version.

The sole trusted repository is `YuLaiZ/token-usage`; see [Architecture](architecture.md) for the download-URL reconstruction, manifest, and staged-install trust chain.

This source gate applies to a new replacement. Recovering an already recorded local transaction does not download or accept a new source: it uses only same-directory paths derived from the executable and journal nonce, and rechecks the recorded hashes before restoring a consistent state.

### Exit codes

- `0` for expected completed states: no stable release available, already up to date, an update is available (`--check`), a Windows background replacement has been queued, or recovery confirms that the interrupted update had already installed the new binary.
- Non-zero when the requested tag does not exist, the current source cannot be safely overwritten, download/manifest/checksum/staged-`--version` validation is rejected, recovery returns the binary to the old version, installation is incomplete, install/rollback/daemon-restart fails, or `--version` is invalid.

### Side-effect boundary

`update --check` is fully read-only. A real `update` first resolves an existing transaction journal when present; otherwise it stops the daemon, replaces the binary, and restarts it only when an update is available and the source is trusted. It does not start or stop the daemon when no update is needed, and it does not rewrite `config.toml`, the database, logs, the macOS LaunchAgent plist, or the Windows Registry.

### Windows asynchronous replacement

Replacing a running `.exe` is restricted on Windows, so the update hands the replacement off to a background helper and returns. Once that helper has been started, the command explicitly reports that the background replacement has been queued, exits `0`, and asks you to run `token-usage version` or `token-usage update --check` shortly to confirm the final version; it never claims completion. On macOS/POSIX the replacement is synchronous and atomic (same-directory backup + rename + fsync, with rollback on failure and journal recovery on the next `update` invocation).

## Configuration File

The fixed path is `~/.token-usage/config.toml` (TOML; comments may be added manually). Defaults work out of the box: a client only needs `enabled = true`; the program fills data-source paths from each tool's default locations. Override defaults with a dotted key in the same section style. `config set` and TUI saves fully rewrite configuration, so existing comments and map-key ordering are not retained; see the template generated by `token-usage config init` for the complete fields and defaults.

`data_dir` determines locations for data files (`usage.db`, logs, PID, runtime-state, and locks); the configuration-file path does not change with `data_dir`. `daemon.autostart` controls autostart (macOS launchd / Windows Registry).
