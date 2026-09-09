package collector

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// A/C 层标题来源：AutoClaw 客户端 Electron LocalStorage（Chromium LevelDB）的
// 会话显示名，与 sessions.json（gateway 侧索引）的 cron label。三者按
// A→C→B 降级链合成（见 resolveAutoClawTitle）；存储形态基于真实客户端
// 数据实测：会话列表整体存于键名后缀 localSessions.v1 的单条值内
// （JSON 数组），值首字节 \x00 为 UTF-16LE 编码、\x01 为 Latin-1。

const (
	// localStorageValueUTF16 / localStorageValueLatin1 是 Chromium LocalStorage 值的
	// 编码标志首字节（实测 \x00=UTF-16LE、\x01=Latin-1）。
	localStorageValueUTF16  = 0x00
	localStorageValueLatin1 = 0x01
	// localStorageKeyPrefix 只采集 agent 会话条目，滤掉其他 UI 状态键。
	localStorageKeyPrefix = "agent:"
	// localSessionsValueMarker 过滤 LocalStorage 值：仅处理包含该 JSON 键的数组，
	// 跳过客户端的其他 LocalStorage 内容。
	localSessionsValueMarker = `"displayName"`
	// autoClawSessionsKeySuffix 为会话列表数组的已知键名后缀（Chromium LocalStorage
	// 键形如 <origin>\x00\x01<键名>）。
	autoClawSessionsKeySuffix = "localSessions.v1"
	// autoClawSystemTitlePrefix 为客户端系统会话（evolution-check 等）的显示名前缀，
	// 属注入文本，不得作为标题。
	autoClawSystemTitlePrefix = "[SYSTEM:"
)

// localSessionsEntry 是 localSessions.v1 JSON 数组的单条会话条目。
type localSessionsEntry struct {
	Key         string `json:"key"`
	DisplayName string `json:"displayName"`
}

// autoClawSessionsIndexEntry 是 sessions.json 单条 entry 的关联字段。
type autoClawSessionsIndexEntry struct {
	key   string
	label string
}

// localStorageDir 返回 AutoClaw 客户端 LocalStorage LevelDB 目录的候选路径。
// Windows/Linux 路径形态未实测验证，探测失败由调用方整层降级（不阻塞采集）。
func localStorageDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "autoclaw", "Local Storage", "leveldb"), nil
}

