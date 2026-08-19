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
- **Configurable**: data-source paths, enabled clients, and routers all have ready-to-use defaults.
- **Single-binary distribution**: built with Go and pure-Go SQLite (no CGO), with macOS and Windows support.

## Quick Start

### Installation

Choose one of the installation methods below. Option A (official Release binary) is recommended: it requires no Go toolchain, installs the official Release binary with one command — the only source that supports in-place self-update. Everything is kept under `~/.token-usage` — configuration, database, logs, and the binary at `~/.token-usage/bin/token-usage` — exposed through your user PATH, with no sudo or administrator privileges needed.

#### Option A: Official Release binary (recommended — enables self-update)

**Paste to an AI agent** (it reads this instruction and performs the install itself):

```text
Install the token-usage CLI on this machine: download the official Release binary
matching this platform (token-usage-darwin-arm64 for macOS Apple Silicon,
token-usage-darwin-amd64 for macOS Intel, token-usage-windows-amd64.exe for Windows)
from github.com/YuLaiZ/token-usage/releases, verify its SHA256 against the
SHA256SUMS file of that release, install it as the real file
~/.token-usage/bin/token-usage (Windows:
%USERPROFILE%\.token-usage\bin\token-usage.exe), and add the bin directory
(~/.token-usage/bin on macOS, %USERPROFILE%\.token-usage\bin on Windows) to the
user PATH: on macOS append export PATH="$HOME/.token-usage/bin:$PATH" to the
shell rc file (zsh: ~/.zshrc; bash: the first file login shells read); on
Windows write the user Path value through a Microsoft.Win32.Registry direct
write that preserves the REG_EXPAND_SZ value type and %VAR% literals — do not
use setx or [Environment]::SetEnvironmentVariable — then broadcast
WM_SETTINGCHANGE with lParam `Environment`. If the broadcast fails, sign out
and back in before opening a new terminal. Then open a new terminal (on
Windows, start a new window from the Start menu or taskbar) and confirm with
`token-usage version` (run `token-usage --help` to see the available commands).
```

Or run it manually:

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
```

The script detects your architecture, downloads the newest official Release (prereleases included), verifies its SHA256 against the official `SHA256SUMS`, installs it to `~/.token-usage/bin/token-usage` without sudo, automatically removes any leftover copy from the old installation layout (with manual-removal guidance when that directory is not writable, and with corresponding manual guidance in other cases — such as a directory occupying that path — or when deletion fails, without affecting the installation), and adds `~/.token-usage/bin` to your user PATH by appending a marker block to your shell rc file (zsh and bash only — zsh: `~/.zshrc`; bash: the first file login shells read — `~/.bash_profile` first, then `~/.bash_login`, then `~/.profile`; for other shells the script prints manual PATH guidance instead). Open a new terminal and run `token-usage version` to confirm.

> Non-login interactive shells (some IDE integrated terminals read `~/.bashrc` instead of login files) do not load the login file; add `export PATH="$HOME/.token-usage/bin:$PATH"` there yourself if needed. Interactive zsh terminals always read `~/.zshrc`.

To pin a specific release tag:

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | TAG=v0.1.0-rc.12 bash
```

Published assets:

- `token-usage-darwin-arm64` (macOS Apple Silicon)
- `token-usage-darwin-amd64` (macOS Intel)
- `token-usage-windows-amd64.exe` (Windows)

Windows — or run it manually (save the script first, then execute it):

```powershell
irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"; powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"
```

The script downloads the newest official Release, verifies its SHA256 against the official `SHA256SUMS`, installs it to `%USERPROFILE%\.token-usage\bin\token-usage.exe` without administrator privileges, and appends `%USERPROFILE%\.token-usage\bin` to the user PATH with a type-preserving registry write (the existing `REG_EXPAND_SZ` value type and `%VAR%` entries are kept as-is). Open a new terminal window from the Start menu or taskbar and run `token-usage version` to confirm. To pin a specific release tag, run the downloaded script with `-Tag`:

