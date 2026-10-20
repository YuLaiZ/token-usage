package collector

import (
	"sync"

	"github.com/YuLaiZ/token-usage/internal/fsident"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// dshCacheEntry 是一次完整解析的结果，按文件指纹缓存。
type dshCacheEntry struct {
	snapshot model.FileSnapshot
	messages []parsedMessage
	sessions []model.Session
	obsolete []parsedObsolete
	statuses []FileScanStatus
}

// dshCache 是进程内解析结果缓存：命中时仍返回结果，事务失败可由下一轮重试。
// 缓存不表达「已提交」，只表达「该文件内容与上次解析时一致」。
type dshCache struct {
	mu      sync.Mutex
	entries map[string]dshCacheEntry
}

func newDSHCache() *dshCache {
	return &dshCache{entries: make(map[string]dshCacheEntry)}
}

func (c *dshCache) get(path string, snapshot model.FileSnapshot) (dshCacheEntry, bool) {
	if c == nil || !fsident.Valid(snapshot) {
		return dshCacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[path]
	if !ok || entry.snapshot != snapshot {
		return dshCacheEntry{}, false
	}
	return entry, true
}

func (c *dshCache) put(path string, snapshot model.FileSnapshot, entry dshCacheEntry) {
	if c == nil || !fsident.Valid(snapshot) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[path] = entry
}

// filterDSHMessages 按请求日期裁剪消息；dates 为空表示全量。
// 返回新切片，绝不原地修改缓存中的切片。
func filterDSHMessages(messages []parsedMessage, dates []string) []parsedMessage {
	if len(dates) == 0 {
		return messages
	}
	allowed := make(map[string]struct{}, len(dates))
	for _, date := range dates {
		allowed[date] = struct{}{}
	}
	out := make([]parsedMessage, 0, len(messages))
	for _, message := range messages {
		if _, ok := allowed[tsMsToDate(message.FirstSeenTS)]; ok {
			out = append(out, message)
		}
	}
	return out
}

// filterDSHObsolete 按请求日期裁剪撤销候选；dates 为空表示全量。
func filterDSHObsolete(obsolete []parsedObsolete, dates []string) []parsedObsolete {
	if len(dates) == 0 {
		return obsolete
	}
	allowed := make(map[string]struct{}, len(dates))
	for _, date := range dates {
		allowed[date] = struct{}{}
	}
	out := make([]parsedObsolete, 0, len(obsolete))
	for _, item := range obsolete {
		if _, ok := allowed[tsMsToDate(item.TS)]; ok {
			out = append(out, item)
		}
	}
	return out
}
