package update

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// version.go 实现 token-usage CLI 自更新使用的 Release tag 解析与版本比较。
//
// 仅支持两种严格形态：
//   - 稳定版：vMAJOR.MINOR.PATCH
//   - 候选版：vMAJOR.MINOR.PATCH-rc.N
//
// 每个数值分量不允许前导零；rc 编号 N 为 >=1 的正整数。
// 显式拒绝：无 v 前缀、构建元数据（+meta）、beta/alpha/nightly 等未知预发布标识、
// 模糊前缀匹配。比较顺序：major → minor → patch → 稳定版高于同号 rc → rc.N 升序。
//
// 本文件不进行任何 I/O 或网络访问，纯字符串解析。

// Version 表示解析后的语义版本值对象。
//
// RC 字段：0 表示稳定版（无预发布后缀），>=1 表示对应 v...-rc.N 的候选编号。
// 通过 RC 字段而非独立的布尔位表达「是否预发布」，避免同名字段与方法冲突，
// 并保证 rc.N 可直接参与 Compare 的数值比较。
type Version struct {
	Major int
	Minor int
	Patch int
	RC    int // 0=稳定版；>=1 对应 -rc.N 后缀
}

// ParseVersion 把 Release tag 字面量解析为 Version。
//
// 成功时返回严格解析结果；任何不合规输入均返回错误与零值 Version，
// 不返回半解析结果，杜绝调用方误用。
func ParseVersion(s string) (Version, error) {
	if len(s) == 0 {
		return Version{}, errors.New(ui.Bi("version string must not be empty", "版本号不能为空"))
	}
	if s[0] != 'v' {
		return Version{}, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("version string %q is missing the v prefix", s),
			fmt.Sprintf("版本号 %q 缺少 v 前缀", s),
		))
	}
	rest := s[1:]

	var core, rcPart string
	if idx := strings.Index(rest, "-"); idx >= 0 {
		core = rest[:idx]
		rcPart = rest[idx+1:]
		if rcPart == "" {
			return Version{}, fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("version string %q has an empty prerelease segment", s),
				fmt.Sprintf("版本号 %q 的预发布段为空", s),
			))
		}
	} else {
		core = rest
	}

	major, minor, patch, err := parseNumericTriple(core)
	if err != nil {
		return Version{}, fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("failed to parse version string %q", s),
			fmt.Sprintf("版本号 %q 解析失败", s),
		), err)
	}

	rc, err := parseRC(rcPart)
	if err != nil {
		return Version{}, fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("failed to parse version string %q", s),
			fmt.Sprintf("版本号 %q 解析失败", s),
		), err)
	}

	return Version{Major: major, Minor: minor, Patch: patch, RC: rc}, nil
}

// parseNumericTriple 解析 MAJOR.MINOR.PATCH 三段，逐段校验无前导零。
func parseNumericTriple(core string) (int, int, int, error) {
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("expected three numeric segments MAJOR.MINOR.PATCH, got %d segments", len(parts)),
			fmt.Sprintf("需要三段数字 MAJOR.MINOR.PATCH，实际 %d 段", len(parts)),
		))
	}
	n0, err := parseNoLeadingZero(parts[0])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("MAJOR segment %q", parts[0]),
			fmt.Sprintf("MAJOR 段 %q", parts[0]),
		), err)
	}
	n1, err := parseNoLeadingZero(parts[1])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("MINOR segment %q", parts[1]),
			fmt.Sprintf("MINOR 段 %q", parts[1]),
		), err)
	}
	n2, err := parseNoLeadingZero(parts[2])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("PATCH segment %q", parts[2]),
			fmt.Sprintf("PATCH 段 %q", parts[2]),
		), err)
	}
	return n0, n1, n2, nil
}