```powershell
powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1" -Tag v0.1.0-rc.12
```

> On an old TLS environment the first `irm` step still runs before the script, so the in-script TLS fallback cannot rescue it: run `[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12` in the current session first, then run the download step.

Manual install steps on Windows (download → SHA256 verify → put the binary into the bin directory → add it to the user PATH) are covered under Option B below.

A binary installed from an official Release — by the script above, by the AI-agent instruction, or manually through Option B's official-asset path — can update itself in place: the binary is the real file on PATH at `~/.token-usage/bin`, which is exactly what the self-update source check requires.

```bash
token-usage update                  # update to the latest stable release
token-usage update --check          # only check; writes no local files
token-usage update --version v0.2.0 # update (or check) a specific release tag
```

See the [CLI Reference](docs/cli.md) for the full set of flags, exit codes, and side-effect boundaries.

> **Development builds cannot self-update.** Binaries from `make build`, `make build-all`, or `go install` report `Version = dev` (or a pseudo-version); `update` treats such a source as untrusted and prints manual-install guidance instead of overwriting it.
>
> **Supported platforms for self-update:** `darwin/arm64`, `darwin/amd64`, and `windows/amd64` have official assets. On any other platform, `update` reports that there is no official asset and asks you to install manually.
>
> **Manual-upgrade boundary:** `update` only replaces the current binary when it is the official Release asset for the reported version — its SHA256 must match the official asset hash for that version. If the current binary is a `go install`/locally built/symlinked copy, or its version or hash do not match, `update` does not overwrite it and prints manual-install guidance instead.
>
> **Why no symlink into PATH:** the self-update source check requires the running executable to be the real binary file. A call through a symlink resolves to the symlink path and is rejected (the "symlinked copy" case in the boundary above). That is why this layout puts `~/.token-usage/bin` itself on PATH instead of placing a link in another directory.
>
> **Interrupted-update recovery:** the source gate applies to a new Release download. If a previous POSIX update left its restricted local transaction journal behind, a later `update` first restores that recorded transaction to a consistent state; it does not accept or download a new binary during recovery.

