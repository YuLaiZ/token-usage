// Package buildinfo 规范化版本与构建元数据。
//
// 业务代码统一通过 Current() 获取规范化的构建信息快照，
// 不直接散读包级注入变量。版本/提交/构建时间来自构建期
// 通过 -ldflags -X 注入的包级变量；当未注入时，回退到
// 运行时可获得的等价信息（debug.BuildInfo 与 VCS settings）。
//
// 解析逻辑抽为未导出的纯函数 resolve，便于单测注入可控输入，
// 与真实环境（debug.ReadBuildInfo / runtime.*）解耦。
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// 程序展示名称，Short/Detail 输出固定前缀。
const progName = "token-usage"

// 以下包级变量在构建期由 -ldflags -X 注入；未注入时保留默认值。
var (
	// Version 语义版本号；未注入时为 "dev"。
	Version = "dev"
	// Commit 完整 VCS revision；未注入时为空。
	Commit = ""
	// BuildTime 构建时间（RFC3339 等格式）；未注入时为空。
	BuildTime = ""
)

// Info 是规范化的构建元数据快照。
//
// Commit 字段内部保留完整 revision（或 "unknown"），
// 仅在展示层（Detail）截断为前 12 位并按需追加 "-dirty"。
// Modified 不直接对外输出字段语义，仅影响 Detail 的 commit 行后缀。
type Info struct {
	Version   string
	Commit    string
	BuildTime string
	GoVersion string
	GOOS      string
	GOARCH    string
	Modified  bool
}

// versionVars 承载构建期可注入的字符串变量值。
type versionVars struct {
	Version   string
	Commit    string
	BuildTime string
}

