// internal/service/service_darwin.go
//go:build darwin

package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/fileutil"
)

// launchdManager 用 launchd LaunchAgent（用户级 gui/$(id -u) domain）实现开机自启。
// 同时实现 AutoStartManager（纯 definition）与 RuntimeStopper（bootout 当前进程）。
type launchdManager struct{}

func newPlatformManager() Manager { return launchdManager{} }

func (launchdManager) Platform() string { return "launchd" }

// plistPath 返回 ~/Library/LaunchAgents/<Label>.plist
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户主目录失败: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// guiDomain 返回 gui/$(id -u)
func guiDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// buildPlist 生成 LaunchAgent plist XML（纯函数，可单测）。
// KeepAlive: Crashed=true + SuccessfulExit=false（崩溃才重启，正常退出不拉起）。
// ThrottleStartInterval=10 防止配置错误时快速重启循环。
// ProgramArguments[0]=BinPath，[1:]=Args。
// StandardOutPath/StandardErrorPath 重定向到 LogDir 下固定 fallback 文件：
// 仅承载 daemon 日志初始化前的极早期输出（初始化后 unix 侧 fd 接管并入当日
// 结构化文件）；固定文件名避免含日期路径的每日假 drift。
func buildPlist(opts Options) string {
	args := append([]string{opts.BinPath}, opts.Args...)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString(`<dict>` + "\n")
	b.WriteString("\t<key>Label</key>\n\t<string>" + plistEscape(opts.Label) + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range args {
		b.WriteString("\t\t<string>" + plistEscape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n")
	b.WriteString("\t\t<key>Crashed</key>\n\t\t<true/>\n")
	b.WriteString("\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n")
	b.WriteString("\t</dict>\n")
	b.WriteString("\t<key>ThrottleStartInterval</key>\n\t<integer>10</integer>\n")
	fallback := FallbackLogFilePath(opts)
	b.WriteString("\t<key>StandardOutPath</key>\n\t<string>" + plistEscape(fallback) + "</string>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>" + plistEscape(fallback) + "</string>\n")
	b.WriteString(`</dict>` + "\n")
	b.WriteString(`</plist>` + "\n")
	return b.String()
}

func plistEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// parsePlistArgs 解析 plist 字节，返回 ProgramArguments 与 StandardOutPath（纯函数，可单测）。
// 用于 SpecMatches 比对的「读文件→解析」链路。
//
// 实现说明：plist 用 <key>X</key><array>...</array> / <key>X</key><string>...</string>
// 的「key/紧邻兄弟元素」配对表达键值。Go encoding/xml 的 path 标签（如 `xml:"array>string"`）
// 无法表达「array 跟在指定 key 之后」，因此用 token 流手工配对 key→紧跟的下一个元素值，
// 并手工遍历 <array> 内的 <string> 元素以避免缩进空白被误当成元素文本。
func parsePlistArgs(data []byte) (args []string, stdoutPath, stderrPath string, err error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	pendingKey := ""
	for {
		tok, derr := dec.Token()
		if derr == io.EOF {
			break
		}
		if derr != nil {
			return nil, "", "", fmt.Errorf("解析 plist 失败: %w", derr)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "key":
			var k string
			if err := dec.DecodeElement(&k, &se); err != nil {
				return nil, "", "", fmt.Errorf("解析 plist key 失败: %w", err)
			}
			pendingKey = k
		case "array":
			if pendingKey == "ProgramArguments" {
				arr, aerr := decodeStringArray(dec)
				if aerr != nil {
					return nil, "", "", fmt.Errorf("解析 ProgramArguments 失败: %w", aerr)
				}
				args = arr
			} else {
				// 跳过非关心的 array 元素内容
				if err := dec.Skip(); err != nil {
					return nil, "", "", fmt.Errorf("跳过 array 失败: %w", err)
				}
			}
			pendingKey = ""
		case "string":
			if pendingKey == "StandardOutPath" || pendingKey == "StandardErrorPath" {
				var s string
				if err := dec.DecodeElement(&s, &se); err != nil {
					return nil, "", "", fmt.Errorf("解析 %s 失败: %w", pendingKey, err)
				}
				if pendingKey == "StandardOutPath" {
					stdoutPath = s
				} else {
					stderrPath = s
				}
			}
			pendingKey = ""
		}
	}
	return args, stdoutPath, stderrPath, nil
}

// decodeStringArray 在已读取 <array> StartElement 后，遍历其内部 <string> 子元素并收集文本。
// 调用方在收到 array 的 StartElement 后立即调用本函数（此时解码器位于 array 内部第一个 token 之前）。
func decodeStringArray(dec *xml.Decoder) ([]string, error) {
	var arr []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "string" {
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, err
				}
				arr = append(arr, s)
			} else {
				if err := dec.Skip(); err != nil {
					return nil, err
				}
			}
		case xml.EndElement:
			// 到达 </array>
			return arr, nil
		}
	}
}

