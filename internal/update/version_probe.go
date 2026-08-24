package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// version_probe.go 实现下载 stage 的生产版本探针。
//
// SHA256SUMS 证明下载字节与 Release 清单一致；本探针额外运行已校验 stage 的
// --version，确认发布资产中实际注入的版本与目标 tag 一致。它不能替代 SHA256
// 校验，但能阻止“清单与二进制彼此一致、却错误注入版本”的发布物被安装。

const (
	stageVersionProbeTimeout   = 15 * time.Second
	maxStageVersionOutputBytes = 4 << 10
	stageVersionOutputPrefix   = "token-usage "
)

var errStageVersionOutputTooLarge = errors.New(ui.Bi("stage --version output exceeds the limit", "stage --version 输出超过上限"))

// stageVersionRunner 抽象实际执行，供单元测试注入固定输出；生产实现见
// runStageVersionCommand。
type stageVersionRunner func(context.Context, string) ([]byte, error)

// execVersionProbe 是生产 VersionProbe。run 保持私有，避免把执行 seam 暴露为用户配置。
type execVersionProbe struct {
	run stageVersionRunner
}

// NewExecVersionProbe 返回生产 stage 版本探针。
func NewExecVersionProbe() VersionProbe {
	return execVersionProbe{run: runStageVersionCommand}
}

// ProbeVersion 执行 stagePath --version，并把严格的一行输出解析为 Release tag。
func (p execVersionProbe) ProbeVersion(ctx context.Context, stagePath string) (string, error) {
	if stagePath == "" || !filepath.IsAbs(stagePath) {
		return "", errors.New(ui.Bi("stage version probe requires a non-empty absolute path", "stage 版本探针要求非空绝对路径"))
	}
	if p.run == nil {
		return "", errors.New(ui.Bi("stage version probe has no runner configured", "stage 版本探针未配置执行器"))
	}
	output, err := p.run(ctx, stagePath)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("failed to run stage --version", "执行 stage --version 失败"), err)
	}
	version, err := parseStageVersionOutput(output)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi("failed to parse stage --version output", "解析 stage --version 输出失败"), err)
	}
	return version, nil
}

// runStageVersionCommand 以有限时间和有限 stdout 运行 stage --version。
// stderr 丢弃：它不属于稳定版本合同，且不应被带入用户可见错误。
func runStageVersionCommand(ctx context.Context, stagePath string) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, stageVersionProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, stagePath, "--version")
	var stdout limitedOutputBuffer
	stdout.limit = maxStageVersionOutputBytes
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if stdout.exceeded {
			return nil, errStageVersionOutputTooLarge
		}
		if probeCtx.Err() != nil {
			return nil, probeCtx.Err()
		}
		return nil, err
	}
	if stdout.exceeded {
		return nil, errStageVersionOutputTooLarge
	}
	return stdout.Bytes(), nil
}

// limitedOutputBuffer 在 child 进程写出超过上限的 stdout 时终止读取，避免异常 stage
// 用无界输出耗尽内存。
type limitedOutputBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedOutputBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return 0, errStageVersionOutputTooLarge
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.exceeded = true
		return remaining, errStageVersionOutputTooLarge
	}
	return b.Buffer.Write(p)
}

// parseStageVersionOutput 只接受 root --version 的稳定输出：
// "token-usage <严格 Release tag>" 加一个 LF 或 CRLF 结尾。
func parseStageVersionOutput(output []byte) (string, error) {
	if len(output) == 0 {
		return "", errors.New(ui.Bi("output is empty", "输出为空"))
	}
	if len(output) > maxStageVersionOutputBytes {
		return "", errStageVersionOutputTooLarge
	}

	raw := string(output)
	if !strings.HasSuffix(raw, "\n") {
		return "", errors.New(ui.Bi("output is missing the trailing newline", "输出缺少末尾换行"))
	}
	line := strings.TrimSuffix(raw, "\n")
	line = strings.TrimSuffix(line, "\r")
	if strings.ContainsAny(line, "\r\n") {
		return "", errors.New(ui.Bi("output must be exactly one line", "输出必须恰好一行"))
	}
	if !strings.HasPrefix(line, stageVersionOutputPrefix) {
		return "", fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("output must start with %q", stageVersionOutputPrefix),
			fmt.Sprintf("输出必须以 %q 开头", stageVersionOutputPrefix),
		))
	}
	version := strings.TrimPrefix(line, stageVersionOutputPrefix)
	if _, err := ParseVersion(version); err != nil {
		return "", fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("version %q in the output is invalid", version),
			fmt.Sprintf("输出中的版本 %q 非法", version),
		), err)
	}
	return version, nil
}
