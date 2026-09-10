# CLI Reference

> [简体中文](cli.zh-CN.md) | English

This document is the authoritative reference for the `token-usage` command-line interface: command tree, positional arguments, flags, exit codes, and examples. Source-code behavior takes precedence over this document.

## Command Tree

```text
token-usage
├── version                               # show version and build metadata (five-line detailed output)
├── completion [bash|zsh|fish|powershell] # generate a shell completion script
├── collect [DATE|DATE-DATE]              # incremental collection for today or a specified date (day/month/year; includes router)
│   ├── all                               # two-phase full collection: all historical messages + full router backfill
│   ├── router --client X                 # full router backfill only (does not touch messages)
│   └── retry                             # retry unresolved groups in collection_errors
├── query [<name> [DATE|DATE-DATE] | DATE|DATE-DATE]
│   ├── client [DATE|DATE-DATE]    # group by client (default view)
│   ├── model [DATE|DATE-DATE]     # group by model
│   ├── provider [DATE|DATE-DATE]  # group by provider
│   ├── project [DATE|DATE-DATE]   # group by project
│   ├── day [DATE|DATE-DATE]       # usage by day
│   ├── month [DATE|DATE-DATE]     # usage by month
│   ├── hour [DATE|DATE-DATE]      # usage by hour
│   ├── weekday [DATE|DATE-DATE]   # usage by weekday
│   ├── heatmap [DATE|DATE-DATE]   # weekday x hour heatmap
│   ├── session [DATE|DATE-DATE]   # session details
│   ├── summary [DATE|DATE-DATE]   # overview summary
│   ├── custom <name> [DATE|DATE-DATE] # explicit configured-view form
│   └── list                        # list configured views (config only; no database)
├── errors [DATE|DATE-DATE]
├── watch [DATE|DATE-DATE]                # refresh the query output at a fixed interval (view selection matches query)
├── doctor                                # read-only health check (config, data directory, database, clients, collection, errors)
├── daemon                                # manage the collection daemon (bare command only prints help)
│   ├── start                             # start the daemon in the background (nginx-style; spawns _run)
│   ├── status                            # show daemon status and config summary
│   ├── stop                              # stop the daemon
│   └── restart                           # stop the old daemon and start a new one under one lock
├── serve                                 # manage the local read-only dashboard HTTP server (bare command only prints help)
│   ├── start                             # run the dashboard in the background (nginx-style; logs to serve.log)
│   ├── status                            # show whether the background dashboard is running
│   ├── stop                              # stop the background dashboard
│   └── restart                           # stop the running dashboard and start a fresh background instance
├── config                                # no arguments: open the interactive configuration TUI
│   ├── show                              # output complete effective TOML (read-only, pure TOML)
│   ├── get <key>
│   ├── set <key> <value>
│   └── init
├── update                                # self-update from official GitHub Releases (--check / --version / --force)
├── _run                                  # hidden; started by daemon start/launchd/the Registry; do not invoke directly
└── _serve-run                            # hidden; background dashboard body spawned by serve start; do not invoke directly
```

Design points:

- There is no top-level `router` subcommand. Router attribution is reached through `collect all` (included) or `collect router` (attribution layer only).
- Dates are **positional arguments**: `DATE` is a day (`YYYYMMDD`), month (`YYYYMM`), or year (`YYYY`; single arg only); `DATE-DATE` is an inclusive range whose endpoints are days or months. Any form expands to at most 366 days. There is no `--date` flag. `errors` accepts the same forms.
- `query` has no `--format` or `--by-*` flag. A subcommand selects the view and output is always a table.
- Running `token-usage` with no arguments only prints help; it starts neither the TUI nor the daemon.
- The root command has a `-v, --version` flag for one-line short output and a `version` subcommand for multi-line detailed output; see [version](#version).
- `completion` is Cobra's built-in command. It writes bash/zsh/fish/PowerShell completion scripts to standard output and reads neither configuration nor the database.
- `update` is a top-level self-update command (flags `--check`, `--version`, and `--force`); it is the only command that rewrites the running binary. By default it does so only when the current binary is an official Release asset; `--force` opts in to overwriting a re-signed official asset, a `go install` of a tagged version, or a dev build. See [update](#update).

## Migrating from v0.1.8 (Breaking Changes)

The command surface was deliberately consolidated; scripts written against v0.1.8 must be updated:

- The six analysis commands `chart`, `compare`, `export`, `forecast`, `report`, and `top` were **removed** (no compatibility aliases). The visual analytics — SVG charts, usage forecasts, period comparisons, top-session rankings — live on in the `serve` dashboard over its read-only HTTP data surface, including page-scoped CSV export there; terminal reports remain `query` and `watch`. The machine-readable CLI export contract and the offline HTML report bundle were removed intentionally and have no full replacement.
- The top-level lifecycle commands moved into the `daemon` group: `token-usage start` → `token-usage daemon start`, likewise `status`, `stop`, and `restart`.
- `token-usage serve` no longer runs a foreground server: bare `serve` only prints the command-group help, and the dashboard is started with `serve start` / `serve restart`.

| v0.1.8 | now |
|---|---|
| `token-usage start` / `status` / `stop` / `restart` | `token-usage daemon start` / `daemon status` / `daemon stop` / `daemon restart` |
| `token-usage serve` (foreground) | `token-usage serve start` (background) |
| `token-usage chart` / `forecast` / `compare` / `top` | `token-usage serve start`, then open the dashboard |
| `token-usage export` / `report` | Terminal queries stay on `query` / `watch`; the dashboard offers page-scoped CSV export. The machine-readable CLI export and the offline report bundle are gone without a full replacement. |

## General Conventions

### Date Argument Format

| Command | Accepted form | Default |
|------|----------|------|
| `collect`, `query` (with subcommands), and `watch` | `DATE` (day `YYYYMMDD`, month `YYYYMM`, or year `YYYY`; year as a single arg only) or `DATE-DATE` (inclusive day/month endpoints) | Today |
| `errors` | `DATE` or `DATE-DATE` (same forms as `collect`/`query`) | With neither a date nor `--source`, only unresolved errors are shown. |

`YYYYMMDD` is an eight-digit compact format (for example, `20260701`); `YYYYMM` selects a calendar month and `YYYY` a calendar year (the year form is accepted only as a single arg). `YYYY-MM-DD`, extra positional arguments, a year used as a range endpoint, and an end date before the start date all fail with an error and command examples. A single arg or a range normalizes to an inclusive per-day list capped at 366 days (one leap year); split longer ranges into multiple runs.

### Exit Codes

`token-usage` maps a command error to an exit code in `main`:

- `0`: success, including idempotent results such as `daemon start` when the daemon is already running and `daemon stop` when it is not.
- `1`: any error (argument validation failure, collection/query failure, daemon-control failure, revision conflict, partial failure, and so on).

The stdout/stderr contract for success and failure is described in each command section.

### Flag Scope

- `--client`: a **PersistentFlag** of `collect`, inherited by its `all`, `router`, and `retry` subcommands.
- `--force`: a **LocalFlag** of `collect`, **not** inherited by subcommands (passing it to a subcommand returns an unknown-flag error).
- `errors` `--source` / `--unresolved` / `--format`: LocalFlags of `errors`.
- Root `-v, --version`: a root-level flag that outputs the one-line short version.

## version

Shows version and build metadata. The `internal/buildinfo` package normalizes version/build metadata once, and the `--version` flag and `version` subcommand share one `buildinfo.Info` snapshot.

```text
token-usage --version        # equivalent to -v; one-line short output
token-usage version          # multi-line detailed output
```

| Form | Output |
|------|------|
| `--version` (`-v`) | One line: `token-usage <version>\n`; local development shows `token-usage v0.1.8-dev` (normalized pseudo-version display). |
| `version` | Strict five-line detailed output (with a trailing newline): `token-usage <version>` / `commit: <hash>` / `build_time: <time>` / `go: <go-version>` / `platform: <os>/<arch>`. |

Example detailed output from a release build:

```text
token-usage <version>
commit: 59a8d55a1b2c
build_time: 2026-07-30 18:00:00
go: go1.26.4
platform: darwin/arm64
```

- `commit` displays the first 12 characters of the full revision; a modified worktree (`vcs.modified=true`) appends `-dirty`.
- **Version-source precedence**: (1) Makefile `ldflags -X` injected `Version` → (2) `debug.ReadBuildInfo().Main.Version` under `go install @version` → (3) local default `dev`.
- **Commit source**: (1) injected `Commit` → (2) `vcs.revision` in `debug.BuildInfo` → (3) `unknown`. **`build_time` does not use `vcs.time`**, because that is commit time rather than build time; it only uses the injected value and is `unknown` when none is injected.
- **`build_time` rendering**: the injected value is UTC RFC3339 (stamped by the release build); it is displayed in the local timezone as `YYYY-MM-DD HH:MM:SS`. Values that do not parse as RFC3339 are shown verbatim.
- **Local-build version display**: for a direct `go build` from a tagged checkout, `debug.ReadBuildInfo().Main.Version` is a Go pseudo-version such as `v0.1.8-0.20260908085159-1280e3f00e99`; it is normalized to `v0.1.8-dev` for display (the base version indicates the release this checkout is heading toward, and the `commit` line keeps the exact revision). Checkouts before the first tag display `v0.0.0-dev`. Two boundary forms keep the toolchain value verbatim instead of normalizing to `-dev`: building exactly on a tag commit shows `v<tag>` (e.g. `v0.1.8`), and the same with a modified worktree shows `v<tag>+dirty` (e.g. `v0.1.8+dirty`). The `-dev` display shares the semantics of the literal `dev` with the update guard: `update` refuses it and `update --force` is required to switch to an official release asset.
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

Per-shell prerequisites:

- **zsh** needs the completion system (`compinit`) initialized first. If loading the script fails with `compdef: command not found`, `compinit` has not run: add `autoload -U compinit` and `compinit` to your rc file (e.g. `~/.zshrc`) before the load line. Some setups intentionally skip `compinit` to keep startup quiet; check your rc file before editing.
- **bash** needs the bash-completion package (the generated script relies on its `_init_completion`). macOS ships bash 3.2 without the package: install bash 4+ and `bash-completion@2` via Homebrew, then restart the shell.
- **fish** and **PowerShell** have no prerequisites: write the script to the shell's completion location and it takes effect on the next session (fish auto-loads `~/.config/fish/completions/`, and an absolute `XDG_CONFIG_HOME` is respected; for PowerShell, append the script output to `$PROFILE`).

On zsh, `compinit` may report insecure directories (group/other-writable directories on the completion search path — most commonly `/opt/homebrew/share/zsh` and its `site-functions` on old Homebrew installs) and ask whether to continue. Three ways to handle it:

1. Answer `y` at the prompt; it reappears in every new shell.
2. Repair the directories once and rerun: `chmod go-w <dir>` on each reported directory you own (the fix Homebrew itself recommends; a directory owned by another user needs an administrator instead — `chmod` cannot fix foreign ownership).
3. Skip the security check permanently: run `compinit -u`, or write `compinit -u` in your rc file instead of plain `compinit` (accept the skipped check consciously).

The official installer script performs the whole setup automatically — on zsh it asks interactively whether to repair the directories, skip the check, or skip completion; see the [Installation Guide](install.md).

For persistent installation instructions for each shell, run `token-usage completion <shell> --help`. This command reads no configuration, database, or data source.

## collect

Collects token-usage data. Before opening the database, `collect` and all of its subcommands run a **daemon-conflict precheck**: if the daemon is running and holds the daemon lock, collection is rejected to avoid concurrent database writes.

```text
token-usage collect [DATE|DATE-DATE]
token-usage collect all
token-usage collect router --client <name>
token-usage collect retry
```

| Form | Purpose | Inherited flags |
|------|------|----------|
| `collect [date]` | Incrementally collects all enabled clients for today or a specified date; reads router logs and backfills attribution during collection. | `--client`, `--force` |
| `collect all` | Two-phase full collection: phase A scans all historical `messages` client by client without consulting `collection_log` (failure of one client does not stop the others); phase B fully backfills attribution for clients with a router configured. Messages use idempotent `(client, id)` UPSERT, so it is safe to rerun. | `--client` |
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
token-usage query day [date]
token-usage query month [date]
token-usage query hour [date]
token-usage query weekday [date]
token-usage query heatmap [date]
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
- `Query range / 统计范围` echoes the resolved date range: a single day alone, or the normalized first-to-last span as `YYYY-MM-DD ~ YYYY-MM-DD` (a month or year arg shows its expanded range).
- `Data through / 数据截至` is the latest message-event timestamp (`messages.ts`) inside the queried range, displayed in local time to the second. Message event times are the temporal boundary of the statistics; the field shows `—` when the range contains no messages.
- `Last successful collection / 最近成功采集` is the most recent successful collection completion time in the whole database (`collection_log.collected_at`, stored in UTC and displayed in local time). It does not imply that every client was collected up to that moment, and it shows `—` before any successful collection exists.

The default date is today. If the queried date range has unresolved entries in `collection_errors`, the results end with a collection-error notice and list the affected entries; when several tables are output (a group), the notice appears once after all of them. Use `errors` for details and `collect retry` to retry them.

Every grouped view (the nine built-in views and every custom multi-dimensional table) ends with a `Total / 总计` row computed from the same date range as the table; session details and the summary do not have this row.

`query day`, `query month`, `query hour`, `query weekday`, and every view containing the `day`, `month`, `hour`, or `weekday` dimension present a time-ordered timeline: rows are ordered by the time dimension ascending (`YYYY-MM-DD` days, `YYYY-MM` months, `00:00`..`23:00` hourly ticks, or weekday names in ISO week order, not by total; when several time dimensions coexist, the first declared one is the sort axis), and a `Trend / 趋势` bar column compares each row's total against the busiest row in the range. The pure `day` and `month` views insert a zero-value row for each day or month without data within the requested range, whereas the pure `hour` view always presents the fixed 24 hourly ticks of the day and the pure `weekday` view always presents the fixed 7 weekday ticks in ISO week order (Monday first), each with zero-value rows for hours or weekdays without data, independent of the requested date range (hours and weekdays are folded from message timestamps to local time, the same timezone semantics as the date column), so the timeline has no gaps.

`query heatmap` renders a weekday-by-hour matrix: rows are the seven weekdays in ISO order (Monday first), columns are the 24 hours `00`..`23` (both folded from message timestamps to local time, the same timezone semantics as the date column). Each cell is a density character (` .:-=+*#%@`, 0..9) scaled against the busiest cell of the table, the trailing column totals each day, and the trailing `Total / 总计` row totals each hour plus the whole range. The matrix does not take part in the `[query.output.columns]` layout, and `heatmap` is a reserved view name — like `session` and `summary` it is not referable from `query.default`, subqueries, or groups.

`query summary` renders a fixed vertical summary: `Clients / 客户端数`, `Total requests / 请求总数`, `Active days / 活跃天数` (days with data inside the range), one line per token column (`Input` through `Total`, always including `Cache Create`), and, when the range contains at least one day with data, `Peak day / 单日峰值` (the date with the highest source total, ties broken by earliest date) and `Daily average / 日均总量` (range total divided by active days, integer division rounded down). Those last two lines are omitted when the range has no data.

### Configurable Query Views

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
- A subquery selects at least 2 distinct built-in dimensions (`client`/`model`/`provider`/`project`/`day`/`month`/`hour`/`weekday`); the declared order is the column order. A group selects at least 2 distinct items from built-in views plus defined subqueries; groups cannot reference groups.
- View names are lowercase identifiers (a letter first, then letters, digits, `_`, `-`) and must not collide with `client`/`model`/`provider`/`project`/`session`/`summary`/`day`/`month`/`hour`/`weekday`/`heatmap`/`custom`/`list`. Values are comma-separated; every segment is trimmed, so `"model, provider"` equals `"model,provider"`. If a handwritten subquery or group was named `list`, rename it before upgrading: newer binaries reject the name because `query list` became a static discovery command.
- `query.default` is matched after trimming; whitespace means "use client". It may reference a built-in view, a subquery, or a group; `session` and `summary` are not referable.
- `query list` takes no positional args and prints a fixed structure in one pass: default behavior (`token-usage query -> <name> (<category>)`), one-time invocation hint showing the direct and explicit forms as equivalent, eleven built-in commands with their purposes, then every configured subquery and group as a single copy-pasteable command for today (such as `token-usage query mpc`) together with its dimensions or members CSV; empty sections say `None`. It only reads the effective config and parses definitions — it never opens `usage.db`, prints statistics, reads collection errors, accepts a date, or changes any state. Bad definitions still fail there with the same localized errors instead of being hidden behind an empty section.

### Output Column Layout

The optional `[query.output]` section defines one global, ordered list of metric columns shared by every query table:

```toml
[query.output]
columns = ["requests", "input", "output", "total", "cache_hit"]
```

`columns` is an ordered string array: an ID appears → the column is shown, absent → hidden, and the array order is the column order of every table. Allowed IDs (case-sensitive):

| ID | Header | Meaning |
|---|---|---|
| `requests` | Requests / 请求数 | message count |
| `input` | Input / 输入 | fresh input tokens |
| `output` | Output / 输出 | output tokens |
| `cache_read` | Cache Read / 缓存读取 | cache read tokens |
| `cache_create` | Cache Create / 缓存创建 | cache create tokens |
| `reasoning` | Reasoning / 推理 | reasoning tokens |
| `total` | Total / 总计 | source total tokens |
| `cache_hit` | Cache Hit / 缓存命中 | cache_read / (fresh input + cache_read + cache_create) |

- **Scope**: the layout applies to `query client`, `model`, `provider`, `project`, `day`, `month`, `hour`, `weekday`, `session`, and every table of the bare query, named views (`query <name>` / `query custom <name>`), and groups, plus the matching `watch` frames (their view tables run the same execution chain as `query`; `watch --by summary` keeps the complete vertical summary). `query summary` is not covered — it keeps its complete vertical summary, including Cache Create; `query list` renders no data table. Dimension columns are always shown on the left of each table (the session table always shows Client/Project/Title/Duration first) and never take part in the layout.
- **Default**: when `[query.output]` or `columns` is missing, the seven columns `requests, input, output, cache_read, reasoning, total, cache_hit` are used, so existing configs and outputs stay identical after an upgrade. `cache_create` is the first metric that is selectable but hidden by default; it always counts toward the Cache Hit denominator, so hiding or showing it never changes any statistic, sort order, or total.
- **Validation**: `query.output` must be a table whose only key is `columns`; the array must be non-empty, its elements strings from the table above, without duplicates (whitespace around an element is trimmed). An empty array is not "restore defaults" — remove `query.output` (or `query.output.columns`) to restore the default layout. Errors are reported with the full config path and the offending value. `config set` cannot write `query.output.columns`; use the TUI Output columns page or edit the TOML by hand.
- **Error boundary**: unrelated view-definition errors (`subqueries`/`groups`/`default`) never block the nine layout-affected static table commands — a valid layout still applies. An invalid `query.output` itself fails those nine commands before the database is opened. A top-level query problem (`[query]` alongside `[Query]`, or a non-table root) silently falls back to the default seven columns for the static table commands, while the bare query, named views, and `query list` keep failing with the existing localized errors. TUI saves always run the full query validation.

`query provider` (and the provider dimension of any custom view) prefers router attribution, then the collector's provider value. Historical empty values remain unattributed; the query does not infer a provider from the client. `provider_aliases` is applied before composite keys are formed: aliases with the same value are combined into one row in every view, without changing `usage.db`.

Query configuration is display-only. Semantic errors (broken references, malformed CSV, unknown keys, top-level conflicts such as `[query]` alongside `[Query]`, or a non-table root like `query = "x"`) make the default path (bare `query` and `query <date>`), every named invocation (`query <name>` / `query custom <name>`), `query list`, and TUI saves fail with the offending key; the nine layout-affected static table commands (`client`/`model`/`provider`/`project`/`day`/`month`/`hour`/`weekday`/`session`) fall back to the default seven columns on a top-level problem and otherwise keep their layout when only unrelated view definitions are broken, `query summary` is unaffected, and `collect`, `daemon status`, `daemon start`, the daemon itself, `config set`, and `config show` keep working and preserve the offending entries. `watch` follows the same split: its default-view and configured-view paths fail with the same localized diagnostics as the bare `query`, while an explicit built-in view name (`watch --by client`) ignores unrelated view-definition errors like the static table commands. In the TUI, `v` on the main menu opens the **Query** page with three entries — **Views** (custom subqueries, groups, default behavior), **Output columns** (the global metric layout, with `d` to restore the default), and **Provider aliases** — each showing a recovery list when its own part of the raw section cannot be parsed. Before downgrading to a version without query-view support, remove the whole `[query]`, `[query.subqueries]`, `[query.groups]`, and `[query.output]` sections: older versions reject any non-empty query section.

Examples:

```bash
token-usage query                    # today, runs the configured default (client when unconfigured)
token-usage query 20260701-20260721  # date range, default view
token-usage query 202608             # single month, default view
token-usage query mpc                # today, the mpc multi-dimensional table (direct shorthand)
token-usage query custom group_q 20260701  # explicit spelling: four tables in declared order
token-usage query summary 20260701   # single-day overview
token-usage query list               # list configured views without touching the database
```

## errors

Displays collection errors.

```text
token-usage errors [DATE|DATE-DATE]
```

The date argument accepts the same forms as `collect`/`query`: a single `YYYYMMDD` day, a `YYYYMM` month, a `YYYY` year, or a `DATE-DATE` range (expanded day-by-day, at most 366 days like every other date range).

- With neither a date nor `--source`, only **unresolved** errors are shown by default.
- With a date (single day, month, year, or range) or `--source`, **all states** (including resolved) are shown by default.
- `--unresolved` explicitly requests unresolved errors only and always takes effect.

Flags:

- `--source <name>`: filters by data source (`claude`, `opencode`, `codex`, `workbuddy`, `zcode`, or `autoclaw`).
- `--unresolved`: shows unresolved errors only.
- `--format <fmt>`: output format, `table` (default, the framed table with the retry hint) or `json`. Invalid values are rejected before the database opens.

With `--format json`, stdout is pure data — a JSON array of error records with no statistics header, no "no error records" line, and no retry hint (errors still go to stderr with a nonzero exit). Each record projects exactly the fields visible in the `table` columns:

| Field | Type | Meaning |
|---|---|---|
| `id` | number | Record ID |
| `date` | string | Date (`YYYY-MM-DD`) |
| `source` | string | Data source |
| `message` | string | Error message |
| `retry_count` | number | Retry attempts so far |
| `resolved` | boolean | Whether the error has been resolved |

Filtering and record order (latest first, same as the `table`) are identical in both formats. An empty result prints `[]`. Output uses two-space indentation with a trailing newline.

Examples:

```bash
token-usage errors                     # unresolved errors
token-usage errors 20260721            # all errors for one date
token-usage errors 20260701-20260707   # all errors across a date range
token-usage errors --source codex      # all errors for one source
token-usage errors --unresolved        # explicitly unresolved only
token-usage errors --format json | jq .  # machine-readable records
```

## doctor

Runs read-only health checks and prints one line per check (`label: status description`, statuses `OK / 正常`, `WARN / 警告`, `FAIL / 失败`, plus `SKIPPED / 跳过` and one informational `INFO / 提示` line) followed by a summary. Strictly read-only: it **never starts, stops, or restarts the daemon and never modifies configuration; no business data is written** — opening the database (journal-mode setup and schema migration) behaves exactly as in every other read command, and doctor itself performs no writes of its own. The data-directory writability probe creates exactly one temporary file and removes it immediately.

```text
token-usage doctor
```

| Check | OK | WARN | FAIL |
|---|---|---|---|
| `Config / 配置` | effective config loads; prints the config path | — | config missing or invalid |
| `Data directory / 数据目录` | directory exists and is writable (probe file created and removed immediately) | — | directory missing, not a directory, or not writable |
| `Database / 数据库` | opens, `PRAGMA quick_check` passes; prints the path and message count | file not created yet (run `collect` to generate) | open failure or quick_check does not pass |
| `Clients / 客户端` | count and names of enabled clients | no client enabled | — |
| `Last collection / 最近采集` | last successful collection time (local timezone) | no successful collection recorded yet | query failure |
| `Data freshness / 数据新鲜度` | humanized time since the last collection (`just now`, `N h ago`, or `N d ago`); OK within 7 days | last collection more than 7 days ago (7 days covers weekends and short holidays); suggests running `token-usage collect` to refresh; warnings only | — |
| `Date consistency / 日期一致性` | every stored local date matches the local date recomputed from its millisecond timestamp; prints the total message count | N messages with date inconsistent with timestamp; check whether the system timezone changed or data was modified directly; warnings only — no auto-fix | query failure |
| `Unresolved errors / 未解决异常` | none | count with pointers to `token-usage errors` and `token-usage collect retry` | query failure |
| `Query definitions / 查询视图` | configured subqueries/groups/default are semantically valid (also OK when none are configured) | issue count with the first diagnostic path and a pointer to `token-usage query list`; warnings only — broken view definitions never block collection or the static table commands | config failed to load |
| `Daemon / 守护进程` | informational only: points to `token-usage daemon status`; doctor never probes or controls the daemon (probing would create lock/config-directory files) | | |

- Checks that cannot run because an upstream check failed print `SKIPPED / 跳过` and add no new count (the upstream FAIL already counts): config failure skips every config-dependent check; a missing or broken database skips the collection, freshness, date-consistency, and error checks; when the last-collection query fails, data freshness is skipped as `unavailable / 无法获取` because last collection already FAILs; with no collection recorded at all, data freshness is skipped because last collection already warns.
- The summary line (`Result / 结果`) is `OK / 一切正常`, `N warnings / N 项警告`, or `N problems / N 项失败` (FAIL takes precedence over WARN).
- The exit code is always 0 in v1: FAIL/WARN are report-only.

Example:

```bash
token-usage doctor
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

> `config get` and `config show` have distinct roles. The former reads a raw user-configuration value without expanding `~` or filling defaults; the latter outputs complete effective TOML after expanding `~` and filling defaults/default paths. Prefer `config show` to inspect runtime-effective configuration; `daemon status` and the TUI are human-readable summaries only.

### config (TUI)

With no arguments, opens the interactive configuration TUI (`bubbletea`). If no configuration file exists, it first writes the default template, then opens the UI. You can edit clients, routers, daemon settings, logs, and query settings (the `v` Query page groups view definitions, the output column layout, and provider aliases); `data_dir` is read-only in the TUI. Saving always goes through `ApplyConfig` (see [config set](#config-set)). Clients outside the router-capable family (currently every client except Claude) show no router field; an existing non-empty router value on such a client is still displayed so it can be cleared, and saving rejects a non-empty value (see the router guard under [config set](#config-set)).

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

It reads the **raw user-configuration value**: the value explicitly written in the configuration file, without expanding `~`, filling default paths, or clamping numeric values. Therefore, fields that are not explicitly written return their zero value (for example, an absent `poll_interval` returns `0`). Use `config show` to inspect the full effective runtime configuration with expanded paths and defaults; `daemon status` and the TUI are human-readable summaries only.

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

**Router guard:** `config set clients.<name>.router <value>` fails before writing when `<value>` is non-empty and `<name>` is not a router-capable client (currently Claude and Codex); the command exits nonzero. Setting an empty value clears the router and is always allowed. Read paths (`config show`, collection, the daemon) keep tolerating a non-empty router on other clients in existing configurations.

**`data_dir` migration:** changing `data_dir` requires `--confirm-migrate`, and the old daemon **must be stopped** (the command rejects a running daemon before writing). Move `usage.db` and `logs` manually; PID/lock/runtime-state are not migrated and are cleaned by the stale protocol.

### Supported Dotted Keys

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

### Autostart Semantic Boundary (Important)

`config set daemon.autostart <bool>` (or the TUI toggle) only **synchronizes the autostart service definition** (a macOS plist or Windows Registry Run key). It **never starts or stops the current daemon**:

- Enabling autostart writes the definition and leaves the current daemon unchanged; the new definition loads at the next login/boot.
- Disabling autostart deletes the definition and leaves the current daemon running; it no longer starts at the next login/boot.

To apply it in the current session, manually run `daemon stop` then `daemon start` (or `daemon restart`). See [daemon](#daemon) for the full explanation of this decoupling.

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

## daemon

The `daemon` command group manages the **collection/analysis daemon** — the background process that watches AI client session logs and keeps usage data current. Its four actions manage only the **currently running daemon** and are fully decoupled from the **autostart definition** (whether it starts automatically at the next login/boot). The dashboard service is a separate program instance managed by the [serve](#serve) group; the two share no PID, lock, state file, log, or port, and stopping or restarting one never affects the other. Bare `token-usage daemon` only prints the command-group help: it starts nothing and creates no state files.

| Command | Purpose | Touches the autostart definition? |
|------|------|--------------------------|
| `daemon start` | Starts the daemon in the background and returns after the monitor-ready handshake; if already running, idempotently returns the current PID. | No |
| `daemon stop` | Stops the current daemon without deleting the plist/Registry definition; idempotent when not running. | No |
| `daemon restart` | Stops the old daemon and starts a new one under one process-control lock; fails and suggests `daemon start` when none is running. | No |
| `daemon status` | Read-only runtime inspection plus five-state autostart drift detection. | No (read-only) |

> None of these commands modifies configuration, a plist, or the Registry. The autostart definition converges through `config set daemon.autostart` or a TUI save.

### daemon start

```text
token-usage daemon start
```

Through `control.Manager.Start`: load configuration under the process-control lock → determine liveness from the daemon lock → if already running, return the current PID without spawning again (exit code 0) → otherwise detached-spawn `_run` → wait up to five seconds for six readiness conditions (PID/instanceID in the PID file, the daemon lock, PID/instanceID/`monitor_ready=true` in runtime-state) → print a success line containing the PID. On timeout, it tries to terminate only the new child and cleans metadata only when the daemon lock is released and the metadata still belongs to this generation, avoiding deletion of a live process or another generation's files.

stdout contains success lines, including the idempotent already-running result; stderr contains actual failures.

### daemon stop

```text
token-usage daemon stop
```

Through `control.Manager.Stop`: load configuration under the process-control lock → determine liveness from the daemon lock → if not running, return the idempotent not-running result → if running, stop by platform (macOS always first idempotently tries `bootout` for the current label; if the daemon lock remains held, sends SIGTERM to the exact read PID; Windows uses `taskkill` on the exact PID) → define success as **daemon lock released** (polling for five seconds), never by deleting a PID file to simulate success.

`daemon stop` **does not delete** the plist/Registry definition: the current session stops, while the next login follows the autostart configuration. Disable autostart with `config set daemon.autostart false`.

### daemon restart

```text
token-usage daemon restart
```

Through `control.Manager.Restart`, it stops the old daemon and starts a new one under one process-control lock. If the daemon is **not running**, it returns `ErrRestartNotRunning`, writes a suggestion to use `token-usage daemon start` to stderr, and exits nonzero.

macOS tradeoff: the stop phase attempts to `bootout` the current job and then the start phase runs the daemon detached; the plist definition remains, but launchd KeepAlive no longer manages it for the current login session. Because saving configuration only maintains the definition file and does not proactively bootstrap it, KeepAlive resumes when the definition is loaded at the next login.

### daemon status

```text
token-usage daemon status
```

Read-only: `Inspect` does not acquire the process-control lock and determines liveness only from the daemon lock. It returns a consistent snapshot containing:

- Runtime state: running with a PID, or not running.
- Startup phase (an extra line when running): monitor initialization / monitoring ready and catch-up in progress / partial catch-up failure with a count and a suggestion to run `token-usage errors`. A successful catch-up adds no extra line; unavailable PID metadata or phase mismatch degrades to an unknown startup phase.
- Data directory and polling interval.
- Five-state autostart drift detection: enabled / autostart on but definition missing / content differs / autostart off but definition remains / not enabled. Drift only suggests saving configuration again; it triggers no writes.

Autostart expresses only whether the daemon starts at the next login/reboot and is independent from whether the current daemon is running. The current runtime state is displayed separately; neither is inferred from the other.

### Startup Catch-Up (Closes the daemon stop → collect → daemon start Data Window)

After monitoring is established by `daemon start`, the daemon performs **startup catch-up** to collect data created between the last manual `collect`/`collect all` and monitor readiness, closing the daemon stop → collect → daemon start data window.

Ordering contract (`daemon.startupCoordinator`):

1. Wait for every analyzer monitor to be ready (ready barrier); if the context is canceled, write no state and perform no catch-up.
2. Write ready state (`monitor_ready=true, catch_up=pending`).
3. Write running state (`catch_up=running`); if the write fails, log it and continue without stopping the daemon.
4. Submit catch-up requests in order: enabled client names ascend; each client first gets a client-source request (opencode/zcode use incremental cursors; claude/workbuddy/autoclaw scan existing JSONL without a date; Codex does state incremental collection first and then a full rollout scan), then receives its router incremental request if configured.
5. Write final state: zero failures means `succeeded`; otherwise `failed` with the exact failure count.

Catch-up is submitted through the analyzer serialization lock (the same path as real-time triggers, guaranteeing ordering and mutual exclusion). Therefore, if the daemon starts successfully and completes catch-up, incremental data generated between daemon stop → collect → daemon start is collected and is not missed because monitoring was not ready. Partial catch-up failures appear in `daemon status` and `errors`.

### _run (Hidden)

An internal command started by `daemon start` through detached spawn or directly by launchd / a Windows Registry Run key. It executes the daemon main loop and must not be invoked by users (it is absent from `--help`). Both startup paths satisfy the invariant that “a control lease exists continuously from reading effective configuration through acquiring the daemon lock”:

- Parent-lease path (`_run` spawned by `daemon start`): the parent holds the process-control lock and authorizes the child through a pipe lease; the child does not acquire the lock.
- Independent path (started directly by launchd/the Registry): without a valid parent lease, it acquires the process-control lock itself (15-second timeout). On timeout it exits successfully with code 0 rather than entering the main loop, avoiding conflict with an in-progress control operation and preventing launchd KeepAlive from immediately relaunching it on macOS.

## watch

Renders the `query` output for a window and refreshes it at a fixed interval until interrupted with Ctrl+C. The frame body is exactly the query output — statistics header, view tables with the configured `[query.output.columns]` layout and `provider_aliases`, and collection-error warnings — selected with the same rules as `query`: with no `--by`, the default view runs (`query.default`, built-in fallback `client`); watch adds only the `Live watch` banner, the fixed-interval refresh, and the screen clearing between frames.

```bash
token-usage watch                      # today, default view, refreshed every 5s
token-usage watch 20260901 --interval 10s
token-usage watch --by group           # a configured view name from query.groups
token-usage watch --once               # render a single frame and exit (pipe-friendly)
```

- `--by` selects the frame view: a built-in view (`client`, `model`, `provider`, `project`, `day`, `month`, `hour`, `weekday`, `heatmap`, `session`, `summary`) or a configured view name from `query.subqueries` / `query.groups`. With no `--by` the default view runs — `query.default` with the built-in fallback `client` (a configured group renders all of its member tables per frame). Explicit built-in names ignore unrelated view-definition errors exactly like the static `query` subcommands; the no-flag and configured-name paths run the full query validation and fail with the same localized diagnostics as the bare `query`. An unknown `--by` value is rejected with the dynamic allowed set before the database opens.
- The date argument accepts the same forms as `query`; with no date the frame tracks today, recomputed on every refresh so it rolls over midnight automatically, while an explicit date or range stays fixed (watching a historical window is a valid use).
- `--interval` accepts a Go duration with a minimum of 1s; shorter values are rejected before the database opens.
- Interactive loops clear the screen between frames (Windows consoles get virtual-terminal processing enabled automatically); `--once` renders exactly one frame with no escape sequences, so redirected output stays plain.
- Strictly read-only: the same opening semantics as every other read command, no daemon interaction, and Ctrl+C leaves no state behind.

## serve

Manages the read-only local HTTP server that serves the built-in dashboard: the embedded HTML page at `/`, JSON endpoints (`/api/meta`, `/api/dashboard`), and SVG charts (`/api/chart/{kind}.svg`) rendered by the same chart core as the embedded page (identical titles, subtitles, and hover text). The dashboard **always runs in the background**: `serve start` spawns a detached server process and returns, `serve status` / `serve stop` inspect and stop it, and `serve restart` replaces a running instance with a fresh background one. Bare `token-usage serve` only prints the command-group help — it listens on no port, starts no process, and creates no state files. The collection daemon is a separate program instance managed by the [daemon](#daemon) group; the two share no PID, lock, state file, log, or port.

The HTTP data surface is strictly read-only — no CORS headers and no database or configuration writes; `serve.json` is the shared lifecycle state, `serve.log` is used for background runs, and `serve.lock`, `serve-state.lock`, and `serve-start.lock` coordinate the lifecycle.

- `--addr` changes the listen address (default `127.0.0.1:8619`); it applies to the freshly started instance of `serve start` / `serve restart`. Binding a public address such as `0.0.0.0` **exposes your usage data to the local network** — the service is read-only but unauthenticated — so keep it on the loopback interface.
- `--open` opens the dashboard in the default browser after `serve start` / `serve restart` have confirmed the background server is up; a failure to launch the browser is printed as a warning and the server keeps running.
- If writing the `serve.json` state file fails at startup (e.g. a read-only data directory), the server exits with an error instead of serving untracked.
- All data queries for one request share a single read snapshot, so totals, per-dimension rows, and session rows are mutually consistent under concurrent collection writes.
- The embedded page renders everything client-side from these numeric rows: KPI cards with delta chips against the baseline window, a metric strip aligned with the configured query output columns (the token-kind columns except requests/total/cache-hit, which are elevated to KPI cards — with the default layout that means input / output / cache read / reasoning, and a cache-create column appears only when the layout includes it), stacked/single-series bar charts (click a day or month bar to focus that range; beyond 92 days the day bars roll up to ISO weeks with drill-down disabled), donut share charts four per row, the weekday-by-hour heat matrix with row and column totals (mirroring the `query heatmap` trailing totals), top sessions with token bars and CSV export, per-dimension data tables with sorting and one-click CSV export (current row order, exact integers), a full-width compare row (daily-totals chart on the left, the metric table on the right) and a full-width forecast row, range presets plus custom `from`/`to` (defaulting to Today on first visit, remembered across reloads), and auto-refresh. A range with fewer than two day buckets hides the per-day chart, fewer than two month buckets hides the per-month chart, a single-day range shows only the hour chart (the weekday view is meaningless for one day) and hides the comparison table — single-bucket forms carry no information — while the KPI delta chips still compare against the baseline window (the previous day for a single day).

| Endpoint | Parameters | Returns |
|------|------|---------|
| `GET /api/meta` | — | version, `min_date`/`max_date` (whole database), `data_through`, `last_collection`; the last three are `null` when absent |
| `GET /api/dashboard` | `from`, `to` (`YYYY-MM-DD`; defaults to the 30 days ending today; span at most 366 days) | range, totals (integers, including `active_days`), compare (baseline window derived from the requested range — an equal-length window ending the day before the range starts, or the previous day for a single-day range; carries base totals plus 8 pre-computed rows with display strings, signed changes, pos/neg change classes, and change % shown as `--` when the baseline is 0; and `daily` — the base window's day rows (gap-filled to the window length, keys are dates, plain integers) feeding the client-side current-vs-base daily chart), forecast (fixed look-back windows: `today_so_far` plus always 2 rows for the last 7/30 days — windows exclude today, averages divide by active days, estimates multiply the average by future days; display strings pre-computed, cells show `—` when a window has no data; independent of the `from`/`to` range), 8 fixed dimension row arrays (`day`/`hour`/`weekday`/`month`/`client`/`model`/`provider`/`project`), top 10 sessions, and `heatmap` — a 7×24 token matrix (`weekdays` in ISO order with Monday first, `hours` as `00:00`..`23:00`, `values` as a 7×24 array with `0` for empty cells) read inside the same snapshot as the totals, so a single refresh cannot mix snapshots; the embedded page renders all charts client-side from these numeric rows, while `GET /api/chart/{kind}.svg` remains available for standalone retrieval |
| `GET /api/chart/{kind}.svg` | same date parameters as `/api/dashboard`; `kind` ∈ `day`/`hour`/`weekday`/`month` (bars), `client`/`model`/`provider`/`project` (pies), `heatmap` | one SVG document (`image/svg+xml`) |
| `GET /`, `GET /assets/…` | — | embedded HTML page and static assets (`Cache-Control: no-store`) |

- Errors are uniform JSON `{"error":{"message":"…"}}`: `400` for invalid parameters, `404` for unknown chart kinds or assets, `500` for query failures.
- Strictly read-only data surface: no CORS headers (same-origin use), no daemon interaction, and no database or configuration writes. The persistent state is `serve.json`, background output goes to `serve.log`, and `serve.lock`, `serve-state.lock`, and `serve-start.lock` coordinate lifecycle transitions. Dimension rows are plain integers — K/M/B formatting is left to the frontend; `provider` rows apply `[provider_aliases]` exactly like the query views.

### serve start / serve status / serve stop / serve restart (background, nginx-style)

`serve start` spawns a detached child process and returns once the child reports ready, `serve status` inspects it, and `serve stop` stops it. These are the only ways to run the dashboard.

```bash
token-usage serve start
token-usage serve start --addr 127.0.0.1:9000
token-usage serve start --open
token-usage serve status
token-usage serve stop
token-usage serve restart
```

- State file: `serve.json` in the data directory (`~/.token-usage/serve.json` by default), written atomically as `{"pid":…,"addr":…,"started_at":…}` as soon as the server finishes listening (the recorded `addr` is what is actually bound). It is removed automatically on graceful stop (`serve stop`, or restart's stop phase); a file left behind by a crash or `SIGKILL` is cleaned up by the stale-state probes in `serve status`/`serve stop` and by the single-instance guard of the next `serve start`. State transitions are serialized by a `serve-state.lock` file lock in the data directory, and stale cleanup is a conditional delete: a stale state is only removed while it still matches what was judged — if a new instance has already taken over, its fresh `serve.json` is never removed.
- Log file: `serve.log` in the data directory (`~/.token-usage/serve.log` by default). Each `serve start` truncates it; the child's stdout and stderr both go there, in plain text (no terminal hyperlinks). A failed start attaches the last 10 log lines to the error message.
- `serve start` reports and returns idempotently with exit code 0 when the recorded state still answers on `/api/meta` (already running — stop it first with `token-usage serve stop`, or use `token-usage serve restart`); state that no longer answers (stale) or is corrupt is removed and start proceeds. If the child does not become ready within 5s, start fails and points at the log tail. Concurrent `serve start` invocations are serialized by a `serve-start.lock` file lock in the data directory (start coordination only — a running instance is described by `serve.json` and held via the `serve.lock` lifecycle lock); a second start while another is still in progress exits non-zero with a retry hint.
- `serve status` exits 0 for every state outcome (only unexpected I/O failures exit non-zero): when `/api/meta` answers it reports the URL, PID, and start time; otherwise (no answer, or a corrupt state file) it removes the stale or corrupt file and reports not running. State transitions are serialized by `serve-state.lock`; when the lock stays held past the bounded retry (a concurrent `serve status`/`serve stop` probing), the command exits non-zero with a busy hint — simply retry.
- `serve stop` sends SIGTERM with a 3s graceful window and a SIGKILL fallback on Unix; on Windows it uses `taskkill /F` — Windows console processes have no cross-process graceful-stop channel, which is acceptable for a strictly read-only service. Success is judged solely by `/api/meta` no longer responding (the recorded PID may have been reused by an unrelated process, so the probe wins over the signal result — a failed signal delivery does not short-circuit the probe wait). The state file is removed only after the probe confirms shutdown (or when the server already fails to answer before signalling — stale or corrupt state is cleaned up). If the server still responds after the SIGKILL fallback (whether or not the kill itself reported an error), the command exits non-zero with the recorded URL and PID, keeps `serve.json` in place, and leaves the process/port for manual inspection. If a new instance takes over while the stop is in progress (the state file is replaced after the old instance went down), the command says so and routes the stop to the new instance instead of reporting the old one. Stopping an already-stopped server is an idempotent no-op that still exits 0.
- `serve restart` stops the running instance with exactly the `serve stop` orchestration (probe-verified) and then starts a fresh background instance with exactly the `serve start` orchestration (`--addr`/`--open` apply to the new instance). With no instance running it simply starts one. If the running instance still answers after the SIGKILL fallback, restart aborts with a non-zero error — the old instance keeps serving; inspect that case with `serve stop`.
- Single-instance contract: at most one dashboard instance runs at any time. A second `serve start` — regardless of the address it asks for — is rejected by a single-instance guard before listening: it prints the running instance's URL and PID and exits 0 idempotently (stop it first with `token-usage serve stop`, or use `token-usage serve restart`); if another instance is caught mid-startup, the guard fails with a retry hint instead. The serving process holds a `serve.lock` lifecycle lock in the data directory for its whole lifetime. Because the guard runs before listening, a same-port conflict with a running instance never surfaces as a listen failure — a listen failure only remains possible when the requested port is occupied by a process that left no `serve.json` record (an unrelated process). `serve status` / `serve stop` therefore always manage the one and only instance.
- `--open` is honored by `serve start` and `serve restart`: the browser opens only after the background server is confirmed up (a failure to launch the browser is a warning).

## update

Updates the `token-usage` binary in place from official GitHub Releases. The CLI only parses flags, assembles dependencies, and formats results; the self-update core lives in `internal/update` (see [Architecture](architecture.md)).

`update` prints its progress while it works: a `Checking for updates…` line, the current/target version pair as soon as a newer release is found (before source verification, so refusals also show it), then step lines for downloading, verifying, stopping the daemon, installing, and restarting the daemon. On an interactive terminal the download renders a single-line live indicator (percentage, transferred/total bytes, average speed); when stdout is redirected or piped the indicator is omitted and only the step lines are printed, and a failed download always closes the line cleanly. A daemon that was running before the update is stopped and restarted automatically on the new binary. `update --check` prints only the checking line and its result.

```text
token-usage update
token-usage update --check
token-usage update --version <tag>
token-usage update --force
```

| Form | Purpose |
|------|---------|
| `update` | Updates to the latest stable Release. If a restricted transaction journal from an interrupted POSIX update exists beside this binary, it is recovered first; a new replacement then proceeds only when the target is strictly higher than the current version and the current source is trusted. It downloads the asset, verifies its SHA256 against the `SHA256SUMS` manifest, stages a `--version` second check, and replaces the binary. A daemon that was running before the update is restarted automatically on the new binary; a daemon that was stopped stays stopped, and the success output points to `token-usage daemon start`. |
| `update --check` | Read-only check; creates no local files (no configuration directory, lock, log, database, or service definition). |
| `update --version vX.Y.Z` / `update --version vX.Y.Z-rc.N` | Updates (or, with `--check`, only checks) the specified exact Release tag. `--version` accepts a strict Release tag (`v` prefix, `MAJOR.MINOR.PATCH`, optional `-rc.N`, no leading zeros); an invalid value errors before any network request. |
| `update --force` | Overwrites the current binary even when its source is not an official Release asset, for exactly two exemptions: a hash mismatch against the official asset of the reported version (a binary re-signed per the install guide, or `go install pkg@vX.Y.Z`), and a dev local build (`Version = dev`, or the normalized `vX.Y.Z-dev` display of a plain-build pseudo-version — both dev forms `update --force` accepts). All structural checks and the target asset's SHA256 / staged `--version` verification still run; symlinked copies and non-official tags cannot be forced. |

`--check` and `--version` may be combined; for example, `update --check --version vX.Y.Z-rc.N` checks a release candidate only. `--force` cannot be combined with `--check` (that combination is rejected explicitly).

Flags:

- `--check` (bool): read-only check; writes no local files.
- `--version` (string): target Release tag. Accepts `vMAJOR.MINOR.PATCH` and `vMAJOR.MINOR.PATCH-rc.N` (no leading zeros, `N >= 1`, no build metadata).
- `--force` (bool): overwrite even if the current binary is not an official Release asset (re-signed, `go install`, or a dev build); see [trust and source verification](#trust-and-source-verification) for the exact exemption boundary.

`update` takes no positional arguments (`Args: NoArgs`).

### Stable / Release-Candidate Selection

By default `update` resolves only the latest **stable** Release and never selects a prerelease. A release candidate is consulted or installed only when you pass its tag explicitly with `--version` (for example `--version vX.Y.Z-rc.N`).

When the local version is strictly newer than the requested or latest release — an RC ahead of the stable channel, or an explicit `--version` downgrade — both `update` and `update --check` report the local/target version pair and make no changes.

### Completion Migration Notice

When a successful `update` crosses the version that introduced automatic shell-completion setup — the current version is below it (an unparseable local-build version — `dev` or a `vX.Y.Z-dev` display — counts as below) and the target is that version or any later one — the success output appends a one-time migration notice with the official installer command for your platform. Re-running the installer sets up completion automatically (on zsh it asks interactively). The Windows background-replacement (deferred) outcome asks you to confirm the final version with `token-usage version` before re-running the installer, so the two never race on the binary. The notice is stateless: it is printed on every threshold crossing, so an explicit `--version` downgrade followed by a later upgrade prints it again. `update --check`, every failure and refusal branch, and the interrupted-transaction recovery outcome never print it.

### Trust and Source Verification

`update` only replaces the current binary when the target is strictly higher and the current source is trusted. The current source is treated as **untrusted** (and a plain `update` refuses to overwrite, printing manual-install guidance instead) when any of the following holds:

- the current `Version` is `dev` or a pseudo-version (e.g. from `make build`, `make build-all`, or `go install`);
- the current binary is not a regular file, or is a symlink;
- the current binary's SHA256 does not match the official asset hash for the current version (e.g. a binary re-signed per the install guide, or `go install pkg@vX.Y.Z`).

The refusal carries a `--force` escape hatch for exactly two exemptions:

- **hash mismatch** (the current version has an official Release and manifest, but the local content differs): re-running with `--force` overwrites the binary with the official asset, so automatic updates resume;
- **dev build** (`Version = dev`, or the normalized `vX.Y.Z-dev` display of a plain-build pseudo-version — both dev forms `update --force` accepts; no comparable official Release or manifest exists, so no hash comparison ever happened): `update --force` switches the installation to the official Release asset.

Symlinked copies and non-official tags cannot be forced — every other refusal reason always requires manual installation. `--force` never skips any check: structural checks still gate the replacement, and the target asset is still downloaded, SHA256-verified against `SHA256SUMS`, and stage-checked with `--version` before it may replace the current binary. A `--force` run is reported as successful with a `--force` note and exits 0; it is never reported as trusted.

On macOS the refusal message distinguishes a locally ad-hoc signed binary (detected via a signature probe) and names the re-signed-official-asset possibility explicitly; on other platforms and whenever the probe is unavailable, the generic message still lists re-signing among the possible causes and mentions the same `--force` exit.

The sole trusted repository is `YuLaiZ/token-usage`; see [Architecture](architecture.md) for the download-URL reconstruction, manifest, and staged-install trust chain.

This source gate applies to a new replacement. Recovering an already recorded local transaction does not download or accept a new source: it uses only same-directory paths derived from the executable and journal nonce, and rechecks the recorded hashes before restoring a consistent state.

### Exit Codes

- `0` for expected completed states: no stable Release available, already up to date, an update is available (`--check`), a Windows background replacement has been queued, or recovery confirms that the interrupted update had already installed the new binary.
- Non-zero when the requested tag does not exist, the current source is unverified and `--force` was not given (hash mismatch or dev build) or cannot be forced at all (symlink / non-official tag), download/manifest/checksum/staged-`--version` validation is rejected, recovery returns the binary to the old version, installation is incomplete, install/rollback/daemon-restart fails, or `--version` is invalid.

### Side-Effect Boundary

`update --check` is fully read-only. A real `update` first resolves an existing transaction journal when present; otherwise it stops the daemon, replaces the binary, and restarts it only when an update is available and the source check passes — trusted, or overridden with `--force`. It does not start or stop the daemon when no update is needed, and it does not rewrite `config.toml`, the database, logs, the macOS LaunchAgent plist, or the Windows Registry.

### Windows Asynchronous Replacement

Replacing a running `.exe` is restricted on Windows, so the update hands the replacement off to a background helper and returns. Once that helper has been started, the command explicitly reports that the background replacement has been queued, exits `0`, and asks you to run `token-usage version` or `token-usage update --check` shortly to confirm the final version; it never claims completion. If the daemon was stopped before the update, the output tells you to start it only after the replacement is confirmed complete — starting it earlier would make the helper abort the replacement (it refuses to touch the binary while the daemon runs). On macOS/POSIX the replacement is synchronous and atomic (same-directory backup + rename + fsync, with rollback on failure and journal recovery on the next `update` invocation).

## Configuration File

The fixed path is `~/.token-usage/config.toml` (TOML; comments may be added manually). All clients are disabled by default: enable the ones you use with `clients.<name>.enabled = true`, and the program fills data-source paths from each tool's default locations. Override defaults with a dotted key in the same section style. `config set` and TUI saves fully rewrite configuration, so existing comments and map-key ordering are not retained; see the template generated by `token-usage config init` for the complete fields and defaults.

`data_dir` determines locations for data files (`usage.db`, logs, PID, runtime-state, and locks); the configuration-file path does not change with `data_dir`. `daemon.autostart` controls autostart (macOS launchd / Windows Registry).
