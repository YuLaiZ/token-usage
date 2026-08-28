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
│   ├── session [YYYYMMDD|YYYYMMDD-YYYYMMDD]# session details
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
├── update                                # self-update from official GitHub Releases (--check / --version / --force)
└── _run                                  # hidden; started by start/launchd/the Registry; do not invoke directly
```

Design points:

- There is no top-level `router` subcommand. Router attribution is reached through `collect all` (included) or `collect router` (attribution layer only).
- Dates are **positional arguments**: a single day is `YYYYMMDD`; an inclusive range is `YYYYMMDD-YYYYMMDD`. There is no `--date` flag. `errors` accepts only a single `YYYYMMDD`.
- `query` has no `--format` or `--by-*` flag. A subcommand selects the view and output is always a table.
- Running `token-usage` with no arguments only prints help; it starts neither the TUI nor the daemon.
- The root command has a `-v, --version` flag for one-line short output and a `version` subcommand for multi-line detailed output; see [version](#version).
- `completion` is Cobra's built-in command. It writes bash/zsh/fish/PowerShell completion scripts to standard output and reads neither configuration nor the database.
- `update` is a top-level self-update command (flags `--check`, `--version`, and `--force`); it is the only command that rewrites the running binary. By default it does so only when the current binary is an official Release asset; `--force` opts in to overwriting a re-signed official asset, a `go install` of a tagged version, or a dev build. See [update](#update).

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
| `--version` (`-v`) | One line: `token-usage <version>\n`; local development shows `token-usage dev`. |
| `version` | Strict five-line detailed output (with a trailing newline): `token-usage <version>` / `commit: <hash>` / `build_time: <time>` / `go: <go-version>` / `platform: <os>/<arch>`. |

Example detailed output from a release build:

```text
token-usage <version>
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
token-usage query                      # today, runs query.default; equivalent to client when unconfigured
token-usage query <date>               # date or range for the default view
token-usage query client [date]        # built-in views
token-usage query model [date]
token-usage query provider [date]
token-usage query project [date]
token-usage query session [date]
token-usage query summary [date]
token-usage query <name> [date]        # direct shorthand for a configured subquery or group
token-usage query custom <name> [date] # explicit equivalent of the line above; kept as-is
token-usage query list                 # lists configured views; reads config only, never opens the database
```

Every date-based query command starts its output with a shared statistics header, printed exactly once no matter how many tables follow (`query list` reads config only and has no header):

```text
Usage statistics / 使用统计
Units / 单位:
  1 K = 1,000 (thousand / 一千)
  1 M = 1,000 K = 1,000,000 (million / 一百万)
  1 B = 1,000 M = 1,000,000,000 (billion / 十亿)
Query range / 统计范围: 2026-07-01 ~ 2026-07-21
Data through / 数据截至: 2026-07-21 23:59:59
Last successful collection / 最近成功采集: 2026-07-22 08:15:03
```

- `Units / 单位` states the abbreviations used for token counts in every table and the summary: values are shown in K, M, or B once they reach 1,000, 1,000,000, or 1,000,000,000, always with two decimal places.
- `Query range / 统计范围` echoes the actual date argument: a single day alone, or an inclusive range as `YYYY-MM-DD ~ YYYY-MM-DD`.
- `Data through / 数据截至` is the latest message-event timestamp (`messages.ts`) inside the queried range, displayed in local time to the second. Message event times are the temporal boundary of the statistics; the field shows `—` when the range contains no messages.
- `Last successful collection / 最近成功采集` is the most recent successful collection completion time in the whole database (`collection_log.collected_at`, stored in UTC and displayed in local time). It does not imply that every client was collected up to that moment, and it shows `—` before any successful collection exists.

The default date is today. If the queried date range has unresolved entries in `collection_errors`, the results end with a collection-error notice and list the affected entries; when several tables are output (a group), the notice appears once after all of them. Use `errors` for details and `collect retry` to retry them.

Every grouped view (the four built-in views and every custom multi-dimensional table) ends with a `Total / 总计` row computed from the same date range as the table; session details and the summary do not have this row.

### Configurable query views

The optional `[query]` section configures what the bare `query` runs and which custom views exist:

```toml
[query]
default = "group_q"                    # unconfigured or whitespace means client

