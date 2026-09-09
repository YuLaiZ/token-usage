package collector

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/mattn/go-runewidth"
)

// B 层派生标题：从会话 JSONL 的首条有效 user 消息提取可读文本。
// 清洗顺序：系统包装前缀跳过 → 成对协议包装剥离 → cron 前缀剥离 →
// 单行化 → 显示宽度截断；全部基于对真实会话文件的形态实测。

// autoClawCronPrefixRe 匹配 cron 触发消息的内嵌前缀 `[cron:<uuid> <label>]`；
// 闭合 ] 取 label 后第一个 ]（label 含 ] 时正则不再跨越，按原文剩余处理）。
var autoClawCronPrefixRe = regexp.MustCompile(`^\[cron:[0-9a-f-]{36} [^\]]*\]`)

const (
	autoClawAuthoredStart = "<<<AUTOCLAW_USER_AUTHORED_REQUEST_START>>>"
	autoClawAuthoredEnd   = "<<<AUTOCLAW_USER_AUTHORED_REQUEST_END>>>"
	// autoClawTitleMaxWidth 派生标题的显示宽度上限（StringWidth 口径，CJK 单字 2 列）。
	autoClawTitleMaxWidth = 100
)

// deriveAutoClawSessionTitle 从 user 行 content 提取派生标题；
// 无有效文本时返回空串（空值依赖 upsert 的空 title 不覆盖语义）。
func deriveAutoClawSessionTitle(content json.RawMessage) string {
	text := autoClawUserText(content)
	if text == "" {
		return ""
	}
	trimmed := strings.TrimSpace(text)
	// 系统包装前缀跳过：协议注入与 evolution-check 类系统消息。
	if strings.HasPrefix(trimmed, "<system-reminder>") || strings.HasPrefix(trimmed, "[SYSTEM:") {
		return ""
	}
	// IM 渠道成对包装剥离取内文；仅 START 无 END 视为无效消息。
	if start := strings.Index(trimmed, autoClawAuthoredStart); start >= 0 {
		rest := trimmed[start+len(autoClawAuthoredStart):]
		end := strings.Index(rest, autoClawAuthoredEnd)
		if end < 0 {
			return ""
		}
		trimmed = strings.TrimSpace(rest[:end])
	}
	// cron 触发消息剥内嵌前缀。
	trimmed = strings.TrimSpace(autoClawCronPrefixRe.ReplaceAllString(trimmed, ""))
	// 单行化：换行与连续空白折叠为单空格。
	trimmed = strings.Join(strings.Fields(trimmed), " ")
	if trimmed == "" {
		return ""
	}
	return truncateDisplayWidth(trimmed, autoClawTitleMaxWidth)
}

// autoClawUserText 归一 user 行 content 的双形态（字符串 / text block 数组），
// 数组取首个 type=="text" block；未知形态（null/数字/对象/无 text block）返回空。
func autoClawUserText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}

// truncateDisplayWidth 按显示宽度截断（runewidth.StringWidth 口径），
// 纯截断不追加省略号（存库原值语义，区别于 UI 层展示截断的 "..." 惯例）。
// 逐 rune 累加与 StringWidth 的字形簇口径在 ZWJ/组合 emoji 上存在偏差
// （可能提前截断，偏短保守），"截断后宽度 ≤ 上限"不变量仍成立。
func truncateDisplayWidth(s string, limit int) string {
	if runewidth.StringWidth(s) <= limit {
		return s
	}
	w := 0
	for i, r := range s {
		w += runewidth.RuneWidth(r)
		if w > limit {
			return s[:i]
		}
	}
	return s
}
