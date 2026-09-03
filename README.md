# token-usage

> [简体中文](README.zh-CN.md) | English

A local LLM usage analytics CLI. It collects token usage from the AI clients you use, keeps each model invocation as an independent record, and turns the data into reusable reports without SQL.

## Highlights

- **Build the report you actually want — no SQL required.** Define a named multi-dimensional view from `client`, `model`, `provider`, and `project`; then compose built-in and custom views into a reusable, ordered report group. Set it as the default, discover it with `query list`, run it by name, and choose the metric columns in table-based reports and their order.
- Message/API-request-level accounting, including accurate attribution across dates, models, branches, and rewinds.
- Collectors for Claude Code/Desktop, OpenCode, Codex, WorkBuddy, ZCode, and Zhipu-AutoClaw.
- CC-Switch router attribution for the Claude family and Codex, backfilling the actual provider and model from proxy logs.
- One-off commands or a real-time background monitoring daemon, with macOS launchd and Windows Registry autostart.
- A pure-Go, single-binary CLI for macOS and Windows.

## Quick Start

### 1. Install and Update

The official installer downloads the latest stable Release, verifies its SHA256 checksum, installs the binary under `~/.token-usage/bin`, and configures your user PATH. It needs neither `sudo` nor administrator privileges.

**Paste this into an AI agent** (it installs and verifies):

```text
Install token-usage on this machine with the appropriate official installer:

- macOS: curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
- Windows PowerShell:
  irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"
  powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"

Open a new terminal, run `token-usage --help` to see the commands, and verify the installation with `token-usage version`.
```

macOS:

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"
powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"
```

Open a new terminal, then verify the installation:

```bash
token-usage version
```

Both the installer and a manually SHA256-verified official Release asset update themselves in place:

```bash
token-usage update                  # latest stable Release
token-usage update --check          # check only; makes no local changes
token-usage update --version vX.Y.Z # a specific Release
```

For a re-signed official asset, a source build (`Version = dev`), or `go install` of a tagged release, run `token-usage update --force` once to replace it with an official Release asset; later updates work normally. Symlinked copies and non-official tags cannot be converted this way.

When a successful `update` first crosses a version that ships installer-managed shell completion, the success output appends a one-time migration notice with the official installer command — re-running it sets up Tab completion automatically (on zsh it asks interactively). See the [CLI Reference](docs/cli.md) for the exact trigger conditions.

For manual installation, a pinned version, source builds, update trust rules, uninstall, migration, and platform-specific notes, see the [Installation Guide](docs/install.md).

### 2. Configure in the TUI and Collect

All clients start disabled. We recommend the guided configuration TUI: it initializes the configuration on first use, lets you enable the clients you use, and can also configure routers, the daemon, logs, and query settings (view definitions, output columns, and aliases).

```bash
token-usage config
```

After saving the TUI configuration, collect the history of enabled clients:

```bash
token-usage collect all
```

`collect all` scans all enabled clients and includes router attribution backfill where configured. It bypasses `collection_log` date deduplication and is safe to rerun because messages are upserted by `(client, id)`. To keep new data current, start the daemon:

```bash
token-usage start
```

### 3. Query and Build Your Own Reports

```bash
token-usage query             # today's default report (client when unconfigured)
token-usage query model       # group by model
token-usage query 20260701-20260721
```

Dates are positional: `YYYYMMDD` for one day, `YYYYMM` for one month, `YYYY` for one year (single arg only), or a day/month range like `202607-202608`. Any form expands to at most 366 days (one leap year); split longer ranges into multiple runs.

The real payoff is making a report fit the question you return to. In the TUI, open **Query** (press `v` in the main menu) to create a named multi-dimensional view, combine views into an ordered report group, select its default, and choose which metric columns every table shows. You can also define portable views in `~/.token-usage/config.toml`:

```toml
[query]
default = "daily_stack"

[query.subqueries]
model_provider_client = "model,provider,client"

