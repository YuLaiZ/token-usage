# 为 token-usage 贡献

> 简体中文 | [English](CONTRIBUTING.md)

感谢你帮助改进 `token-usage`。本指南说明如何提出改动、在本地验证，以及准备 Pull Request。

## 开始前

- 开始新功能前，请先查看现有 Issue。如果目标行为、数据源或兼容性边界需要讨论，请先创建 Issue。
- 请保持贡献聚焦。无关的重构、仅格式化的改动和功能改动应拆分为不同的 Pull Request。
- 不要在 Issue、提交或 Pull Request 中包含本地用量数据库、客户端日志、配置文件、API key、access token、个人信息，或含有这些内容的截图。

## 开发环境

要求：

- Go `1.26.4`，或 `go.mod` 中声明的版本。
- Git，以及受支持的开发平台（macOS 或 Windows）。

```bash
git clone https://github.com/YuLaiZ/token-usage.git
cd token-usage
go mod download

# 编译当前平台
make build

# 查看可用命令
./token-usage --help
```

需要快速开发命令时可使用 `go run ./cmd/token-usage --help`；但验证版本/构建元数据时请使用 Makefile target，因为 `go run` 可能不包含 VCS 元数据。

## 贡献流程

1. 基于最新 `main` 创建聚焦的分支。
2. 修改行为前，先阅读受影响的实现和现有测试。
3. 能合理测试的改动，应与实现一同新增或调整测试。
4. 保持最小 diff，并对改动的 Go 文件运行 `gofmt`。
5. 用户可见行为、命令、配置项、数据源或平台约束变更时，同时更新对应的英文和中文文档。
6. 运行与改动匹配的验证后，再创建聚焦的 Pull Request。

## 代码与测试要求

代码改动先运行相关包测试，并在请求评审前运行更完整的检查：

```bash
# 全部测试
go test ./...

# 竞争检测
go test -race ./...

# 静态检查
go vet ./...

# 构建受支持平台的发布产物
make build-all
```

纯文档改动至少要确认相对链接可用、代码块渲染正确，并确保 `git diff --check` 没有空白错误。

修改 collector、router adapter、配置行为或守护进程生命周期时，需要覆盖变更后的行为合同，并在适用时更新架构/CLI 文档。请保持消息级 token 统计语义，避免静默改变历史数据行为。

### 正式发布构建（release-build / release-verify）

`make build` 与 `make build-all` 默认注入 `VERSION=dev`，其产物 `version` 为 `dev`，**不能**用于发布或自动更新。发布前需可重复地构建三份官方资产与 `SHA256SUMS`，并在本地校验：

```bash
make release-build VERSION=vX.Y.Z[-rc.N] COMMIT=<commit> BUILD_TIME=<UTC RFC3339>
make release-verify VERSION=vX.Y.Z[-rc.N]
```

`release-build` 拒绝 `dev` 及任何不符合版本合同（`vMAJOR.MINOR.PATCH[-rc.N]`，无前导零）的 tag，然后产出 `dist/token-usage-darwin-arm64`、`dist/token-usage-darwin-amd64`、`dist/token-usage-windows-amd64.exe` 与严格排序的 `dist/SHA256SUMS`。`release-verify` 经 updater 使用的同一 `internal/update` 解析路径重新校验清单格式与 hash，并在本机为受支持平台时校验注入的 `--version`。只有 `release-build` 产物才能作为 GitHub Release 资产。

## 文档规范

英文是默认的公开文档；中文是完整的对应版本：

| 英文默认入口 | 中文对应版本 |
|---|---|
| `README.md` | `README.zh-CN.md` |
| `docs/architecture.md` | `docs/architecture.zh-CN.md` |
| `docs/cli.md` | `docs/cli.zh-CN.md` |
| `CONTRIBUTING.md` | `CONTRIBUTING.zh-CN.md` |

- 两种语言版本应保持语义等价。翻译用户可见说明、标题、表格和注释；命令、路径、配置 key 和代码标识符保持原样。
- 英文文档链接英文对应文档；中文文档链接中文对应文档。标题附近的语言切换链接必须准确。
- 每次文档化行为变化都应在同一个 Pull Request 中更新两个版本。若暂时存在翻译缺口，请在 Pull Request 描述中明确说明。

## Git 提交信息

使用简洁的一句话**中文**提交信息。不要使用 `feat`、`fix` 等 Conventional Commit 前缀，不要添加多行提交正文或 `Co-authored-by` trailer。

```bash
git commit -m "补充贡献指南"
```

## Pull Request

创建 Pull Request 前，请确保它：

- 说明用户可见行为及改动原因。
- 在适用时标明受影响的命令、配置、collector、router 或平台。
- 包含相关测试及结果，以及刻意未运行的检查和原因。
- 更新匹配的英文和中文文档。
- 不包含生成的二进制、本地数据库、日志、凭证或无关改动。

一个 Pull Request 只聚焦一个主题。小而可评审的改动，加上清晰的验证证据，更容易被评估。

## 发布与打 tag

只有经授权的维护者才能创建版本 tag 或 GitHub Release。

- 发布工作流（`.github/workflows/release.yml`）仅在 push 形如 `v*` 的 tag 时运行。它先校验 tag 合同，以及 tag 指向的 commit 与触发事件 commit 一致（同时适用于 annotated 与 lightweight tag），再依次执行 `go test` / `go vet` / `make release-build` / `make release-verify`；若同名 Release 已存在则直接失败（绝不覆盖、修补、删除或移动资产）。
- 贡献者不要自行 push `v*` tag 或发布 Release。发布候选（`-rc.N`）由工作流标记为 prerelease。

如需发布，请创建 Issue 提出，由维护者打 tag 并发布。

## 报告 Issue

报告 bug 时，请给出 token-usage 版本、操作系统与架构、准确命令、预期与实际行为，以及最小复现步骤。发布前请脱敏所有本地路径、日志、配置值、数据库内容和凭证。

提出功能请求或新数据源时，请说明预期行为、相关 client/router 版本，以及本地数据格式和 token 口径的可公开、非敏感依据。
