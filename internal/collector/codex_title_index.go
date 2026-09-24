package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// codexSessionIndexFile 是 Codex 标题索引文件名（App 的 thread 改名会追加写入
// 该文件，位于配置的 state_dir 下）。索引是本地缓存而非完整服务端清单：
// 同一 id 可重复出现（改名历史），按 updated_at 取最新；未命中须降级到
// state DB / rollout 原有标题，不得把「无记录」解释成「清空标题」。
const codexSessionIndexFile = "session_index.jsonl"

// CodexTitleIndexPath 返回 state_dir 下标题索引的绝对路径。
func CodexTitleIndexPath(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, codexSessionIndexFile)
}

// codexIndexLine 对应索引 JSONL 每行结构。
type codexIndexLine struct {
	ID         string `json:"id"`
	ThreadName string `json:"thread_name"`
	UpdatedAt  string `json:"updated_at"`
}

// ReadCodexTitleIndex 读取 state_dir 下的标题索引，返回 id → 最新有效记录
// （model.CodexTitleIndexRecord：标题原文 + updated_at UnixNano）。
//
// 行级校验：id 非空、thread_name 非空、updated_at 可按 RFC3339 解析；不满足的行
// 跳过并计数（含坏 JSON 与末尾正在写入的半行），不使整批失败。同一 id 的重复行
// 取 updated_at 最新者；时间相同取文件中后出现的有效记录（>= 判定）。取消请求
// 尊重 ctx。文件不存在返回 (nil, nil)：无索引属正常形态（老版本 Codex / 尚未
// 写入），调用方保留既有采集能力；存在但打开/读取失败返回错误供诊断。
// UpdatedAtUnixNano 随记录返回，供落库侧做索引重建旧快照的防倒退比较。
//
// 单行大小沿用 maxJSONLLineSize（与 rollout JSONL 同一上限，索引行远小于该值，
// 上限只防异常文件耗尽内存）。
//
// 读中变化的一致性：读前/读后各 stat 一次，指纹（size+mtime）不一致则重读一次
// （有界重试）；仍不一致则采用已读内容——索引按行合并取最新，读到部分新行只
// 影响本轮同步量，下一轮轮询幂等收敛，无需严格快照。
func ReadCodexTitleIndex(ctx context.Context, stateDir string, logger *slog.Logger) (map[string]model.CodexTitleIndexRecord, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	path := CodexTitleIndexPath(stateDir)
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("访问 Codex 标题索引失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Codex 标题索引不是普通文件: %s", path)
	}

	titles, badLines, firstBad, readErr := readCodexTitleIndexOnce(ctx, path)
	if readErr != nil {
		return nil, readErr
	}
	// codexIndexPostReadHook 仅供测试注入：在首遍读取后、读后指纹 stat 前执行，
	// 用于构造「读文件期间内容变化」的竞态窗口（生产恒为 nil）。
	if codexIndexPostReadHook != nil {
		codexIndexPostReadHook()
	}
	// 读中变化：指纹不一致时有界重读一次。
	after, statErr := os.Stat(path)
	if statErr == nil && !codexIndexFingerprintEqual(info, after) {
		if retry, rb, re, rerr := readCodexTitleIndexOnce(ctx, path); rerr == nil {
			titles, badLines, firstBad = retry, rb, re
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if badLines > 0 && logger != nil {
		logger.Warn("Codex title index has skipped lines",
			"path", path, "count", badLines, "first_line", firstBad.line, "error", firstBad.err)
	}
	return titles, nil
}

// codexIndexPostReadHook 见 ReadCodexTitleIndex 内注释（测试注入点，生产 nil）。
var codexIndexPostReadHook func()

// codexIndexFingerprintEqual 比较两次 stat 的 size+mtime 指纹。
func codexIndexFingerprintEqual(a, b os.FileInfo) bool {
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

type codexIndexBadLine struct {
	line int
	err  error
}

// readCodexTitleIndexOnce 单遍读取并按 updated_at 合并重复 id（同时间后行胜出）。
// 返回坏行计数与首坏行定位（有界告警）。
func readCodexTitleIndexOnce(ctx context.Context, path string) (map[string]model.CodexTitleIndexRecord, int, codexIndexBadLine, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, codexIndexBadLine{}, fmt.Errorf("打开 Codex 标题索引失败: %w", err)
	}
	defer file.Close()

	var (
		titles    = make(map[string]model.CodexTitleIndexRecord)
		updatedAt = make(map[string]time.Time)
		badLines  int
		firstBad  codexIndexBadLine
	)
	addBad := func(lineNo int, err error) {
		if badLines == 0 {
			firstBad = codexIndexBadLine{line: lineNo, err: err}
		}
		badLines++
	}

	it := newJSONLLineIter(ctx, file, maxJSONLLineSize)
	for it.Next() {
		if it.Oversized() {
			addBad(it.LineNo(), errJSONLLineOversized)
			continue
		}
		raw := it.Line()
		if len(raw) == 0 {
			continue
		}
		var line codexIndexLine
		if err := json.Unmarshal(raw, &line); err != nil {
			addBad(it.LineNo(), err)
			continue
		}
		if line.ID == "" || line.ThreadName == "" {
			addBad(it.LineNo(), errors.New("索引行缺少 id 或非空 thread_name"))
			continue
		}
		ts, err := time.Parse(time.RFC3339, line.UpdatedAt)
		if err != nil {
			addBad(it.LineNo(), fmt.Errorf("updated_at 不可解析: %w", err))
			continue
		}
		if prev, ok := updatedAt[line.ID]; !ok || !ts.Before(prev) {
			// 首见、更新或同时间：后出现的有效记录胜出。
			updatedAt[line.ID] = ts
			titles[line.ID] = model.CodexTitleIndexRecord{
				Name:              line.ThreadName,
				UpdatedAtUnixNano: ts.UnixNano(),
			}
		}
	}
	if err := it.Err(); err != nil {
		return nil, badLines, firstBad, fmt.Errorf("读取 Codex 标题索引失败: %w", err)
	}
	return titles, badLines, firstBad, nil
}