[query.subqueries]
mpc = "model,provider,client"          # one multi-dimensional table

[query.groups]
group_q = "client,model,provider,mpc"  # several tables in this order
```

- `query <name> [date]` and `query custom <name> [date]` are equivalent spellings for the same configured subquery (one table) or group (tables in declared order): same target and same output, validated under the same rules — name resolution, reserved-name rejection, date validation order (date errors take precedence over name/definition errors), and every failure happening before the database opens. Error examples naturally show each spelling's own command form (`token-usage query 20260701` vs `token-usage query custom 20260701`). The direct name is positional argument dispatch on the root `query` command — configured names never become dynamic subcommands. With two positional args the first must be the view name; a digit-leading first arg (`token-usage query 20260701 20260702`) is rejected before the config is loaded with a bilingual usage error naming both accepted forms.
- Unknown or reserved names are rejected before the database is opened, and date errors take precedence over name/definition errors; both surface in either spelling.
- A subquery selects at least 2 distinct built-in dimensions (`client`/`model`/`provider`/`project`); the declared order is the column order. A group selects at least 2 distinct items from built-in views plus defined subqueries; groups cannot reference groups.
- View names are lowercase identifiers (a letter first, then letters, digits, `_`, `-`) and must not collide with `client`/`model`/`provider`/`project`/`session`/`summary`/`custom`/`list`. Values are comma-separated; every segment is trimmed, so `"model, provider"` equals `"model,provider"`. If a handwritten subquery or group was named `list`, rename it before upgrading: newer binaries reject the name because `query list` became a static discovery command.
- `query.default` is matched after trimming; whitespace means "use client". It may reference a built-in view, a subquery, or a group; `session` and `summary` are not referable.
- `query list` takes no positional args and prints a fixed structure in one pass: default behavior (`token-usage query -> <name> (<category>)`), one-time invocation hint showing the direct and explicit forms as equivalent, six built-in commands with their purposes, then every configured subquery and group as a single copy-pasteable command for today (such as `token-usage query mpc`) together with its dimensions or members CSV; empty sections say `None`. It only reads the effective config and parses definitions — it never opens `usage.db`, prints statistics, reads collection errors, accepts a date, or changes any state. Bad definitions still fail there with the same localized errors instead of being hidden behind an empty section.

`query provider` (and the provider dimension of any custom view) prefers router attribution, then the collector's provider value. Historical empty values remain unattributed; the query does not infer a provider from the client. `provider_aliases` is applied before composite keys are formed: aliases with the same value are combined into one row in every view, without changing `usage.db`.

Query configuration is display-only. Semantic errors (broken references, malformed CSV, unknown keys, top-level conflicts such as `[query]` alongside `[Query]`, or a non-table root like `query = "x"`) make the default path (bare `query` and `query <date>`), every named invocation (`query <name>` / `query custom <name>`), `query list`, and TUI saves fail with the offending key; the six static built-in views, `collect`, `status`, `start`, the daemon, `config set`, and `config show` keep working and preserve the offending entries. The TUI "Query views" page (press `v` in the main menu) edits this section with guided selection and shows a recovery list when the raw section cannot be parsed. Before downgrading to a version without query-view support, remove the whole `[query]`, `[query.subqueries]`, and `[query.groups]` sections: older versions reject any non-empty query section.

Examples:

```bash
token-usage query                    # today, runs the configured default (client when unconfigured)
token-usage query 20260701-20260721  # date range, default view
token-usage query mpc                # today, the mpc multi-dimensional table (direct shorthand)
token-usage query custom group_q 20260701  # explicit spelling: four tables in declared order
token-usage query summary 20260701   # single-day overview
token-usage query list               # list configured views without touching the database
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

