# Installation

> [简体中文](install.zh-CN.md) | English

This document covers every installation method in detail: the recommended installer script, manual binary installation with the same layout, `go install`, building from source, self-update, and uninstall/migration. For the shortest path, see the [README Quick Start](../README.md#quick-start).

## Layout

Configuration, database, and logs always live under `~/.token-usage` (Windows: `%USERPROFILE%\.token-usage`). The script and manual binary installations also put the binary there — `~/.token-usage/bin/token-usage` (Windows: `%USERPROFILE%\.token-usage\bin\token-usage.exe`) — exposed through your user PATH, with no sudo or administrator privileges needed; `go install` and the direct development build (`go build`) place the binary elsewhere (see their sections). The methods differ in upgrade semantics:

- **Official Release binary** (installed by the script below, by the AI-agent instruction, or manually): supports in-place self-update — the binary is the real file on PATH, which is exactly what the self-update source check requires.
- **Built from source** (`make build` / `go build`): reports `Version = dev` (plain-build pseudo-versions are normalized to `dev`) and cannot self-update by default; run `token-usage update --force` to switch to an official Release asset (automatic updates then work normally), or rebuild and replace the file manually.

Published assets (the platforms with official binaries — also the platforms supported by self-update):

- `token-usage-darwin-arm64` (macOS Apple Silicon)
- `token-usage-darwin-amd64` (macOS Intel)
- `token-usage-windows-amd64.exe` (Windows)

## Install with the Official Script (Recommended)

### macOS

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | bash
```

To pin a specific Release tag:

```bash
curl -fsSL https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.sh | TAG=vX.Y.Z bash
```

The script detects your architecture, downloads the latest stable official Release, verifies its SHA256 against the official `SHA256SUMS`, installs it to `~/.token-usage/bin/token-usage` without sudo, automatically removes any leftover copy from the old installation layout (with manual-removal guidance when that directory is not writable, and with corresponding manual guidance in other cases — such as a directory occupying that path — or when deletion fails, without affecting the installation), and adds `~/.token-usage/bin` to your user PATH by appending a marker block to your shell rc file (zsh and bash only — zsh: `~/.zshrc`; bash: the first file login shells read — `~/.bash_profile` first, then `~/.bash_login`, then `~/.profile`; for other shells the script prints manual PATH guidance instead). Open a new terminal and run `token-usage version` to confirm. To install an RC, pass its exact tag with `TAG=vX.Y.Z-rc.N`.

> Non-login interactive shells (some IDE integrated terminals read `~/.bashrc` instead of login files) do not load the login file; add `export PATH="$HOME/.token-usage/bin:$PATH"` there yourself if needed. Interactive zsh terminals always read `~/.zshrc`.

### Windows

```powershell
irm https://raw.githubusercontent.com/YuLaiZ/token-usage/main/scripts/install.ps1 -OutFile "$env:TEMP\install.ps1"
powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1"
```

Two commands run in order: the first downloads the installer to a temporary file, the second executes it — paste both lines into one PowerShell window. The installer deliberately does not use the one-pipe `irm ... | iex` form: the downloaded script starts with a UTF-8 BOM, which `Invoke-Expression` cannot parse.

To pin a specific Release tag, run the downloaded script with `-Tag`:

```powershell
powershell -ExecutionPolicy Bypass -File "$env:TEMP\install.ps1" -Tag vX.Y.Z
```

The script downloads the latest stable official Release, verifies its SHA256 against the official `SHA256SUMS`, installs it to `%USERPROFILE%\.token-usage\bin\token-usage.exe` without administrator privileges, and appends `%USERPROFILE%\.token-usage\bin` to the user PATH with a type-preserving registry write (the existing `REG_EXPAND_SZ` value type and `%VAR%` entries are kept as-is), then broadcasts `WM_SETTINGCHANGE` so the new PATH is picked up without signing out (if the broadcast fails, the script prints a sign-out-and-back-in hint). Open a new terminal window from the Start menu or taskbar and run `token-usage version` to confirm. To install an RC, pass its exact tag with `-Tag vX.Y.Z-rc.N`.

> On an old TLS environment the first `irm` step still runs before the script, so the in-script TLS fallback cannot rescue it: run `[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12` in the current session first, then run the download step.

## Self-Update

A binary installed from an official Release can update itself in place:

```bash
token-usage update                  # update to the latest stable Release
token-usage update --check          # only check; writes no local files
token-usage update --version vX.Y.Z # update (or check) a specific Release tag
```

`update` only replaces the current binary when it is the official Release asset for the reported version — its SHA256 must match the official asset hash for that version. Development builds (`Version = dev` or a pseudo-version from `make build`, `make build-all`, or `go install`), symlinked copies, and version/hash mismatches are treated as untrusted: a plain `update` refuses to overwrite and prints manual-install guidance. Of these, a hash mismatch (a re-signed binary or `go install pkg@vX.Y.Z`) and a dev build can be overridden explicitly with `update --force`; symlinked copies and non-official tags cannot. Self-update supports exactly the platforms with official assets (`darwin/arm64`, `darwin/amd64`, `windows/amd64`); on any other platform, `update` reports that there is no official asset and asks you to install manually. See [trust and source verification](cli.md#trust-and-source-verification) in the CLI Reference for the full rules, exit codes, side effects, and the Windows asynchronous-replacement note.

> **Why no symlink into PATH:** the self-update source check requires the running executable to be the real binary file. A call through a symlink resolves to the symlink path and is rejected. That is why this layout puts `~/.token-usage/bin` itself on PATH instead of placing a link in another directory.

## Manual Binary Installation (Same Layout)

Manual installation performs the same layout by hand — the binary at `~/.token-usage/bin/token-usage` (Windows: `%USERPROFILE%\.token-usage\bin\token-usage.exe`), with the bin directory added to your user PATH. A SHA256-verified official asset is equivalent to the script installation and supports in-place self-update.

### macOS — Official Release Asset

```bash
# `releases/latest/download/...` always points to the newest stable Release
# and never resolves to a prerelease. To install a specific version, use the
# Releases page URL for that tag instead.
curl -fsSL -o token-usage-darwin-arm64 https://github.com/YuLaiZ/token-usage/releases/latest/download/token-usage-darwin-arm64
curl -fsSL -o SHA256SUMS https://github.com/YuLaiZ/token-usage/releases/latest/download/SHA256SUMS
# Verify the SHA256 against the Release's SHA256SUMS:
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

> **Downloaded the binary with a browser instead of `curl`?** Browser-saved files carry the `com.apple.quarantine` attribute, and the official binaries are ad-hoc signed, which Gatekeeper does not accept for quarantined files: the first run is killed silently (no output, exit code 137). Re-signing in place fixes it:
>
> ```bash
> codesign --sign - --force ~/.token-usage/bin/token-usage
> ```
>
> Removing the attribute alone (`xattr -d com.apple.quarantine ...`) may not be enough because Gatekeeper caches its verdict. Files downloaded with `curl` (or the official script) never get the attribute and are unaffected.
>
> Note the side effect: re-signing rewrites the binary's signature section, so its SHA256 no longer matches the official `SHA256SUMS` and a plain `token-usage update` treats the binary as unverified and refuses to overwrite. Run `token-usage update --force` to have the update replace it with an official asset (automatic updates then work normally again), or install manually.

### Windows — Official Release Asset

```powershell
# Download from the newest stable Release (latest/download never resolves to
# a prerelease; use the Releases page URL for a specific tag), then verify
# the SHA256 against the Release's SHA256SUMS:
curl.exe -fsSL -o token-usage-windows-amd64.exe https://github.com/YuLaiZ/token-usage/releases/latest/download/token-usage-windows-amd64.exe
curl.exe -fsSL -o SHA256SUMS https://github.com/YuLaiZ/token-usage/releases/latest/download/SHA256SUMS
# Verify the SHA256 against the Release's SHA256SUMS. Abort before installation
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

### Built from Source (macOS or Windows)

```bash
git clone https://github.com/YuLaiZ/token-usage.git && cd token-usage
make build   # produces ./token-usage (make build-all produces dist/token-usage-windows-amd64.exe)
```

Put the built binary into the bin directory and add it to your PATH exactly as in the official-asset steps above (`~/.token-usage/bin/token-usage` on macOS, `%USERPROFILE%\.token-usage\bin\token-usage.exe` on Windows). A source-built binary reports `Version = dev` (plain-build pseudo-versions are normalized to `dev`) and cannot self-update by default; run `token-usage update --force` to switch to an official Release asset (automatic updates then work normally), or rebuild and replace the file under the bin directory manually.

## go install (Requires Go)

```bash
go install github.com/YuLaiZ/token-usage/cmd/token-usage@latest
```

The binary is installed to `$GOBIN` (by default `~/go/bin`); ensure that directory is on `PATH`. Configuration and logs remain under `~/.token-usage/`. Verify the installation with `token-usage --version`. A binary installed with a Release tag (for example, `go install github.com/YuLaiZ/token-usage/cmd/token-usage@vX.Y.Z`) is not byte-identical to the official asset; run `token-usage update --force` once to replace it with an official Release asset, after which normal self-update works. The same applies when `@latest` resolves to a Release tag. If `@latest` resolves to a development version, inspect `token-usage version`: literal `Version = dev` is eligible for `--force`, while an explicitly requested pseudo-version must be installed manually.

## Build Directly with Go (for Development)

```bash
git clone https://github.com/YuLaiZ/token-usage.git && cd token-usage
go build -o token-usage ./cmd/token-usage
./token-usage --help
./token-usage --version
```

## Uninstall and Migration

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
