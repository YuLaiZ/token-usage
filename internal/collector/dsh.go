package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/fsident"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// DSHCollector 采集 DeepSeek Harness（DSH）的会话日志。
//
// 数据源是 ~/.dsh/sessions：每个会话一份只追加的 JSONL（默认 zstd 拼接帧），
// 首行 header 带 id/cwd/parentSession/isSeeded，逐条 assistant/message 自带
// provider/model 与四桶 token。采集器只读，不依赖网络、密钥或 DSH 运行。
type DSHCollector struct {
	cfg   *config.Config
	cache *dshCache
}

// NewDSHCollector 基于全局配置构造采集器。
func NewDSHCollector(cfg *config.Config) *DSHCollector {
	return &DSHCollector{cfg: cfg, cache: newDSHCache()}
}

// Name 返回配置 key，engine 用它匹配 [clients.dsh] 与 router 配置。
func (c *DSHCollector) Name() string { return "dsh" }

// SyncSources 返回空：DSH 是 JSONL 型采集器，没有 SQLite 增量游标。
func (c *DSHCollector) SyncSources() []string { return nil }

// Collect 扫描会话根目录，解码候选会话并产出 Session 元数据。
func (c *DSHCollector) Collect(ctx context.Context, req CollectRequest, logger *slog.Logger) (CollectResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CollectResult{}, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	if req.ChangedFile != "" {
		logger.Debug("dsh ignores ChangedFile; scanning all sessions", "file", req.ChangedFile)
	}

	root := c.sessionsRoot()
	if root == "" {
		return CollectResult{}, nil
	}

	discovery := discoverDSHSessions(root)
	for _, warning := range discovery.Warnings {
		logger.Warn("dsh discovery warning", "warning", warning)
	}

	var result CollectResult
	var errs []error
	seen := make(map[string]string)
	for _, message := range discovery.Errors {
		errs = append(errs, errors.New(message))
	}

	for _, candidate := range discovery.Sessions {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		before := fsident.SnapshotOfFile(candidate.Path)
		if entry, ok := c.cache.get(candidate.Path, before); ok {
			if duplicate, err := dshCheckDuplicate(seen, candidate.Path, entry.sessions); err != nil {
				errs = append(errs, err)
				continue
			} else if duplicate {
				continue
			}
			result.Messages = append(result.Messages, toModelMessages(filterDSHMessages(entry.messages, req.Dates))...)
			result.Sessions = append(result.Sessions, entry.sessions...)
			result.DSHObsolete = append(result.DSHObsolete, toDSHObsolete(filterDSHObsolete(entry.obsolete, req.Dates))...)
			result.FileStatuses = append(result.FileStatuses, entry.statuses...)
			continue
		}

		decoded, err := decodeDSHSession(ctx, candidate.Path, candidate.Compression)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", candidate.Path, err))
			result.FileStatuses = append(result.FileStatuses, FileScanStatus{Path: candidate.Path, Err: err})
			continue
		}
		after := fsident.SnapshotOfFile(candidate.Path)
		status := FileScanStatus{
			Path:            candidate.Path,
			Before:          before,
			After:           after,
			BadLines:        decoded.BadLines,
			OversizedLines:  decoded.Oversized,
			TruncatedTail:   decoded.Truncated,
			TrailingNewline: decoded.TrailingNewline,
		}
		result.FileStatuses = append(result.FileStatuses, status)
		if len(decoded.Events) == 0 {
			// 空文件或首帧未写完：没有 header 可校验，也不算错误。
			continue
		}
		if decoded.Header.Version != candidate.Version {
			errs = append(errs, fmt.Errorf(
				"%s: header version %d != filename version %d",
				candidate.Path, decoded.Header.Version, candidate.Version))
			continue
		}
		if decoded.Header.Version != 4 {
			logger.Warn("skip unsupported dsh session format",
				"path", candidate.Path, "version", decoded.Header.Version)
			continue
		}
		if decoded.AllInherited {
			logger.Warn("skip fully inherited dsh session", "path", candidate.Path)
			continue
		}
		parsed := buildDSHResult(decoded)
		if duplicate, err := dshCheckDuplicate(seen, candidate.Path, parsed.Sessions); err != nil {
			errs = append(errs, err)
			continue
		} else if duplicate {
			continue
		}
		if before == after && fsident.Valid(before) && status.FullyParsed() {
			c.cache.put(candidate.Path, before, dshCacheEntry{
				snapshot: before,
				messages: parsed.Messages,
				sessions: parsed.Sessions,
				obsolete: parsed.Obsolete,
				statuses: []FileScanStatus{status},
			})
		}
		result.Messages = append(result.Messages, toModelMessages(filterDSHMessages(parsed.Messages, req.Dates))...)
		result.Sessions = append(result.Sessions, parsed.Sessions...)
		result.DSHObsolete = append(result.DSHObsolete, toDSHObsolete(filterDSHObsolete(parsed.Obsolete, req.Dates))...)
	}

	if len(errs) > 0 {
		result.PartialErr = errors.Join(errs...)
	}
	return result, nil
}

// sessionsRoot 从 [clients.dsh].paths.sessions_dir 解析会话根；未配置时返回空。
func (c *DSHCollector) sessionsRoot() string {
	if c == nil || c.cfg == nil {
		return ""
	}
	client, ok := c.cfg.ClientConfig("dsh")
	if !ok || client == nil {
		return ""
	}
	return dshExpandHome(strings.TrimSpace(client.Paths["sessions_dir"]))
}

func dshExpandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// dshCheckDuplicate 检查本轮候选里是否出现重复 session id；缓存命中同样参与。
func dshCheckDuplicate(seen map[string]string, path string, sessions []model.Session) (bool, error) {
	if len(sessions) == 0 {
		return false, nil
	}
	id := sessions[0].ID
	if prev, ok := seen[id]; ok {
		return true, fmt.Errorf("%s: duplicate session id %q (already at %s)", path, id, prev)
	}
	seen[id] = path
	return false, nil
}