// loadAutoClawClientTitles 只读加载 LocalStorage 中的会话显示名（A 层），
// 返回 key → displayName。目录不存在返回 fs.ErrNotExist（预期形态，Debug 级降级）；
// 打开失败（运行时文件锁等）与其他错误原样返回（调用方 Warn 一次后降级 C/B）。
// 只读模式必须显式 ReadOnly:true——缺省写模式会取排它锁并改动客户端目录。
func loadAutoClawClientTitles(dir string) (map[string]string, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fs.ErrNotExist
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	db, err := leveldb.OpenFile(dir, &opt.Options{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// 客户端会话列表存于键名后缀 localSessions.v1 的条目；优先精确匹配已知
	// 键名，防同库残留的其他含 displayName 数组（迁移残留/陈旧键）整体胜出；
	// 无已知键名时退回条目最多的候选（键结构漂移的兜底识别）。
	var known, fallbackBest map[string]string
	iter := db.NewIterator(nil, nil)
	defer iter.Release()
	for iter.Next() {
		entries := parseLocalStorageValue(iter.Value())
		if len(entries) == 0 {
			continue
		}
		next := make(map[string]string, len(entries))
		for _, e := range entries {
			next[e.Key] = e.DisplayName
		}
		if strings.HasSuffix(string(iter.Key()), autoClawSessionsKeySuffix) {
			if len(next) > len(known) {
				known = next
			}
			continue
		}
		if len(next) > len(fallbackBest) {
			fallbackBest = next
		}
	}
	if err := iter.Error(); err != nil {
		// 迭代中途错误（活跃写入库可能发生）：已解析条目仍可用（部分可用，
		// 不整层废弃）；该路径无法在单测可靠构造损坏 LevelDB 数据，语义由本
		// 返回约定保证。已知限制。
		if len(known) > 0 || len(fallbackBest) > 0 {
			return pickAutoClawTitleSet(known, fallbackBest), err
		}
	}
	return pickAutoClawTitleSet(known, fallbackBest), nil
}

// pickAutoClawTitleSet 选择条目集合：已知键名优先，否则退回兜底候选。
func pickAutoClawTitleSet(known, fallbackBest map[string]string) map[string]string {
	if len(known) > 0 {
		return known
	}
	return fallbackBest
}

// parseLocalStorageValue 解码单条 LocalStorage 值并抽取会话条目；
// 非 UTF-16LE/Latin-1 标志、非会话 JSON、解析失败均返回 nil（单条解码失败
// 不影响其余条目）。
func parseLocalStorageValue(val []byte) []localSessionsEntry {
	if len(val) < 1 {
		return nil
	}
	var text string
	switch val[0] {
	case localStorageValueUTF16:
		text = decodeUTF16LE(val[1:])
	case localStorageValueLatin1:
		// Latin-1 为单字节码位直映射，不能经 string() 透传（0x80-0xFF 会
		// 被 JSON 解析替换为 U+FFFD），须逐字节转码点。
		rs := make([]rune, len(val)-1)
		for i, c := range val[1:] {
			rs[i] = rune(c)
		}
		text = string(rs)
	default:
		return nil
	}
	if !strings.Contains(text, localSessionsValueMarker) {
		return nil
	}
	var entries []localSessionsEntry
	if err := json.Unmarshal([]byte(text), &entries); err != nil {
		return nil
	}
	out := entries[:0]
	for _, e := range entries {
		if !strings.HasPrefix(e.Key, localStorageKeyPrefix) {
			continue
		}
		if strings.TrimSpace(e.DisplayName) == "" || strings.HasPrefix(e.DisplayName, autoClawSystemTitlePrefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// decodeUTF16LE 解码 UTF-16LE 字节序列（奇数长度截断尾字节，代理对由 utf16.Decode 处理）。
func decodeUTF16LE(b []byte) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// loadAutoClawSessionIndex 读取 agents/{agentId}/sessions/sessions.json，
// 返回 sessionId → (key, label)（A 层反查桥接与 C 层 label 共用一次读取）。
// 文件缺失/损坏返回 nil（A、C 层整体降级，B 层不受影响）。
func loadAutoClawSessionIndex(path string) map[string]autoClawSessionsIndexEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var raw map[string]struct {
		SessionID string `json:"sessionId"`
		Label     string `json:"label"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	idx := make(map[string]autoClawSessionsIndexEntry, len(raw))
	// 实测 sessionId 在 entries 间唯一；若未来出现重复，按 key 字典序首个取值
	// 保证跨运行确定性（不依赖 map 遍历序）。
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		e := raw[key]
		if e.SessionID == "" {
			continue
		}
		if _, exists := idx[e.SessionID]; exists {
			continue
		}
		idx[e.SessionID] = autoClawSessionsIndexEntry{key: key, label: e.Label}
	}
	return idx
}

// loadClientTitles 加载 A 层标题；localTitleFn 非 nil 时走注入 seam（测试），
// 否则生产路径按平台探测 LocalStorage 目录。
func (c *AutoClawCollector) loadClientTitles() (map[string]string, error) {
	if c.localTitleFn != nil {
		return c.localTitleFn()
	}
	dir, err := localStorageDir()
	if err != nil {
		return nil, err
	}
	return loadAutoClawClientTitles(dir)
}

// resolveAutoClawTitle 按 A→C→B 层级合成会话标题：
// A=LocalStorage displayName（key 经 sessions.json 反查桥接）、C=sessions.json
// cron label（A 失效时的稳定备胎）、B=首条有效 user 消息派生（独立于客户端
// 状态的兜底，可能为空）。空 title 依赖 upsert 的空值不覆盖语义。
func (c *AutoClawCollector) resolveAutoClawTitle(derived, fileSessionID, agentID, sessionsDir string,
	localTitles map[string]string, indexCache map[string]map[string]autoClawSessionsIndexEntry) string {
	idx, cached := indexCache[agentID]
	if !cached {
		idx = loadAutoClawSessionIndex(filepath.Join(sessionsDir, agentID, "sessions", "sessions.json"))
		indexCache[agentID] = idx
	}
	if idx != nil {
		if e, ok := idx[fileSessionID]; ok {
			if dn := localTitles[e.key]; strings.TrimSpace(dn) != "" {
				return dn
			}
			if strings.TrimSpace(e.label) != "" {
				return e.label
			}
		}
	}
	return derived
}