With no arguments, opens the interactive configuration TUI (`bubbletea`). If no configuration file exists, it first writes the default template, then opens the UI. You can edit clients, routers, daemon settings, logs, and provider aliases; `data_dir` is read-only in the TUI. Saving always goes through `ApplyConfig` (see [config set](#config-set)). Clients outside the router-capable family (currently every client except Claude) show no router field; an existing non-empty router value on such a client is still displayed so it can be cleared, and saving rejects a non-empty value (see the router guard under [config set](#config-set)).

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

**Router guard:** `config set clients.<name>.router <value>` fails before writing when `<value>` is non-empty and `<name>` is not a router-capable client (currently Claude only); the command exits nonzero. Setting an empty value clears the router and is always allowed. Read paths (`config show`, collection, the daemon) keep tolerating a non-empty router on other clients in existing configurations.

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

`provider_aliases` changes labels and grouping only in `query provider`; it does not alter collected or router-backfilled data, and takes effect on the next query. When a name contains `.`, use a quoted segment, for example:

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
- When a new configuration file is created, the completion notice states that no client is enabled by default and prints an enable example (`token-usage config set clients.<name>.enabled true`). The default template ships with every client `enabled = false`, a commented `router` line, and a commented provider-alias example.

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
token-usage update --force
```

| Form | Purpose |
|------|---------|
| `update` | Updates to the latest stable release. If a restricted transaction journal from an interrupted POSIX update exists beside this binary, it is recovered first; a new replacement then proceeds only when the target is strictly higher than the current version and the current source is trusted. It downloads the asset, verifies its SHA256 against the `SHA256SUMS` manifest, stages a `--version` second check, replaces the binary, and restores the daemon to its previous run state. |
| `update --check` | Read-only check; creates no local files (no configuration directory, lock, log, database, or service definition). |
| `update --version vX.Y.Z` / `update --version vX.Y.Z-rc.N` | Updates (or, with `--check`, only checks) the specified exact release tag. `--version` accepts a strict release tag (`v` prefix, `MAJOR.MINOR.PATCH`, optional `-rc.N`, no leading zeros); an invalid value errors before any network request. |
| `update --force` | Overwrites the current binary even when its source is not an official Release asset, for exactly two exemptions: a hash mismatch against the official asset of the reported version (a binary re-signed per the install guide, or `go install pkg@vX.Y.Z`), and a dev local build (`Version = dev`; plain-build pseudo-versions are normalized to `dev`). All structural checks and the target asset's SHA256 / staged `--version` verification still run; symlinked copies and non-official tags cannot be forced. |

`--check` and `--version` may be combined; for example, `update --check --version vX.Y.Z-rc.N` checks a release candidate only. `--force` cannot be combined with `--check` (that combination is rejected explicitly).

Flags:

- `--check` (bool): read-only check; writes no local files.
- `--version` (string): target release tag. Accepts `vMAJOR.MINOR.PATCH` and `vMAJOR.MINOR.PATCH-rc.N` (no leading zeros, `N >= 1`, no build metadata).
- `--force` (bool): overwrite even if the current binary is not an official Release asset (re-signed, `go install`, or a dev build); see [trust and source verification](#trust-and-source-verification) for the exact exemption boundary.

`update` takes no positional arguments (`Args: NoArgs`).

### Stable / release-candidate selection

By default `update` resolves only the latest **stable** release and never selects a prerelease. A release candidate is consulted or installed only when you pass its tag explicitly with `--version` (for example `--version vX.Y.Z-rc.N`).

### Trust and source verification

`update` only replaces the current binary when the target is strictly higher and the current source is trusted. The current source is treated as **untrusted** (and a plain `update` refuses to overwrite, printing manual-install guidance instead) when any of the following holds:

- the current `Version` is `dev` or a pseudo-version (e.g. from `make build`, `make build-all`, or `go install`);
- the current binary is not a regular file, or is a symlink;
- the current binary's SHA256 does not match the official asset hash for the current version (e.g. a binary re-signed per the install guide, or `go install pkg@vX.Y.Z`).

The refusal carries a `--force` escape hatch for exactly two exemptions:

- **hash mismatch** (the current version has an official Release and manifest, but the local content differs): re-running with `--force` overwrites the binary with the official asset, so automatic updates resume;
- **dev build** (`Version = dev`; plain-build pseudo-versions are normalized to `dev`, so this is the only dev form `update --force` accepts; no comparable official Release or manifest exists, so no hash comparison ever happened): `update --force` switches the installation to the official release asset.

Symlinked copies and non-official tags cannot be forced — every other refusal reason always requires manual installation. `--force` never skips any check: structural checks still gate the replacement, and the target asset is still downloaded, SHA256-verified against `SHA256SUMS`, and stage-checked with `--version` before it may replace the current binary. A `--force` run is reported as successful with a `--force` note and exits 0; it is never reported as trusted.

On macOS the refusal message distinguishes a locally ad-hoc signed binary (detected via a signature probe) and names the re-signed-official-asset possibility explicitly; on other platforms and whenever the probe is unavailable, the generic message still lists re-signing among the possible causes and mentions the same `--force` exit.

The sole trusted repository is `YuLaiZ/token-usage`; see [Architecture](architecture.md) for the download-URL reconstruction, manifest, and staged-install trust chain.

This source gate applies to a new replacement. Recovering an already recorded local transaction does not download or accept a new source: it uses only same-directory paths derived from the executable and journal nonce, and rechecks the recorded hashes before restoring a consistent state.

### Exit codes

- `0` for expected completed states: no stable release available, already up to date, an update is available (`--check`), a Windows background replacement has been queued, or recovery confirms that the interrupted update had already installed the new binary.
- Non-zero when the requested tag does not exist, the current source is unverified and `--force` was not given (hash mismatch or dev build) or cannot be forced at all (symlink / non-official tag), download/manifest/checksum/staged-`--version` validation is rejected, recovery returns the binary to the old version, installation is incomplete, install/rollback/daemon-restart fails, or `--version` is invalid.

### Side-effect boundary

`update --check` is fully read-only. A real `update` first resolves an existing transaction journal when present; otherwise it stops the daemon, replaces the binary, and restarts it only when an update is available and the source check passes — trusted, or overridden with `--force`. It does not start or stop the daemon when no update is needed, and it does not rewrite `config.toml`, the database, logs, the macOS LaunchAgent plist, or the Windows Registry.

### Windows asynchronous replacement

Replacing a running `.exe` is restricted on Windows, so the update hands the replacement off to a background helper and returns. Once that helper has been started, the command explicitly reports that the background replacement has been queued, exits `0`, and asks you to run `token-usage version` or `token-usage update --check` shortly to confirm the final version; it never claims completion. On macOS/POSIX the replacement is synchronous and atomic (same-directory backup + rename + fsync, with rollback on failure and journal recovery on the next `update` invocation).

## Configuration File

The fixed path is `~/.token-usage/config.toml` (TOML; comments may be added manually). All clients are disabled by default: enable the ones you use with `clients.<name>.enabled = true`, and the program fills data-source paths from each tool's default locations. Override defaults with a dotted key in the same section style. `config set` and TUI saves fully rewrite configuration, so existing comments and map-key ordering are not retained; see the template generated by `token-usage config init` for the complete fields and defaults.

`data_dir` determines locations for data files (`usage.db`, logs, PID, runtime-state, and locks); the configuration-file path does not change with `data_dir`. `daemon.autostart` controls autostart (macOS launchd / Windows Registry).
