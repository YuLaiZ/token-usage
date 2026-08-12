package update

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"time"
)

// HTTPDoer 是最小 HTTP 客户端契约，镜像 http.Client.Do。
// 生产实现用 *http.Client（携带 HTTPS-only 重定向策略）；测试用 httptest.Server + *http.Client，
// 验证请求 URL、User-Agent、状态码、超时与响应大小，绝不访问真实 GitHub。
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ReleaseClient 抽象 GitHub Release 查询。
// 生产实现经 HTTPDoer 访问 https://api.github.com/repos/YuLaiZ/token-usage，
// 解析 Release tag（vMAJOR.MINOR.PATCH 或 vMAJOR.MINOR.PATCH-rc.N）与资产清单。
//
// tag 为空字符串表示查询 latest 稳定版；非空表示查询精确 tag。
// 资产名固定：token-usage-darwin-arm64 / token-usage-darwin-amd64 /
// token-usage-windows-amd64.exe / SHA256SUMS。
type ReleaseClient interface {
	// FetchRelease 获取指定 tag（空字符串表示 latest）的 Release 元数据与资产清单。
	// latest 端点返回 404 时返回 ErrNoStableRelease；显式 tag 端点返回 404 时返回 ErrVersionNotFound；
	// 草稿、prerelease 不一致、资产集合不合规等返回相应校验错误。
	FetchRelease(ctx context.Context, tag string) (*Release, error)
}

// ExecutableResolver 解析「当前可执行文件路径」。
// 生产实现包装 os.Executable；测试注入固定值，避免依赖测试二进制真实路径。
// 自更新流程用它定位被替换的当前二进制，并据此推导安装目录。
type ExecutableResolver interface {
	Executable() (string, error)
}

// Lstat 是文件元信息查询的 seam，镜像 os.Lstat 语义：不跟随 symlink。
// POSIX 安装据此判断目标路径是否已存在、是否为 symlink，决定原子替换策略；
// 测试注入 fake 避免触碰真实文件系统。
type Lstat interface {
	Lstat(name string) (fs.FileInfo, error)
}

// FileReader 抽象按路径读取完整文件内容，镜像 os.ReadFile。
// 读取已安装版本文件、读 SHA256SUMS / provenance 时使用；
// 测试注入内存 fake，避免真实磁盘 IO。
type FileReader interface {
	ReadFile(name string) ([]byte, error)
}

// TempFileCreator 抽象临时文件创建，镜像 os.CreateTemp(dir, pattern)。
// 下载到 stage 文件、写新二进制时使用；生产实现用 os.CreateTemp
// 并保证 temp 与 target 同目录同卷（参照 fileutil.tempPattern），以便后续原子 rename。
// 测试注入 fake 以隔离真实文件系统。
type TempFileCreator interface {
	CreateTemp(dir, pattern string) (*os.File, error)
}

// Clock 抽象时间源。生产实现用 time.Now；
// 测试注入确定性 fake，驱动轮询/超时与下载过期判断，杜绝真实 wall clock。
type Clock interface {
	Now() time.Time
}

// NonceGenerator 抽象一次性随机标识生成（如下载 temp 后缀、幂等键）。
// 生产实现用 crypto/rand；测试注入确定性 fake，便于断言生成值与清理前缀。
// 生成值仅用于本次更新的临时命名/幂等，不参与安全决策（安全决策走 SHA256/provenance）。
type NonceGenerator interface {
	Nonce() string
}

// ProcessStarter 抽象「启动一个新进程」（替换后启动新版本二进制）。
// 生产实现包装 os/exec（detached spawn）；测试注入 fake 仅记录调用参数，
// 不真正 fork 子进程。返回的 Process 句柄允许调用方做 best-effort 等待或放弃 wait 权，
// 语义参照 internal/control.spawnedProcess。
type ProcessStarter interface {
	Start(ctx context.Context, binPath string, args []string) (Process, error)
}

// Process 抽象已启动的子进程句柄，供 ProcessStarter 返回。
// PID 用于日志/状态展示；Release 放弃 wait 权（detached 子进程由系统收养）。
type Process interface {
	PID() int
	Release() error
}

// PlatformInstaller 抽象「在当前平台完成二进制落地的最后一步」。
// 生产实现按 GOOS 分派：POSIX 做 chmod 0755 + 原子 rename，
// Windows 经 helper 等待父进程退出后 MoveFileEx。
// 测试注入 fake 仅校验调用参数与顺序，不触碰真实文件系统。
//
// 真正的 Install 方法签名在引入具体安装选项后定型，当前只声明 Platform 形状。
type PlatformInstaller interface {
	// Platform 返回当前安装器对应的 GOOS（"darwin"/"linux"/"windows"），
	// 便于上层在分派前做平台断言与日志。
	Platform() string
}