To uninstall or migrate from an earlier layout, see [Uninstall and migration](#uninstall-and-migration) below.

#### Option B: Manual binary installation (same layout)

Option B performs the same layout as Option A by hand — the binary at `~/.token-usage/bin/token-usage` (Windows: `%USERPROFILE%\.token-usage\bin\token-usage.exe`), with the bin directory added to your user PATH. The two sub-paths differ in upgrade semantics:

- **Official Release asset** (downloaded and SHA256-verified below): equivalent to Option A, supports in-place self-update.
- **Built from source** (`make build` / `go build`): reports `Version=dev` or a pseudo-version and cannot self-update; upgrade by rebuilding and replacing the file manually (see the development-build note above).

**macOS — official Release asset**:

```bash
# The `latest` link requires a stable release; while only prereleases are
# published it returns 404. Use the newest tag from the Releases page
# (the example below pins v0.1.0-rc.12):
curl -fsSL -o token-usage-darwin-arm64 https://github.com/YuLaiZ/token-usage/releases/download/v0.1.0-rc.12/token-usage-darwin-arm64
curl -fsSL -o SHA256SUMS https://github.com/YuLaiZ/token-usage/releases/download/v0.1.0-rc.12/SHA256SUMS
# Verify the SHA256 against the release's SHA256SUMS:
shasum -a 256 -c SHA256SUMS --ignore-missing
chmod u+x token-usage-darwin-arm64
mkdir -p ~/.token-usage/bin
mv token-usage-darwin-arm64 ~/.token-usage/bin/token-usage
```

Then add `~/.token-usage/bin` to your PATH: append this block to your shell rc file (zsh: `~/.zshrc`; bash: the first file login shells read — `~/.bash_profile` first, then `~/.bash_login`, then `~/.profile`):

```sh
# >>> token-usage path >>>
export PATH="$HOME/.token-usage/bin:$PATH"
# <<< token-usage path <<<
```

> Non-login interactive shells (some IDE integrated terminals read `~/.bashrc` instead of login files) do not load the login file; add `export PATH="$HOME/.token-usage/bin:$PATH"` there yourself if needed. Interactive zsh terminals always read `~/.zshrc`.

Open a new terminal and verify with `token-usage --help` and `token-usage version`.

**Windows — official Release asset**:

```powershell
# Download from the newest tag on the Releases page (the example below pins
# v0.1.0-rc.12), then verify the SHA256 against the release's SHA256SUMS:
curl.exe -fsSL -o token-usage-windows-amd64.exe https://github.com/YuLaiZ/token-usage/releases/download/v0.1.0-rc.12/token-usage-windows-amd64.exe
curl.exe -fsSL -o SHA256SUMS https://github.com/YuLaiZ/token-usage/releases/download/v0.1.0-rc.12/SHA256SUMS
# Verify the SHA256 against the release's SHA256SUMS. Abort before installation
# if the exact asset entry is absent or its expected hash does not match:
$sumsEntry = Select-String -Path SHA256SUMS -Pattern '^[0-9a-fA-F]{64}  token-usage-windows-amd64\.exe$' | Select-Object -First 1
if ($null -eq $sumsEntry) { throw 'SHA256SUMS has no hash for token-usage-windows-amd64.exe.' }
$expected = ($sumsEntry.Line -split '\s+')[0].ToLowerInvariant()
$actual = (Get-FileHash .\token-usage-windows-amd64.exe).Hash.ToLower()
if ($actual -ne $expected) { throw "SHA256 MISMATCH: expected $expected, got $actual" }
'SHA256 OK'
New-Item -ItemType Directory -Force $env:USERPROFILE\.token-usage\bin | Out-Null
Move-Item token-usage-windows-amd64.exe $env:USERPROFILE\.token-usage\bin\token-usage.exe -Force
```

Then append `%USERPROFILE%\.token-usage\bin` to the user PATH with a type-preserving registry write, which keeps the existing `REG_EXPAND_SZ` value type and `%VAR%` entries intact. The already-contained check below also matches existing unexpanded entries (e.g. `%USERPROFILE%\.token-usage\bin`) by expanding them first, mirroring the installer's semantics — expansion applies only to `REG_EXPAND_SZ` values. Do **not** use `setx` or `[Environment]::SetEnvironmentVariable`: `setx` truncates long values, and `SetEnvironmentVariable` rewrites the value as `REG_SZ` with `%VAR%` entries permanently expanded.

```powershell
$dir  = "$env:USERPROFILE\.token-usage\bin"
$key  = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
$raw  = $key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
$kind = if ($key.GetValueNames() -contains 'Path') { $key.GetValueKind('Path') } else { [Microsoft.Win32.RegistryValueKind]::ExpandString }
$norm = ($raw -split ';') | ForEach-Object {
    $lit = $_.Trim().TrimEnd('\')
    $exp = if ($kind -eq 'ExpandString') { [Environment]::ExpandEnvironmentVariables($_).Trim().TrimEnd('\') } else { $lit }
    @($lit, $exp)
}
if ($norm -notcontains $dir) {
    $new = if ([string]::IsNullOrEmpty($raw)) { $dir } else { $raw.TrimEnd(';') + ';' + $dir }
    $key.SetValue('Path', $new, $kind)
}
$key.Close()
```

> A direct registry write does not broadcast the environment change: sign out and back in, then open a new terminal window from the Start menu or taskbar and verify with `token-usage --help` and `token-usage version`.

**Built from source** (macOS or Windows):

```bash
git clone https://github.com/YuLaiZ/token-usage.git && cd token-usage
make build   # produces ./token-usage (make build-all produces dist/token-usage-windows-amd64.exe)
```

Put the built binary into the bin directory and add it to your PATH exactly as in the official-asset steps above (`~/.token-usage/bin/token-usage` on macOS, `%USERPROFILE%\.token-usage\bin\token-usage.exe` on Windows). A source-built binary reports `Version=dev` or a pseudo-version and cannot self-update; to upgrade, rebuild and replace the file under the bin directory manually.

#### Option C: `go install` (requires Go)

```bash
go install github.com/YuLaiZ/token-usage/cmd/token-usage@latest
```

The binary is installed to `$GOBIN` (by default `~/go/bin`); ensure that directory is on `PATH`. Configuration and logs remain under `~/.token-usage/`. Verify the installation with `token-usage --version`.

#### Option D: Build directly with Go (for development)

```bash
git clone https://github.com/YuLaiZ/token-usage.git && cd token-usage
go build -o token-usage ./cmd/token-usage
./token-usage --help
./token-usage --version
```

#### Uninstall and migration

Uninstalling leaves no system-wide leftovers:

1. Stop the daemon if it is running: `token-usage stop`.
2. If autostart was ever enabled, first run `token-usage config set daemon.autostart false`. This removes the autostart definition (the `~/Library/LaunchAgents/<label>.plist` file on macOS, the Registry Run entry on Windows) so it does not keep pointing at a deleted binary and fail at every login.
3. Delete the application directory: `rm -rf ~/.token-usage` (Windows: `Remove-Item -Recurse -Force $env:USERPROFILE\.token-usage`). The current terminal may still have the deleted binary cached; run `hash -r` and confirm `token-usage` no longer resolves, or simply open a new terminal and confirm.
4. Remove the PATH configuration.

   macOS: delete the marker block from your shell rc file:

   ```sh
   # >>> token-usage path >>>
   export PATH="$HOME/.token-usage/bin:$PATH"
   # <<< token-usage path <<<
   ```

   Windows: preferably remove the `bin` directory entry and write the remaining entries back with the same type-preserving `Microsoft.Win32.Registry` direct write used at install time (deleting the `Path` value outright if no other entries remain), keeping the `REG_EXPAND_SZ` value type and `%VAR%` literals. Like the install snippet, entry matching expands unexpanded entries first, but only for `REG_EXPAND_SZ` values, so an existing `%USERPROFILE%` entry is removed as well:

   ```powershell
   $dir  = "$env:USERPROFILE\.token-usage\bin"
   $key  = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
   if ($key.GetValueNames() -contains 'Path') {
       $raw  = $key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
       $kind = $key.GetValueKind('Path')
       $kept = ($raw -split ';') | Where-Object {
           $lit = $_.Trim().TrimEnd('\')
           $exp = if ($kind -eq 'ExpandString') { [Environment]::ExpandEnvironmentVariables($_).Trim().TrimEnd('\') } else { $lit }
           $_ -and ($lit -ne $dir) -and ($exp -ne $dir)
       }
       if (@($kept).Count -gt 0) {
           $key.SetValue('Path', ($kept -join ';'), $kind)
       } else {
           $key.DeleteValue('Path')
           Write-Output 'No remaining entries; the Path value has been deleted.'
       }
   } else {
       Write-Output 'Path value not found; nothing to clean up.'
   }
   $key.Close()
   ```

   or delete the `%USERPROFILE%\.token-usage\bin` entry through the modern "Edit environment variables for your account" dialog and verify the `Path` value type is still `REG_EXPAND_SZ` (the legacy list editor has a known issue of rewriting it as `REG_SZ`). Do **not** use `setx` or `[Environment]::SetEnvironmentVariable` — the former truncates values, the latter degrades the value type. After a direct registry write, sign out and back in (or confirm through the "Edit environment variables for your account" dialog) before new terminal windows pick up the change.
5. If you ever followed the old symlink tutorial, also remove the leftover link: `/usr/local/bin/token-usage` on macOS, `%LOCALAPPDATA%\Microsoft\WindowsApps\token-usage.exe` on Windows.

Migration notes:

- If an older `token-usage` copy sits earlier on PATH (the old Windows tutorial put the exe in an arbitrary directory), locate it with `which token-usage` / `Get-Command token-usage` and remove it; an earlier entry shadows the new layout.
- If autostart was enabled before you reinstalled, run `token-usage config set daemon.autostart true` once in a new terminal (after the PATH change has taken effect) so the definition is rebuilt at the new location. If the configuration file was deleted during uninstall, run `token-usage config init` first — `config set` fails when no configuration file exists.

### First use

```bash
# 1. Initialize the configuration file (with defaults at ~/.token-usage/config.toml) and database
token-usage config init
#    Or open the interactive configuration TUI directly (it initializes first if needed)
token-usage config

# 2. Collect all historical data (do this once on first use)
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

# 3. Keep today's data up to date in one of two ways:
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
| `config init` | Initializes the configuration file and database. |
| `config get <key>` | Reads one user-configuration value by dotted key; it does not expand `~` or fill defaults. |
| `config show` | Outputs complete effective TOML: expands `~`, fills default values/paths, is read-only, and emits pure TOML. |
| `config set <key> <value>` | Writes one configuration value atomically, synchronizes autostart, and prints follow-up actions. |

> `config set daemon.autostart` only synchronizes the autostart definition; it **does not** start or stop the current daemon. To apply it in the current session, run `stop` then `start` (or `restart`) manually.
>
> `config get` returns the raw user-configuration value (without `~` expansion or defaults; fields not explicitly written return their zero value). Use `config show` to inspect the complete effective runtime configuration, including expanded paths and defaults.

### Version

| Command | Purpose |
|------|------|
| `--version` (or `-v`) | One-line short output: `token-usage <version>`; for example, `token-usage v0.1.0`, or `token-usage dev` during local development. |
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

The configuration file is `~/.token-usage/config.toml` in TOML format, and you may add comments manually. Defaults work out of the box: a client only needs `enabled = true`; the program fills the data-source paths from each tool's default location.

> `config set` and TUI saves fully rewrite the user configuration file, so existing comments and map-key ordering are not preserved. Back up handwritten notes first.

There are two read-only ways to inspect configuration, with different purposes:

- `config get <key>` reads the **raw user-configuration value** by dotted key: the value explicitly written in the configuration file. It does not expand `~`, fill default paths, or clamp numeric values. Fields not explicitly configured return their zero value.
- `config show` outputs complete **effective TOML**: the runtime configuration after expanding `~` and filling core defaults for `data_dir`, `daemon`, and `log`, plus registry default paths for clients and routers. It emits pure TOML without a prefix, is suitable for scripts and redirection, and is read-only: it does not create configuration/database/log files or acquire a process lock.

> `config show` includes local paths: `~` is expanded; explicitly relative paths and their derived default paths (for example, `log.dir` derived from `data_dir` and `sessions_dir` derived from `state_dir`) remain relative; other home-based defaults are absolute paths. Check for sensitive information before sharing. Its output is not a template to overwrite the user configuration: writing it back would freeze default paths and discard comments.

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

> **Current router-attribution support**: only Claude (Code/Desktop) with `router = "cc_switch"` receives message-level attribution backfill. For other clients (OpenCode/Codex/WorkBuddy/ZCode/AutoClaw), raw logs are still written to `raw_router_logs` even if `router` is configured, but `messages` are not backfilled because CC Switch recognizes only the Claude family in `app_type`.
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

For detailed architecture, see [docs/architecture.md](docs/architecture.md); for CLI commands, see [docs/cli.md](docs/cli.md).

## Contributing

Issues and pull requests are welcome. Read the [Contributing Guide](CONTRIBUTING.md) before opening a PR. In particular:

1. Ensure the relevant tests pass: `go test ./...`.
2. Use a one-sentence Chinese commit message without prefixes such as `feat` or `fix`.
3. Keep each PR focused on one change topic.

## License

This project is released under the [MIT License](LICENSE).
