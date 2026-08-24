# token-usage

> [简体中文](README.zh-CN.md) | English

A local LLM usage analytics tool for collecting, analyzing, and querying token usage across AI clients.

## Features

- **Message/API-request-level accounting**: each actual model invocation is stored independently instead of aggregating tokens by session.
- **Accurate attribution across dates and models**: messages that span dates, sessions that use multiple models, and forks/branches are neither missed nor double-counted.
- **Preserves actual rewind consumption**: completed calls before a rewind and new calls after it are kept separately rather than deduplicated.
- **Codex rollout replay deduplication**: replayed complete token snapshots caused by rate-limit bucket changes are not counted twice, while legitimate resets and multi-turn calls are retained.
- **Multiple data sources**: Claude Code/Desktop, OpenCode, Codex, WorkBuddy, ZCode, and Zhipu-AutoClaw.
- **CC Switch router attribution**: backfills the actual provider/model from CC-Switch proxy logs (currently effective only for the Claude family).
- **Two run modes**: one-off CLI commands and a real-time monitoring daemon with nginx-style background startup.
- **Autostart**: macOS launchd and the Windows Registry, configurable through the config TUI or `config set`.
- **Opt-in configuration**: every client is disabled by default; enable only the ones you use, while data-source paths fall back to ready-to-use defaults.
- **Single-binary distribution**: built with Go and pure-Go SQLite (no CGO), with macOS and Windows support.

## Quick Start

### Installation

The recommended method is the official Release binary installed by the official script: no Go toolchain, no sudo or administrator privileges — and the only installation source that supports in-place self-update. Everything is kept under `~/.token-usage` (configuration, database, logs, and the binary at `~/.token-usage/bin`), exposed through your user PATH.

#### Option A: Official Release binary (recommended — enables self-update)

**Paste to an AI agent** (it runs the installer script and verifies):

```text
Install the token-usage CLI on this machine by running the official installer
script for this platform:

- macOS: curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
- Windows PowerShell (two commands):
  irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"
  powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"

The script downloads the latest stable official Release, verifies its SHA256 against
the release's SHA256SUMS, installs the binary to ~/.token-usage/bin, and
configures the user PATH. Then open a new terminal and confirm with
`token-usage version` (run `token-usage --help` to see the available commands).
```

Or run it manually:

macOS:

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
```

Windows — two commands: download the installer, then execute it (paste both lines into one PowerShell window):

```powershell
irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"
powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"
```

The script detects the platform, downloads the latest stable official Release, verifies the SHA256 against the official `SHA256SUMS`, installs to `~/.token-usage/bin` (Windows: `%USERPROFILE%\.token-usage\bin`) without sudo or administrator privileges, configures the user PATH, and handles leftovers from the old installation layout (removed automatically on macOS; detected and reported with removal guidance on Windows). Open a new terminal and run `token-usage version` to confirm. To pin a specific release tag (including an RC), and for PATH semantics, old-TLS environments, and non-login-shell notes, see the [Installation Guide](docs/install.md).

An official-Release binary can update itself in place:

```bash
token-usage update                  # update to the latest stable release
token-usage update --check          # only check; writes no local files
token-usage update --version vX.Y.Z # update (or check) a specific release tag
```

See the [CLI Reference](docs/cli.md) for the full set of flags, exit codes, and side-effect boundaries.

#### Other installation methods

- [Manual binary installation (same layout)](docs/install.md#manual-binary-installation-same-layout): download and SHA256-verify an official asset by hand and configure the PATH yourself — equivalent to Option A, including self-update.
- [`go install` / building from source](docs/install.md): requires a Go toolchain; such binaries report `Version = dev` or a pseudo-version and cannot self-update (see [trust and source verification](docs/cli.md#trust-and-source-verification)).

To uninstall or migrate from an earlier layout, see [Uninstall and migration](docs/install.md#uninstall-and-migration).

### First use

```bash
# 1. Initialize the configuration file (with defaults at ~/.token-usage/config.toml) and database
token-usage config init
#    Or open the interactive configuration TUI directly (it initializes first if needed)
token-usage config

# 2. Enable the clients you use (all clients are disabled by default), e.g.
token-usage config set clients.claude.enabled true

