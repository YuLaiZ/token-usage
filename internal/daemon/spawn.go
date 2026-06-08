// internal/daemon/spawn.go
// 无 build tag：所有平台编译，定义跨平台共用的 SpawnOptions 类型。
// 平台专属的 SpawnDetached + detachedSysProcAttr 在 spawn_unix.go / spawn_windows.go 中。

package daemon

// SpawnOptions 描述 detached spawn 的参数。
// 跨平台共用（spawn_unix.go 与 spawn_windows.go 的 SpawnDetached 均接收此类型）。
type SpawnOptions struct {
	BinPath    string   // 可执行文件绝对路径（os.Executable 探测）
	Args       []string // 固定 ["_run"]（指向 Hidden 内部命令）
	StdoutPath string   // 日志输出文件路径（空则丢弃到 io.Discard）
	StderrPath string   // 日志错误文件路径（空则丢弃到 io.Discard）
	// Lease 是可选的父子 control lease 上下文。
	//   - nil：无父 lease（launchd/注册表直接拉起场景），child 自行获取 control lock。
	//   - 非 nil：父进程持 control lock 并通过 pipe lease 授权 child。spawn helper 把
	//     read end 通过 ExtraFiles（POSIX）或 AdditionalInheritedHandles（Windows）传给 child，
	//     并把 instanceID + fd/handle 写入 child env。
	// Lease 的平台专属 Reader 字段由 spawn_unix.go / spawn_windows.go 解析。
	Lease *LeaseSpawnInput
}

// LeaseSpawnInput 是父进程 spawn child 时传递的 lease 上下文。
//
// InstanceID 是父进程生成的一次性实例标识（通过 TOKEN_USAGE_START_INSTANCE 传 child）。
// Reader 是 pipe read end 的平台载体：
//   - POSIX：*os.File（放入 cmd.ExtraFiles，child 中成为 fd 3+i）；
//   - Windows：syscall.Handle（标记 inheritable，放入 AdditionalInheritedHandles）。
//
// Reader 用 interface{} 而非具体类型，避免本平台无关文件 import syscall/os 平台专属符号；
// spawn_unix.go / spawn_windows.go 内部类型断言取出载体。
type LeaseSpawnInput struct {
	InstanceID string
	// Reader 平台专属的 pipe read end 载体。spawn_unix.go 期望 *os.File；
	// spawn_windows.go 期望 syscall.Handle。ChildEnvLeaseVars 由 spawn helper 追加，
	// 因此这里不重复持有 env 字符串。
	Reader interface{}
}