// Enable 写 plist 文件到 ~/Library/LaunchAgents/，**不 bootstrap**、不启动进程。
// 只维护 definition：登录时由 launchd 自动加载 LaunchAgents，不主动 bootstrap。
// 已存在的 plist 被覆盖（幂等）。MkdirAll 确保 LaunchAgents 目录与兜底日志目录存在
// （Enable 是写路径，此处创建目录不违反 Status 的只读契约；log.dir 允许自定义
// 尚不存在的路径，launchd 按 plist 路径打开 stdio 时目录必须已存在）。
func (launchdManager) Enable(opts Options) error {
	p, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return fmt.Errorf("创建 LaunchAgents 目录失败: %w", err)
	}
	if err := os.MkdirAll(opts.LogDir, 0755); err != nil {
		return fmt.Errorf("创建兜底日志目录 %s 失败: %w", opts.LogDir, err)
	}
	if err := fileutil.ReplaceCompleteFile(p, []byte(buildPlist(opts)), 0644); err != nil {
		return fmt.Errorf("写入 plist 失败: %w", err)
	}
	return nil
}

// bootoutJob 执行 launchctl bootout gui/$(id -u)/<label>。
// 对「未加载」的 job 报错（Could not find service / not booted）降级为成功（幂等）。
// 注意：bootout 不会删除 plist 文件——由调用方决定是否删（Disable 删，StopCurrent 不删）。
func bootoutJob(label string) error {
	cmd := exec.Command("launchctl", "bootout", guiDomain()+"/"+label)
	out, err := cmd.CombinedOutput()
	if err != nil {
		s := string(out)
		if strings.Contains(s, "Could not find service") ||
			strings.Contains(s, "No such process") ||
			strings.Contains(s, "not booted") {
			return nil
		}
		return fmt.Errorf("launchctl bootout 失败: %w（输出: %s）", err, s)
	}
	return nil
}

// Disable 关闭自启：删 plist 文件（**不 bootout**——进程继续跑直到 reboot/登录）。
// 只修改定义文件，不以当前 job/daemon 仍存在作为失败。
// 调用点：Sync(autostart=false) 收敛、漂移重装前的旧定义清理。
// opts 当前 macOS 实现不使用（保持签名一致），保留供未来扩展。
func (launchdManager) Disable(opts Options) error {
	_ = opts
	// 删除 plist 文件，下次登录/重启 launchd 不再加载。
	// 不 bootout：已运行的进程继续跑，直到用户手动 stop 或 reboot/登录。
	p, perr := plistPath()
	if perr != nil {
		return perr
	}
	// 忽略删除错误（文件不存在也算成功，幂等）
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 plist 失败: %w", err)
	}
	return nil
}

// StopCurrent 仅停止当前进程：bootout 卸载 job，**保留 plist 文件**。
// 属于 RuntimeStopper（进程停止层），不属于 AutoStartManager（定义层）。
// 调用点：control 包的 stop 命令。下次登录/重启 launchd 仍会按 plist 自启。
// opts 当前 macOS 实现不使用（保持签名一致）。
func (launchdManager) StopCurrent(opts Options) error {
	_ = opts
	return bootoutJob(Label)
}

// Status 只按 plist 定义报告自启状态，不把 launchd job 是否 loaded、daemon 是否 running
// 混入 Exists。Exists = plist 文件存在；SpecMatches = plist 内容与 opts 是否一致。
// 不通过 launchctl print 探测；job 是否 loaded 是进程层关切，
// 由 control 包的 daemon lock 判活；此处只看定义文件。
func (launchdManager) Status(opts Options) (AutoStartStatus, error) {
	p, err := plistPath()
	if err != nil {
		return AutoStartStatus{}, err
	}
	data, rerr := os.ReadFile(p)
	if rerr != nil {
		if errors.Is(rerr, fs.ErrNotExist) {
			return AutoStartStatus{Exists: false}, nil
		}
		return AutoStartStatus{}, fmt.Errorf("读取 plist 状态失败: %w", rerr)
	}
	specOK, _ := specMatchesFromPlistBytes(opts, data)
	return AutoStartStatus{Exists: true, SpecMatches: specOK}, nil
}

// specMatchesFromPlistBytes 读 plist 字节并比对其 ProgramArguments + StandardOutPath 是否与 opts 一致。
// 供 Status 的「job 已加载」与「job 未加载但 plist 存在」两个分支共用，确保两路径 SpecMatches 判定一致。
// 解析失败返回 err，调用方降级为 SpecMatches=false。
func specMatchesFromPlistBytes(opts Options, data []byte) (bool, error) {
	args, stdoutPath, stderrPath, err := parsePlistArgs(data)
	if err != nil {
		return false, err
	}
	return specMatchesPlist(opts, args, stdoutPath, stderrPath), nil
}

// specMatchesPlist 比对当前 Options 与已安装 plist 的 ProgramArguments 及两条日志路径。
func specMatchesPlist(opts Options, plistArgs []string, stdoutPath, stderrPath string) bool {
	wantArgs := append([]string{opts.BinPath}, opts.Args...)
	if len(plistArgs) != len(wantArgs) {
		return false
	}
	for i := range wantArgs {
		if plistArgs[i] != wantArgs[i] {
			return false
		}
	}
	want := FallbackLogFilePath(opts)
	return stdoutPath == want && stderrPath == want
}