// parseRC 解析预发布后缀。空字符串表示稳定版；"rc.N"（N>=1，无前导零）为候选版。
// 任何其它标识（beta/alpha/nightly/构建元数据）一律拒绝。
func parseRC(rcPart string) (int, error) {
	if rcPart == "" {
		return 0, nil // 稳定版
	}
	// 显式拒绝构建元数据：rcPart 内不允许出现 '+'。
	if strings.Contains(rcPart, "+") {
		return 0, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("prerelease segment %q contains build metadata (not allowed)", rcPart),
			fmt.Sprintf("预发布段 %q 含构建元数据（不允许）", rcPart),
		))
	}
	const prefix = "rc."
	if !strings.HasPrefix(rcPart, prefix) {
		return 0, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("prerelease segment %q must look like rc.N", rcPart),
			fmt.Sprintf("预发布段 %q 必须形如 rc.N", rcPart),
		))
	}
	num := rcPart[len(prefix):]
	if num == "" {
		return 0, errors.New(ui.Bi("rc number is missing", "rc 编号缺失"))
	}
	n, err := parseNoLeadingZero(num)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("rc number %q", num),
			fmt.Sprintf("rc 编号 %q", num),
		), err)
	}
	if n < 1 {
		return 0, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("rc number must be a positive integer, got %d", n),
			fmt.Sprintf("rc 编号必须为正整数，实际 %d", n),
		))
	}
	return n, nil
}

// parseNoLeadingZero 解析非负整数，拒绝前导零（"0" 本身合法，"01"/"00" 非法）、
// 非数字、空串、负号。
func parseNoLeadingZero(s string) (int, error) {
	if s == "" {
		return 0, errors.New(ui.Bi("numeric segment is empty", "数字段为空"))
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("leading zero found: %q", s),
			fmt.Sprintf("含前导零: %q", s),
		))
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%s", ui.Bi(
				fmt.Sprintf("non-digit character found: %q", s),
				fmt.Sprintf("含非数字字符: %q", s),
			))
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", ui.Bi("failed to parse number", "数值解析失败"), err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("negative numbers are not supported: %q", s),
			fmt.Sprintf("不支持负数: %q", s),
		))
	}
	return n, nil
}

// Compare 按既定顺序比较两个 Version：major → minor → patch → 稳定版高于同号 rc →
// rc.N 升序。返回 -1/0/1 分别表示 v 小于/等于/大于 other。
func (v Version) Compare(other Version) int {
	if v.Major != other.Major {
		return signCmp(v.Major, other.Major)
	}
	if v.Minor != other.Minor {
		return signCmp(v.Minor, other.Minor)
	}
	if v.Patch != other.Patch {
		return signCmp(v.Patch, other.Patch)
	}
	// 同三元组：稳定版（RC==0）高于任意候选版（RC>=1）；候选版之间按 N 升序。
	// rank() 把 RC==0 映射为 +inf（最高），RC>=1 保持原值，使比较方向统一为「大者更新」。
	if v.RC == other.RC {
		return 0
	}
	return signCmp(prereleaseRank(v.RC), prereleaseRank(other.RC))
}

// prereleaseRank 把 RC 编号映射为比较序：稳定版（RC==0）返回最大值，
// 候选版返回 RC 原值，从而 stable > rc.2 > rc.1。
func prereleaseRank(rc int) int {
	if rc == 0 {
		// 稳定版高于任何候选版；用 int 最大值保证数值大于任意 RC>=1。
		return int(^uint(0) >> 1)
	}
	return rc
}

// IsPrerelease 报告是否为候选版本（RC>=1）。
func (v Version) IsPrerelease() bool { return v.RC >= 1 }

// String 还原规范字面量。稳定版输出 vMAJOR.MINOR.PATCH；候选版输出
// vMAJOR.MINOR.PATCH-rc.N。输出可作为 ParseVersion 的合法输入（往返一致）。
func (v Version) String() string {
	core := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.RC > 0 {
		return fmt.Sprintf("%s-rc.%d", core, v.RC)
	}
	return core
}

// signCmp 返回 a 相对 b 的 -1/0/1。
func signCmp(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
