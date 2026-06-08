# Contributing to token-usage

> [简体中文](CONTRIBUTING.zh-CN.md) | English

Thanks for helping improve `token-usage`. This guide explains how to propose changes, validate them locally, and prepare a pull request.

## Before You Start

- Check existing issues before starting a feature. Open an issue first when the intended behavior, data source, or compatibility boundary needs discussion.
- Keep a contribution focused. Separate unrelated refactors, formatting-only churn, and feature changes into different pull requests.
- Do not include local usage databases, client logs, configuration files, API keys, access tokens, personally identifiable information, or screenshots containing them in an issue, commit, or pull request.

## Development Setup

Requirements:

- Go `1.26.4` or the version declared in `go.mod`.
- Git and a supported development platform (macOS or Windows).

```bash
git clone https://github.com/YuLaiZ/token-usage.git
cd token-usage
go mod download

# Build for the current platform
make build

# Show available commands
./token-usage --help
```

Use `go run ./cmd/token-usage --help` for quick command development when needed, but use Makefile targets when validating version/build metadata because `go run` may not embed VCS metadata.

## Contribution Workflow

1. Start from the latest `main` and create a focused branch.
2. Read the affected implementation and its existing tests before changing behavior.
3. Add or adjust tests with the implementation whenever the change is testable.
4. Keep the diff minimal and run `gofmt` on changed Go files.
5. Update the relevant English and Chinese documentation together when a user-visible behavior, command, configuration option, data source, or platform constraint changes.
6. Run the validation appropriate to the change, then open a focused pull request.

## Code and Test Expectations

For code changes, start with the focused package tests and run the broader checks before requesting review:

```bash
# All tests
go test ./...

# Race detector
go test -race ./...

# Static checks
go vet ./...

# Build supported release artifacts
make build-all
```

For documentation-only changes, at minimum verify that relative links resolve, code blocks render correctly, and `git diff --check` reports no whitespace errors.

When changing a collector, router adapter, configuration behavior, or daemon lifecycle, include tests for the changed contract and update the architecture/CLI documentation where relevant. Preserve message-level token accounting semantics and do not silently change historical-data behavior.

## Documentation Policy

English is the default public documentation. The Chinese versions are complete counterparts:

| English default | Chinese counterpart |
|---|---|
| `README.md` | `README.zh-CN.md` |
| `docs/architecture.md` | `docs/architecture.zh-CN.md` |
| `docs/cli.md` | `docs/cli.zh-CN.md` |
| `CONTRIBUTING.md` | `CONTRIBUTING.zh-CN.md` |

- Keep both language versions semantically equivalent. Translate user-facing descriptions, headings, tables, and comments while leaving commands, paths, configuration keys, and code identifiers unchanged.
- English documents must link to English counterparts; Chinese documents must link to Chinese counterparts. Keep the language switch near the title accurate.
- Update both versions in the same pull request whenever a documented behavior changes. For a temporary translation gap, state it explicitly in the pull-request description.

## Commit Messages

Use a concise, one-sentence **Chinese** commit message. Do not use Conventional Commit prefixes such as `feat` or `fix`, and do not add multi-line commit bodies or `Co-authored-by` trailers.

```bash
git commit -m "补充贡献指南"
```

## Pull Requests

Before opening a pull request, make sure it:

- Explains the user-visible behavior and the reason for the change.
- Identifies affected commands, configuration, collectors, routers, or platforms when applicable.
- Includes relevant tests and their results, plus any intentionally unrun checks and why.
- Updates matching English and Chinese documentation.
- Does not contain generated binaries, local databases, logs, credentials, or unrelated changes.

Keep one pull request focused on one topic. Small, reviewable changes with clear verification evidence are much easier to evaluate.

## Reporting Issues

For a bug report, include the token-usage version, operating system and architecture, exact command, expected and actual behavior, and minimal reproduction steps. Redact all local paths, logs, configuration values, database content, and credentials before posting.

For a feature request or a new data source, describe the expected behavior, the relevant client/router version, and the available non-sensitive evidence about the local data format and token semantics.
