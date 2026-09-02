package engine

import (
	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/fsident"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// === startup 跳过门（file_scan_log）===
//
// 作用域唯一：daemon startup catch-up 的现存 JSONL 全扫（CollectRequest.ScanExistingJSONL）。
// 其余入口（ChangedFile / CLI Dates / --force / retry / collect all）不注入门，恒全读。
//
// 门命中 = 门记录存在 + parser_version 匹配 + 读前快照三元组（file_identity、
// mtime_ns、file_size）与门记录完全一致 + 快照 identity 有效。命中则跳过该文件的
// 全部读取；任何不一致都倒向全文件重读（绝不「只读新增」或弃区间）。

// scanGateSupported 报告 client 是否参与 startup 跳过门。
// WorkBuddy/AutoClaw 暂不参与：其 provider 字段依赖 models.json 映射（用户可
// 编辑的外部文件），映射变化不改变 JSONL 文件证据——门命中会阻断「重读后
// provider 纠正」路径（messages upsert 的 provider=excluded.provider 仅在该文件
// 重新产出消息时生效）。待映射文件证据纳入门模型（映射指纹/版本）后开放。
func scanGateSupported(client string) bool {
	return client == "claude" || client == "codex"
}

// newScanGate 构造门命中判定（records 为开启采集前的门表预取快照，只读）。
func newScanGate(records map[string]model.FileScanLog) collector.FileSkipGate {
	return func(path string, before model.FileSnapshot) bool {
		rec, ok := records[path]
		if !ok {
			return false
		}
		if rec.ParserVersion != int64(db.ParserVersion) {
			// 解析器版本变化：旧记录整表失效（逐记录表现为永不命中），全部重读。
			return false
		}
		if !fsident.Valid(before) {
			// identity 不可用（获取失败/无效/平台不支持）：该文件禁用门，每次全读。
			return false
		}
		return rec.FileIdentity == before.Identity && rec.MtimeNS == before.MtimeNS && rec.FileSize == before.Size
	}
}

// scanGateRowsFor 从逐文件采集状态生成门记录：只对「本次实际读取、fullyParsed、
// 读前读后快照一致且 identity 有效」的文件产出记录（门推进条件的文件级实现）。
// 门命中跳过的文件（Skipped）不产出——其记录已存在且仍一致，无需重写。
func scanGateRowsFor(client string, statuses []collector.FileScanStatus) []model.FileScanLog {
	var rows []model.FileScanLog
	for _, st := range statuses {
		if !st.FullyParsed() {
			continue // 文件级错误 / 坏行 / 尾行未终结 / 被跳过
		}
		if st.Before != st.After {
			continue // 读取窗口内文件变化：结果照常落库，门不推进
		}
		if !fsident.Valid(st.Before) {
			continue // identity 不可用：不写门（该文件保持每次全读）
		}
		rows = append(rows, model.FileScanLog{
			Client:        client,
			FilePath:      st.Path,
			FileIdentity:  st.Before.Identity,
			MtimeNS:       st.Before.MtimeNS,
			FileSize:      st.Before.Size,
			ParserVersion: int64(db.ParserVersion),
		})
	}
	return rows
}

// countGateSkipped 统计被门命中跳过的文件数（Debug 心跳用）。
func countGateSkipped(statuses []collector.FileScanStatus) int {
	n := 0
	for _, st := range statuses {
		if st.Skipped {
			n++
		}
	}
	return n
}
