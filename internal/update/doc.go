// Package update 实现 token-usage CLI 的自更新流程：严格解析 Release tag、查询
// GitHub Release、下载平台资产、校验 SHA256SUMS 与当前二进制来源、原子替换当前
// 可执行文件，并按替换前运行态恢复守护进程。
//
// 版本、清单、下载、来源验证、进程控制、POSIX 事务和 Windows staged replacement
// 均由本包提供。依赖注入边界仅服务测试与生产装配，不暴露为用户 flag、环境变量或配置项。
// 所有外部副作用（网络、文件系统、进程、时钟、随机数）都通过 seam 注入，
// 使实现能在 httptest + t.TempDir + fake clock 下做到无真实网络、
// 无真实 HOME、无真实 daemon 的确定性测试。
//
// 清理约定：任何涉及临时文件的实现都必须按精确 basename 前缀删除普通文件，
// 绝不递归删除目录，复用 internal/fileutil.CleanupKnownTempFiles 的语义。
package update
