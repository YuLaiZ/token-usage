package update

import (
	"context"
	"io/fs"
)

// 本文件定义 Windows staged replacement 的依赖边界。
//
// Windows 不允许替换正在运行的可执行文件，自更新需经专属 helper：
//   - 等待父进程（当前 token-usage CLI）退出，释放对旧 .exe 的句柄；
//   - 用 MoveFileEx(MOVEFILE_REPLACE_EXISTING) 把下载的新 .exe 原子替换到位；
//   - 启动新版本进程；
//   - 把结果写入约定文件，供下一次完整 update（Apply）在来源校验通过后消费。
//
// 这些操作涉及 Windows 系统 API 与真实进程句柄，必须在测试中以 fake 替换。
// 所有 seam 均不暴露为用户可见的 flag / 环境变量 / 配置项。

// ParentWaiter 抽象「等待指定父进程实例退出」。
// 生产实现用 Windows API 按显式身份（PID + 创建时间）打开句柄并等待，杜绝 PID 复用导致
// 误等无关进程；测试注入 fake 记录收到的身份并按预置返回。helper 在 MoveFileEx 前调用本 seam。
type ParentWaiter interface {
	// WaitParentExit 阻塞直到 identity 指定的父进程实例退出或 ctx 取消/超时。
	// identity 来自校验过的 helperPlan.Parent（由父进程 spawn helper 前捕获）。
	// 返回 nil 表示指定父进程已退出，可安全替换旧 .exe；任何无法确认身份的情况返回非 nil。
	WaitParentExit(ctx context.Context, identity ProcessIdentity) error
}

// FileMover 抽象 Windows 的 MoveFileEx 原子替换。
// 生产实现调用 MoveFileExW 并带 MOVEFILE_REPLACE_EXISTING；
// 测试注入 fake 仅校验 from/to 参数并更新内存状态。
type FileMover interface {
	// MoveReplace 把 from 原子移动到 to，覆盖已存在的 to。
	// 语义对齐 MoveFileEx(MOVEFILE_REPLACE_EXISTING)：
	// 成功返回 nil；from 不存在或目标卷不支持原子替换时返回错误。
	MoveReplace(from, to string) error
}

// ResultWriter 抽象「把更新结果写入约定文件，供下一次完整 update（Apply）在来源校验通过后消费」。
// 生产实现按固定路径（如 <data_dir>/update-result.json）写入；
// 测试注入 fake 仅记录写入内容，不触碰真实文件系统。
type ResultWriter interface {
	// WriteResult 把 JSON 序列化后的更新结果写入约定路径。
	// mode 仅在创建新文件时生效；已存在文件保持原权限。
	WriteResult(path string, data []byte, mode fs.FileMode) error
}

// WindowsHelper 组合 ParentWaiter、FileMover 与 ResultWriter，作为 Windows 平台的
// 总装配接口。生产 helper runner 将这些依赖与进程控制逻辑串成一次完整替换；测试用 fake 驱动。
//
// Platform 返回 "windows"，便于上层在分派前做平台断言（与 PlatformInstaller 对齐）。
type WindowsHelper interface {
	Platform() string
	ParentWaiter
	FileMover
	ResultWriter
}