# 3. Collect all historical data (do this once on first use)
#    Option A (recommended): automatic full collection; no date range is needed and router backfill is included
token-usage collect all
#    Option B: specify a date range manually
token-usage collect 20260101-20260721

# If daemon.autostart is already enabled, the daemon may be running in the background.
# Stop it before the first full collection (collect detects the daemon conflict and rejects concurrent database writes):
#   token-usage stop && token-usage collect all && token-usage start

# To backfill router attribution for one client separately, run:
#   token-usage collect router --client claude
# Note: collect all already includes router backfill, so collect router is normally unnecessary.

# 4. Keep today's data up to date in one of two ways:
#    Option A: start the daemon to monitor data-source changes automatically (recommended)
token-usage start
#    Option B: collect today's data manually
token-usage collect
```

> **About the first historical collection**: `collect all` scans all historical data for every enabled client (it does not skip data based on `collection_log`, and upserts by message primary key) and fully backfills attribution for clients with a router configured.
> `collect <date-range>` normally deduplicates with `collection_log` and only fills missing dates; add `--force` to recollect and overwrite.

## Command Reference

Running `token-usage` without arguments only prints help. See the **[CLI Reference](docs/cli.md)** for complete arguments, flags, exit codes, and examples.

### Collection and queries

| Command | Purpose |
|------|------|
| `collect [date]` | Incremental collection for today or specified dates, including router processing; use `--client X` to limit the client and `--force` to recollect. |
| `collect all` | Two-phase full collection: all historical `messages`, then full router backfill; `--client X` limits it to one client. |
| `collect router --client X` | Full router backfill only; does not touch `messages`; `--client` is required and the client must have a router configured. |
| `collect retry` | Retries unresolved groups in `collection_errors`; `--client X` limits the client. |
| `query [date]` | Queries usage statistics; defaults to today and groups by client. |
| `query client/model/project/sessions/summary [date]` | Queries the selected view. |
| `errors [YYYYMMDD]` | Displays collection errors; supports `--source X` and `--unresolved`. |

Dates are positional arguments: a single day is `YYYYMMDD`, and an inclusive range is `YYYYMMDD-YYYYMMDD`; there is no `--date` flag.

### Configuration

| Command | Purpose |
|------|------|
| `config` | Opens the interactive configuration TUI, including the autostart toggle. |
| `config init` | Initializes the configuration file and database; prints that all clients start disabled, with an example command to enable one. |
| `config get <key>` | Reads one user-configuration value by dotted key; it does not expand `~` or fill defaults. |
| `config show` | Outputs complete effective TOML: expands `~`, fills default values/paths, is read-only, and emits pure TOML. |
| `config set <key> <value>` | Writes one configuration value atomically, synchronizes autostart, and prints follow-up actions. |

> `config set daemon.autostart` only synchronizes the autostart definition; it **does not** start or stop the current daemon. To apply it in the current session, run `stop` then `start` (or `restart`) manually.
>
> `config get` returns the raw user-configuration value (without `~` expansion or defaults; fields not explicitly written return their zero value). Use `config show` to inspect the complete effective runtime configuration, including expanded paths and defaults.

### Version

| Command | Purpose |
|------|------|
| `--version` (or `-v`) | One-line short output: `token-usage <version>`; local development shows `token-usage dev`. |
| `version` | Multi-line detailed output: version, commit, build time, Go version, and platform. |

> `version` and `--version` are purely static commands: they do not read configuration, open the database, initialize logging, or access the network. `internal/buildinfo` normalizes their version and build metadata, which `make build`, `make build-all`, and `make install` inject through `-ldflags`.

### Self-update

| Command | Purpose |
|------|------|
| `update` | Updates the current binary to the latest stable release when it is an official Release asset and its source is trusted; `--check` only checks, `--version vX.Y.Z[-rc.N]` targets a specific tag. |

> Only a binary installed from an official Release can self-update; `make build`/`go install`/symlinked copies fall back to manual-install guidance. See the [CLI Reference](docs/cli.md) for flags, exit codes, side effects, and the Windows asynchronous-replacement note.

### Shell completion (optional)

`token-usage completion <bash|zsh|fish|powershell>` writes the completion script for the selected shell to standard output. For example, in the current zsh session:

```bash
source <(token-usage completion zsh)
```

For persistent installation, see `token-usage completion <shell> --help`. See the [CLI Reference](docs/cli.md) for complete command documentation.

### Daemon

| Command | Purpose |
|------|------|
| `start` | Starts the daemon in the background and returns after the monitor-ready handshake; if already running, returns its PID idempotently. |
| `status` | Shows runtime status, startup phase, and autostart drift detection in five states; read-only. |
| `stop` | Stops the current daemon without deleting its autostart definition; idempotent when it is not running. |
| `restart` | Stops the old daemon and starts a new one under one process-control lock; tells you to use `start` if none is running. |

> `start`, `stop`, `restart`, and `status` never modify the configuration, plist, or Registry; they manage only the current daemon. The autostart definition converges through `config set daemon.autostart` or the TUI.

## Common Scenarios

### First initialization

```bash
token-usage config init
token-usage collect all
token-usage start
```

### Add a normal client

```bash
token-usage config set clients.zcode.enabled true
token-usage collect all --client zcode
```

Here, “add” means adding an existing collector type to user configuration or changing it from disabled to enabled. An unknown client name is rejected by `config set`; adding a brand-new collector type is not only a configuration change. Without a router, the command finishes normally without router backfill.

### Add a client with a router

```bash
token-usage config set clients.claude.enabled true
token-usage config set clients.claude.router cc_switch
token-usage config set routers.cc_switch.db_path ~/.cc-switch/cc-switch.db
token-usage collect all --client claude
```

`collect all --client claude` completes both the full `messages` scan and full router backfill in one command.

If the daemon is running before collection, the action suggestions from `config set` or the TUI are combined as follows:

```bash
token-usage stop
token-usage collect all --client claude
token-usage start
```

After establishing monitoring, `start` also performs startup catch-up to collect data created between the last manual collection and monitor readiness.

### Add a router to an existing client later

```bash
token-usage collect router --client claude
```

This backfills only the router; it does not recollect client messages.

### Repair failed groups

```bash
token-usage errors
token-usage collect retry
token-usage collect retry --client codex
```

### Stop temporarily while keeping autostart

```bash
token-usage stop
```

`stop` does not delete the plist or Registry entry. The current session remains stopped, but the next login starts according to the autostart configuration.

### Reload configuration

```bash
token-usage config set daemon.poll_interval 60
token-usage restart
```

If the daemon is not running, `restart` fails and suggests:

```bash
token-usage start
```

### Enable autostart without starting immediately

```bash
token-usage config set daemon.autostart true
# The autostart definition is saved immediately; the current process is unchanged.
token-usage start  # Start now if needed.
```

### Disable autostart while keeping the daemon running

```bash
token-usage config set daemon.autostart false
# The current daemon keeps running; it will not autostart at the next login.
```

## Daemon Lifecycle

The daemon has two fully decoupled layers: **runtime state** and **autostart state**.

- **Runtime state** (`start`/`stop`/`restart`/`status`) manages the currently running daemon. The daemon lock (`<data_dir>/token-usage.lock`) is the **only source of truth** for liveness; PID and runtime-state files are best-effort location/status metadata.
- **Autostart state** (`config set daemon.autostart` / TUI) only synchronizes the operating-system service definition (a macOS plist or Windows Registry Run key); it **never** starts or stops the current daemon.

Therefore:

- `stop` stops the current session, while the next login still starts according to the autostart configuration because the definition is retained.
- `config set daemon.autostart false` leaves the current daemon running, while preventing autostart at the next login.
- To apply an autostart change in the current session, run `stop` then `start` (or `restart`) manually.

After `start`, the daemon performs **startup catch-up**: once monitoring is ready, it collects incremental data created between the last manual collection and monitoring readiness, closing the data window around stop → collect → start. Partial catch-up failures appear in `status` and `errors`.

For the full process-control model (control lock, daemon lock, parent-child lease, PID + runtime-state, and startup catch-up ordering), see the [Architecture](docs/architecture.md); for command-level details, see the [CLI Reference](docs/cli.md).

## Configuration

The configuration file is `~/.token-usage/config.toml` in TOML format, and you may add comments manually. All clients are disabled by default: enable the ones you use with `clients.<name>.enabled = true`; the program fills the data-source paths from each tool's default location.

> `config set` and TUI saves fully rewrite the user configuration file, so existing comments and map-key ordering are not preserved. Back up handwritten notes first.

There are two read-only ways to inspect configuration, with different purposes:

- `config get <key>` reads the **raw user-configuration value** by dotted key: the value explicitly written in the configuration file. It does not expand `~`, fill default paths, or clamp numeric values. Fields not explicitly configured return their zero value.
- `config show` outputs complete **effective TOML**: the runtime configuration after expanding `~` and filling core defaults for `data_dir`, `daemon`, and `log`, plus registry default paths for clients and routers. It emits pure TOML without a prefix, is suitable for scripts and redirection, and is read-only: it does not create configuration/database/log files or acquire a process lock.

> `config show` includes local paths: `~` is expanded; explicitly relative paths and their derived default paths (for example, `log.dir` derived from `data_dir` and `sessions_dir` derived from `state_dir`) remain relative; other home-based defaults are absolute paths. Check for sensitive information before sharing. Its output is not a template to overwrite the user configuration: writing it back would freeze default paths and discard comments.

The example below shows clients in the enabled state; the template generated by `config init` ships with every client `enabled = false` (plus a commented `router` line and provider-alias example).

```toml
# Data directory for the database, logs, PID, and locks
data_dir = "~/.token-usage"

