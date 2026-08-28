package update

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// signature_probe.go 提供 SignatureProbe 的生产实现。
//
// darwin 上执行 codesign -dv 读取当前二进制的签名元信息：该命令只展示元信息、
// 不做完整性校验，本实现仅以其 stderr 中 flags 字段含 adhoc 作为「带 ad-hoc
// 签名标记」的判定依据。探测结论只用于细化 hash 失配分支的提示文案，
// 不参与可信判定；任何失败（非 darwin、codesign 缺失、超时）一律降级 SignatureUnknown。

// execSignatureProbe 是 SignatureProbe 的生产实现。
type execSignatureProbe struct{}

// NewExecSignatureProbe 返回生产签名探测实现。
func NewExecSignatureProbe() SignatureProbe { return execSignatureProbe{} }

// probeTimeout 限制 codesign 子进程的最长执行时间，超时按探测失败降级。
const probeTimeout = 5 * time.Second

func (execSignatureProbe) ProbeSignature(ctx context.Context, binPath string) SignatureProbeResult {
	if runtime.GOOS != "darwin" {
		return SignatureUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "codesign", "-dv", binPath)
	cmd.Stderr = &stderr
	// codesign 对未签名对象会以非零退出并把原因写到 stderr；探测只读 stderr 的
	// flags 字段，退出码不影响结论（未签名不会命中 adhoc，同样降级 unknown）。
	_ = cmd.Run()
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "flags=") && strings.Contains(line, "adhoc") {
			return SignatureAdhoc
		}
	}
	return SignatureUnknown
}