[query.groups]
daily_stack = "client,model,provider,model_provider_client"

# One global, ordered metric-column layout for every query table
# (optional; the default keeps today's seven columns).
[query.output]
columns = ["requests", "input", "output", "total", "cache_hit"]
```

```bash
token-usage query model_provider_client
token-usage query daily_stack 20260701-20260721
token-usage query list
```

`query list` reads only configuration and never opens the usage database, so it is a safe way to discover built-in and configured views. The optional `[query.output]` layout picks which metric columns appear — and in which order — in every query table (`cache_create` is available but hidden by default; `query summary` keeps its complete summary). The [CLI Reference](docs/cli.md#configurable-query-views) describes the validation rules and complete command contract.

## Command Cheat Sheet

| Command | Purpose |
|---|---|
| `config` / `config init` | Open the configuration TUI / create initial configuration and database. |
| `config set <key> <value>` | Change one configuration value. |
| `collect [date]` | Incrementally collect today or a date range. |
| `collect all` | Collect all history without `collection_log` date deduplication; safe to rerun. |
| `collect retry` | Retry unresolved collection failures. |
| `query [date]` | Run the default report. |
| `query client/model/provider/project/session/summary [date]` | Run a built-in report. |
| `query <name> [date]` | Run a configured view or group. |
| `query list` | List views without opening the usage database. |
| `errors` | Show collection failures. |
| `version` / `--version` | Show detailed / one-line version information. |
| `start` / `status` / `stop` / `restart` | Control the background daemon. |
| `completion <shell>` | Print a Bash, Zsh, Fish, or PowerShell completion script. |
| `update` | Self-update an official Release asset in place; use `update --force` once to switch an eligible re-signed, source-built, or `go install` binary. |

Run `token-usage --help` for a command overview, or read the [CLI Reference](docs/cli.md) for flags, exit codes, side-effect boundaries, configuration behavior, and daemon lifecycle.

## Shell Completion

`completion` writes a shell-completion script to standard output. For example, load Zsh completion in the current session:

```bash
source <(token-usage completion zsh)
```

Per-shell prerequisites: **zsh** requires the completion system (`compinit`) to be initialized first — loading the script without it fails with `compdef: command not found`, so add `autoload -U compinit; compinit` to your rc file before the load line (some setups intentionally skip `compinit`; check your rc file first). **bash** requires the bash-completion package (macOS ships bash 3.2 without it — install bash 4+ and `bash-completion@2` via Homebrew). **fish** and **PowerShell** have no prerequisites.

On zsh, `compinit` may report insecure directories (group/other-writable completion directories — most common on old Homebrew installs) and ask whether to continue. Three ways to handle it: answer `y` at the prompt (it reappears in each new shell), repair the directories once with `chmod go-w <dir>` (the fix Homebrew itself recommends), or switch to `compinit -u` to skip the security check permanently.

The official installer script sets all of this up automatically (on zsh it asks interactively); see the [Installation Guide](docs/install.md). For persistent setup, run `token-usage completion <bash|zsh|fish|powershell> --help`.

## Documentation

| Document | What it covers |
|---|---|
| [Installation Guide](docs/install.md) | All installation methods, self-update, uninstall, migration, PATH, and platform notes. |
| [CLI Reference](docs/cli.md) | Command tree, arguments, flags, examples, configuration, and daemon behavior. |
| [Architecture](docs/architecture.md) | Data flow, storage, process control, update design, and extension points. |
| [Contributing Guide](CONTRIBUTING.md) | Development setup, tests, documentation, commits, and pull requests. |

## Platform Support

| Platform | Build | Daemon | Autostart |
|---|---|---|---|
| macOS | Yes | Yes | launchd |
| Windows | Yes | Yes | Registry Run key |

## Development

```bash
make build
go test ./...
go test -race ./...
```

Use `make build-all` for the supported macOS and Windows targets. See the [Contributing Guide](CONTRIBUTING.md) for the full development workflow.

## License

This project is released under the [MIT License](LICENSE).
