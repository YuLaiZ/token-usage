package cli

import (
	"github.com/spf13/cobra"

	"github.com/YuLaiZ/token-usage/internal/buildinfo"
)

// NewRootCmd 构造用户可见的根命令。
//
// 仅在此处调用一次 buildinfo.Current()，随后由内部 newRootCmd
// 同时配置根命令的 Version/template 与 version 子命令，保证两者
// 使用同一份构建信息快照。
func NewRootCmd() *cobra.Command {
	return newRootCmd(buildinfo.Current())
}

// newRootCmd 用注入的 info 快照构造根命令。
//
// 根命令 Version 与 version 子命令共享同一份 info，避免重复读取
// 真实环境导致的快照漂移。version 子命令挂在最前以提升命令树可读性。
func newRootCmd(info buildinfo.Info) *cobra.Command {
	root := &cobra.Command{
		Use:     "token-usage",
		Short:   "本地 LLM 使用数据统计工具",
		Long:    "采集、分析和查询各 AI 客户端的 token 使用情况",
		Version: info.Version,
	}
	// 固定短输出为 "token-usage <version>\n"，不使用 Cobra 默认的
	// "token-usage version <version>"。
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")

	root.AddCommand(
		newVersionCmd(info),
		newConfigCmd(),
		newCollectCmd(),
		newQueryCmd(),
		newErrorsCmd(),
		newStartCmd(),
		newStatusCmd(),
		newStopCmd(),
		newRestartCmd(),
		newInternalRunCmd(),
	)

	return root
}
