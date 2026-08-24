package cli

import (
	"github.com/spf13/cobra"
)

// initConsoleFn 与 InitConsole 分离，测试注入替身验证恢复的双路径覆盖。
var initConsoleFn = InitConsole

// ExecuteWithConsole 是 main 的执行骨架：先建立控制台 UTF-8 会话再执行命令，
// 成功与错误两条路径都恰好恢复一次原代码页（进程退出路径不能依赖 defer）。
// 错误文本已由 cobra 单次输出（"Error: …"），此处只把错误映射为退出码，
// 不重复打印。
func ExecuteWithConsole(cmd *cobra.Command) int {
	restore := initConsoleFn()
	err := cmd.Execute()
	if restore != nil {
		restore()
	}
	if err != nil {
		return 1
	}
	return 0
}