// Current 读取真实运行环境并返回规范化的构建信息。
//
// 仅在此处接触真实环境（debug.ReadBuildInfo、runtime.Version、
// runtime.GOOS/GOARCH、包级注入变量），随后委托纯函数 resolve。
func Current() Info {
	in := versionVars{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
	}
	var bi *debug.BuildInfo
	if raw, ok := debug.ReadBuildInfo(); ok {
		bi = raw
	}
	return resolve(in, bi, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// resolve 是核心解析纯函数：依据注入变量、可选 debug.BuildInfo、
// 以及 runtime 提供的 GoVersion/GOOS/GOARCH，按既定优先级归一化输出 Info。
//
// bi 为 nil 表示无 build info（等价于取不到 Main.Version 与 settings）。
func resolve(in versionVars, bi *debug.BuildInfo, goVer, goos, goarch string) Info {
	out := Info{
		Version:   resolveVersion(in.Version, bi),
		Commit:    resolveCommit(in.Commit, bi),
		BuildTime: resolveBuildTime(in.BuildTime),
		GoVersion: resolveGoVersion(bi, goVer),
		GOOS:      goos,
		GOARCH:    goarch,
		Modified:  resolveModified(bi),
	}
	// 无 revision 时 Modified 无意义，强制回退，避免展示层出现 "unknown-dirty"。
	if out.Commit == "unknown" {
		out.Modified = false
	}
	return out
}

// resolveVersion 归一化版本号。
//
// 优先级：
//  1. 注入的非空且非 "dev" 值；
//  2. debug.BuildInfo.Main.Version，排除空值、"(devel)" 与本地构建的伪版本号；
//  3. 回退 "dev"。
//
// 第 2 优先级仅用于捕获 go install pkg@v0.1.0 注入的真实 SemVer 模块版本，
// 本地 go build / make build 产生的伪版本号一律排除并回退到 "dev"。
//
// 不自动补 "v" 前缀，不把 commit 转成版本号。
func resolveVersion(injected string, bi *debug.BuildInfo) string {
	if injected != "" && injected != "dev" {
		return injected
	}
	if bi != nil {
		if mv := bi.Main.Version; mv != "" && mv != "(devel)" && !isPseudoVersion(mv) {
			return mv
		}
	}
	return "dev"
}

// resolveCommit 归一化提交 revision（内部保留完整值）。
//
// 优先级：
//  1. 注入的非空 commit；
//  2. debug.BuildInfo settings 中的 vcs.revision（完整 revision）；
//  3. 回退 "unknown"。
//
// 展示层截断与 dirty 后缀由 Detail 处理，本函数不做截断。
func resolveCommit(injected string, bi *debug.BuildInfo) string {
	if injected != "" {
		return injected
	}
	if bi != nil {
		if rev := readSetting(bi.Settings, "vcs.revision"); rev != "" {
			return rev
		}
	}
	return "unknown"
}

// resolveBuildTime 归一化构建时间。
//
// 优先级：
//  1. 注入的非空 BuildTime；
//  2. 固定 "unknown"。
//
// 禁止使用 vcs.time 或 SOURCE_DATE_EPOCH 等环境变量回填。
func resolveBuildTime(injected string) string {
	if injected != "" {
		return injected
	}
	return "unknown"
}

// resolveGoVersion 归一化 Go 工具链版本。
//
// 优先取 debug.BuildInfo.GoVersion；缺失时回退 runtime version。
func resolveGoVersion(bi *debug.BuildInfo, fallback string) string {
	if bi != nil && bi.GoVersion != "" {
		return bi.GoVersion
	}
	return fallback
}

// resolveModified 从 settings 读取 vcs.modified 标记。
//
// 仅当值为 "true" 时认为工作区有改动。
func resolveModified(bi *debug.BuildInfo) bool {
	if bi == nil {
		return false
	}
	return readSetting(bi.Settings, "vcs.modified") == "true"
}

// readSetting 在 settings 中按键取值；找不到返回空串。
func readSetting(settings []debug.BuildSetting, key string) string {
	for _, s := range settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// isPseudoVersion 判断是否为 Go 模块伪版本号。
//
// 本地直接 go build / make build 时，debug.ReadBuildInfo().Main.Version
// 为伪版本号（形如 v0.0.0-20260730061846-59a8d5538012+dirty），既非空值
// 也非 "(devel)"，但不是真实的 SemVer 模块版本。此判断用于把这类伪版本
// 排除，使其回退到默认的 "dev"，与"本地默认版本为 dev"的设计一致。
// go install pkg@v0.1.0 时 Main.Version 为真实 SemVer（如 v0.1.0），不会被误判。
func isPseudoVersion(v string) bool {
	return strings.HasPrefix(v, "v0.0.0-")
}

// Short 返回单行短版本字符串，格式为 "<progName> <version>\n"。
func (i Info) Short() string {
	v := i.Version
	if v == "" {
		v = "dev"
	}
	return fmt.Sprintf("%s %s\n", progName, v)
}

// Detail 返回严格五行的详细版本信息（末尾换行）。
//
// commit 行在展示层截断为前 12 位；Modified=true 且存在有效 revision
// （即 Commit != "unknown"）时追加 "-dirty"；总行数恒为 5。
func (i Info) Detail() string {
	version := i.Version
	if version == "" {
		version = "dev"
	}
	commit := displayCommit(i.Commit, i.Modified)
	buildTime := i.BuildTime
	if buildTime == "" {
		buildTime = "unknown"
	}
	goVer := i.GoVersion
	if goVer == "" {
		goVer = "unknown"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s %s\n", progName, version))
	b.WriteString(fmt.Sprintf("commit: %s\n", commit))
	b.WriteString(fmt.Sprintf("build_time: %s\n", buildTime))
	b.WriteString(fmt.Sprintf("go: %s\n", goVer))
	b.WriteString(fmt.Sprintf("platform: %s/%s\n", i.GOOS, i.GOARCH))
	return b.String()
}

// displayCommit 把内部完整 commit 截断为前 12 位展示，
// 并在 Modified=true 时追加 "-dirty"。
//
// "unknown" 原样返回，不截断也不追加 dirty。
func displayCommit(commit string, modified bool) string {
	if commit == "unknown" || commit == "" {
		return "unknown"
	}
	short := commit
	// 仅当超过 12 位时截断；不足 12 位时原样，避免切片越界。
	if len(commit) > 12 {
		short = commit[:12]
	}
	if modified {
		return short + "-dirty"
	}
	return short
}