[clients.claude]
enabled = true
router = "cc_switch"          # Router attribution (currently effective only for the Claude family)

[clients.opencode]
enabled = true

[clients.codex]
enabled = true
# paths.state_dir = "~/.codex"  # Example dotted-key override; omit to use the default

[clients.workbuddy]
enabled = true

[clients.zcode]
enabled = true

[clients.autoclaw]
enabled = true

# Router middleware; the table name is the implementation type
[routers.cc_switch]
# Omit db_path to use the default ~/.cc-switch/cc-switch.db

# Provider display-name mapping for CC Switch router attribution (raw name = display name)
[provider_aliases]
"Zhipu AI Coding Plan" = "Zhipu GLM"

[daemon]
poll_interval = 30            # SQLitePoller interval in seconds
autostart = false             # Autostart (macOS launchd / Windows Registry)
                              # Before enabling it, run token-usage collect all once to initialize historical data

[log]
level = "info"
dir = "~/.token-usage/logs"
max_days = 7
```

> **Current router-attribution support**: only Claude (Code/Desktop) with `router = "cc_switch"` receives message-level attribution backfill. Configuration entry points reject a non-empty `router` on other clients (OpenCode/Codex/WorkBuddy/ZCode/AutoClaw): `config set clients.X.router` fails up front, and the TUI neither offers nor saves one. Existing configurations that already contain such a value are still read without errors; their raw logs would only be written to `raw_router_logs` without backfilling `messages`, because CC Switch recognizes only the Claude family in `app_type`.
>
> **Provider aliases**: `provider_aliases` only normalizes provider display names backfilled by CC Switch; each key must exactly match the raw provider name. After changing it, follow the command suggestion to run `collect router --client <name>` (or `collect all --client <name>`) and backfill existing attribution data.

## Platform Support

| Platform | Builds | Daemon | Autostart |
|------|------|----------|----------|
| macOS | ✅ | ✅ | ✅ launchd |
| Windows | ✅ | ✅ | ✅ Registry Run key |

## Development

```bash
# Build for the current platform
make build

# Cross-compile (darwin arm64/amd64 + windows amd64)
make build-all

# Run tests
go test ./...

# Run the race detector
go test -race ./...
```

`make build`, `make build-all`, and `make install` inject `Version`, `Commit`, and `BuildTime` into `internal/buildinfo` through `-ldflags -X` (default `VERSION=dev`) for the `--version` flag and `version` command. Without injected values, direct `go build` reports version `dev` and build time `unknown`; its commit falls back to the Go VCS revision when available. `go run` uses a temporary cached executable and may lack VCS information, showing `commit: unknown`; do not use it to verify build metadata.

For installation details, see [docs/install.md](docs/install.md); for detailed architecture, see [docs/architecture.md](docs/architecture.md); for CLI commands, see [docs/cli.md](docs/cli.md).

## Contributing

Issues and pull requests are welcome. Read the [Contributing Guide](CONTRIBUTING.md) before opening a PR. In particular:

1. Ensure the relevant tests pass: `go test ./...`.
2. Use a one-sentence Chinese commit message without prefixes such as `feat` or `fix`.
3. Keep each PR focused on one change topic.

## License

This project is released under the [MIT License](LICENSE).
