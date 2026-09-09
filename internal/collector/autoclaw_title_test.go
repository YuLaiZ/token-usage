package collector

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/mattn/go-runewidth"
	"github.com/syndtr/goleveldb/leveldb"
)

// B 层派生清洗：按真实会话文件实测形态逐条定义期望。
func TestDeriveAutoClawSessionTitle(t *testing.T) {
	cases := []struct {
		name    string
		content string // JSON 字面量（str 或数组）
		want    string
	}{
		{"普通字符串消息原文保留", `"帮我看看今天A股行情"`, "帮我看看今天A股行情"},
		{"system-reminder 包装跳过", `"<system-reminder>\nAUTOCLAW_STABLE_POLICY_REMINDER\n一些协议内容"`, ""},
		{"[SYSTEM: 注入跳过", `"[SYSTEM: Post-turn evolution check — this message is auto-generated] 内容"`, ""},
		{"AUTHORED 成对包装取内文", `"<<<AUTOCLAW_USER_AUTHORED_REQUEST_START>>>\n云南交通旅游图怎么玩\n<<<AUTOCLAW_USER_AUTHORED_REQUEST_END>>>"`, "云南交通旅游图怎么玩"},
		{"AUTHORED 仅 START 无 END 跳过", `"<<<AUTOCLAW_USER_AUTHORED_REQUEST_START>>>只有开头"`, ""},
		{"cron 前缀剥离", `"[cron:c90a4373-257c-4c71-9f55-78d6625cd956 A股每日收盘分析] 请对今日A股市场做收盘分析"`, "请对今日A股市场做收盘分析"},
		{"cron 前缀后仅剩空白无效", `"[cron:c90a4373-257c-4c71-9f55-78d6625cd956 A股每日收盘分析]   "`, ""},
		{"换行与连续空白折叠为单空格", "\"第一行\\n\\n第二行   内容\"", "第一行 第二行 内容"},
		{"数组形态取首个 text block", `[{"type":"text","text":"数组里的首条消息"}]`, "数组里的首条消息"},
		{"数组无 text block 无效", `[{"type":"image","url":"x.png"}]`, ""},
		{"content 为 null 无效", `null`, ""},
		{"content 为数字无效", `42`, ""},
		{"content 为对象无效", `{"a":1}`, ""},
		{"纯空白无效", `"   "`, ""},
		{"空串无效", `""`, ""},
		{"text block 空文本无效", `[{"type":"text","text":""}]`, ""},
		{"前导空白不影响前缀判定", `"  <system-reminder>包装"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveAutoClawSessionTitle(json.RawMessage(tc.content))
			if got != tc.want {
				t.Errorf("deriveAutoClawSessionTitle(%s) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

// 截断为显示宽度口径：99/100/101 列 CJK 边界，断言截断后 StringWidth ≤ 100。
func TestDeriveAutoClawSessionTitleTruncateBoundary(t *testing.T) {
	// CJK 单字宽 2：49 字 = 98 列（未截断），50 字 = 100 列（恰好不截），51 字 = 102 列（截到 ≤100）。
	w49 := make([]rune, 49)
	w50 := make([]rune, 50)
	w51 := make([]rune, 51)
	for _, r := range [][]rune{w49, w50, w51} {
		for i := range r {
			r[i] = '评'
		}
	}
	cases := []struct {
		name string
		raw  string
	}{
		{"98 列不截断", string(w49)},
		{"100 列恰好不截", string(w50)},
		{"102 列截到不超过 100", string(w51)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveAutoClawSessionTitle(json.RawMessage(mustJSONString(t, tc.raw)))
			if w := runewidth.StringWidth(got); w > 100 {
				t.Errorf("StringWidth = %d, want <= 100", w)
			}
			if w := runewidth.StringWidth(got); w%2 != 0 {
				t.Errorf("StringWidth = %d, CJK 全角截断后应为偶数列（不能切出半列）", w)
			}
		})
	}
	// 51 字应恰好截为 50 字（100 列），防 rune 计数型实现（截 100 个 rune = 200 列）逃逸。
	got := deriveAutoClawSessionTitle(json.RawMessage(mustJSONString(t, string(w51))))
	if runewidth.StringWidth(got) != 100 || len([]rune(got)) != 50 {
		t.Errorf("boundary truncate: got %d runes / %d cols, want 50 runes / 100 cols", len([]rune(got)), runewidth.StringWidth(got))
	}
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// 端到端：B 层经 Collect 产出 Session.Title（user 行此前从未被解析）。
func TestAutoClawCollectDerivedTitle(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "agents", "main", "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sessionsDir, "sess-title-1.jsonl")
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "autoclaw", "title-authored-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Clients: map[string]config.Client{
		"autoclaw": {Enabled: true, Paths: map[string]string{"sessions_dir": filepath.Join(tmp, "agents")}},
	}}
	c := NewAutoClawCollector(cfg)
	// stub 掉 A 层：本测试聚焦 B 层，且不触碰开发机/CI 上的真实客户端库。
	c.localTitleFn = func() (map[string]string, error) {
		return nil, fs.ErrNotExist
	}
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(result.Sessions))
	}
	if got := result.Sessions[0].Title; got != "帮我梳理本地数据库结构" {
		t.Errorf("Title = %q, want 帮我梳理本地数据库结构 (首条有效 user 消息，跳过 system-reminder、剥离 AUTHORED 包装)", got)
	}
}

// ---- A/C 层：LocalStorage 与 sessions.json ----

// buildLocalLevelDB 用 goleveldb 建临时库写入 LocalStorage 形态条目（\x00=UTF-16LE / \x01=Latin-1）。
func buildLocalLevelDB(t *testing.T, vals map[string][]byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "leveldb")
	db, err := leveldb.OpenFile(dir, nil)
	if err != nil {
		t.Fatalf("open test leveldb: %v", err)
	}
	for k, v := range vals {
		if err := db.Put([]byte(k), v, nil); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dir
}

// utf16LEValue 按码位编码为 LocalStorage 形态（\x00 标志 + UTF-16LE）。
// 注意必须经 rune → uint16 转换，逐 UTF-8 字节膨胀会得到 mojibake。
func utf16LEValue(jsonText string) []byte {
	u := utf16.Encode([]rune(jsonText))
	out := make([]byte, 1, len(u)*2+1)
	out[0] = 0x00
	buf := make([]byte, 2)
	for _, v := range u {
		binary.LittleEndian.PutUint16(buf, v)
		out = append(out, buf...)
	}
	return out
}

func TestLoadAutoClawClientTitles(t *testing.T) {
	sessions := `[{"key":"agent:main:main","displayName":"AI技能盘点"},
{"key":"agent:main:1d34786e","displayName":"云南交通旅游图"},
{"key":"agent:main:evolution-check:msd21tkc-0","displayName":"[SYSTEM: Post-turn evolution check — auto]"},
{"key":"agent:main:blank","displayName":"   "},
{"key":"no-prefix-key","displayName":"不应采集"},
{"key":"agent:main:latin1","displayName":"Latin1标题"}]`

	// 漂移形态：有条目但无 displayName 键。
	drifted := `[{"key":"agent:main:drifted"}]`

	dir := buildLocalLevelDB(t, map[string][]byte{
		"_app://autoclaw\x00\x01autoclaw.localSessions.v1": utf16LEValue(sessions),
		"_app://autoclaw\x00\x01other.key":                 append([]byte{0x01}, []byte(`"plain"`)...),
		"_app://autoclaw\x00\x01drift.v1":                  append([]byte{0x00}, []byte(drifted)...),
	})
	titles, err := loadAutoClawClientTitles(dir)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if got := titles["agent:main:main"]; got != "AI技能盘点" {
		t.Errorf("titles[main] = %q, want AI技能盘点", got)
	}
	if got := titles["agent:main:1d34786e"]; got != "云南交通旅游图" {
		t.Errorf("titles[1d34786e] = %q, want 云南交通旅游图", got)
	}
	if got := titles["agent:main:latin1"]; got != "Latin1标题" {
		t.Errorf("titles[latin1] = %q, want Latin1标题 (known 值内条目)", got)
	}
	for _, key := range []string{"agent:main:evolution-check:msd21tkc-0", "agent:main:blank", "no-prefix-key", "agent:main:drifted"} {
		if got, ok := titles[key]; ok {
			t.Errorf("titles[%q] = %q, want filtered out", key, got)
		}
	}
	if len(titles) != 3 {
		t.Errorf("titles size = %d, want 3", len(titles))
	}

	// 目录不存在 → fs.ErrNotExist（调用方 Debug 降级）。
	if _, err := loadAutoClawClientTitles(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing dir error = %v, want fs.ErrNotExist", err)
	}

	// 空库（结构漂移极端形态）→ 空 map 无错误。
	emptyDir := buildLocalLevelDB(t, map[string][]byte{})
	if titles, err := loadAutoClawClientTitles(emptyDir); err != nil || len(titles) != 0 {
		t.Errorf("empty db: titles=%v err=%v, want empty/nil", titles, err)
	}
}

func TestLoadAutoClawSessionIndex(t *testing.T) {
	// 正常形态（testdata 夹具：A 命中 / 仅 label / 无 entry 三态 entry 并存）。
	withLabel := loadAutoClawSessionIndex(filepath.Join("..", "..", "testdata", "autoclaw", "title-sessions-with-label.json"))
	if withLabel == nil {
		t.Fatal("with-label index should not be nil")
	}
	e, ok := withLabel["sess-a"]
	if !ok || e.key != "agent:main:main" || e.label != "C 层标签A" {
		t.Errorf("entry for sess-a = %+v ok=%v", e, ok)
	}
	if e, ok := withLabel["sess-c"]; !ok || e.label != "Cron: 只在C层" {
		t.Errorf("entry for sess-c = %+v ok=%v (want cron label)", e, ok)
	}
	if _, ok := withLabel["sess-unrelated"]; !ok {
		t.Error("sess-unrelated entry should exist (无 label entry)")
	}

	// 仅 sessionId、无 label 的 entry（label 降级路径）。
	noLabel := loadAutoClawSessionIndex(filepath.Join("..", "..", "testdata", "autoclaw", "title-sessions-no-label.json"))
	if e, ok := noLabel["sess-nolabel"]; !ok || e.label != "" {
		t.Errorf("entry for sess-nolabel = %+v ok=%v (want present, empty label)", e, ok)
	}

	// 损坏 JSON → nil（A、C 层整体降级）。
	broken := loadAutoClawSessionIndex(filepath.Join("..", "..", "testdata", "autoclaw", "title-sessions-broken.json"))
	if broken != nil {
		t.Errorf("broken index = %v, want nil", broken)
	}

	// 文件缺失 → nil（A、C 层整体降级）。
	if idx := loadAutoClawSessionIndex(filepath.Join(t.TempDir(), "missing.json")); idx != nil {
		t.Errorf("missing file index = %v, want nil", idx)
	}
}

// 端到端层级合成：A → C → B → 空。
func TestAutoClawCollectTitleLayering(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "agents")
	sessionsPath := filepath.Join(sessionsDir, "main", "sessions")
	if err := os.MkdirAll(sessionsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// sessions.json 三态夹具自 testdata/autoclaw 复制：A 命中 / 仅 label / 无 entry。
	idxData, err := os.ReadFile(filepath.Join("..", "..", "testdata", "autoclaw", "title-sessions-with-label.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionsPath, "sessions.json"), idxData, 0o644); err != nil {
		t.Fatal(err)
	}
	// sess-reorder：assistant 行先于首条有效 user 行，验证文件级 Title 回填不丢派生值。
	reorder := `{"type":"session","version":3,"id":"sess-reorder","timestamp":"2026-08-27T03:21:36.693Z","cwd":"/tmp/aw"}
{"type":"message","id":"sess-reorder-a1","timestamp":"2026-08-27T03:21:36.939Z","message":{"role":"assistant","content":[{"type":"text","text":"好的"}],"usage":{"input_tokens":100,"output_tokens":20}}}
{"type":"message","id":"sess-reorder-u1","timestamp":"2026-08-27T03:21:37.000Z","message":{"role":"user","content":"<<<AUTOCLAW_USER_AUTHORED_REQUEST_START>>>\nreorder 派生标题\n<<<AUTOCLAW_USER_AUTHORED_REQUEST_END>>>"}}`
	if err := os.WriteFile(filepath.Join(sessionsPath, "sess-reorder.jsonl"), []byte(reorder), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"sess-a", "sess-c", "sess-b", "sess-empty"} {
		content := `{"type":"session","version":3,"id":"` + sid + `","timestamp":"2026-08-27T03:21:36.693Z","cwd":"/tmp/aw"}
{"type":"message","id":"` + sid + `-u1","timestamp":"2026-08-27T03:21:36.939Z","message":{"role":"user","content":"B层派生标题"}}
{"type":"message","id":"` + sid + `-a1","timestamp":"2026-08-27T03:21:52.968Z","message":{"role":"assistant","content":[{"type":"text","text":"好的"}],"usage":{"input_tokens":100,"output_tokens":20}}}
`
		if err := os.WriteFile(filepath.Join(sessionsPath, sid+".jsonl"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Clients: map[string]config.Client{
		"autoclaw": {Enabled: true, Paths: map[string]string{"sessions_dir": sessionsDir}},
	}}
	c := NewAutoClawCollector(cfg)
	c.localTitleFn = func() (map[string]string, error) {
		return map[string]string{"agent:main:main": "A层客户端标题"}, nil
	}

	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	got := map[string]string{}
	for _, s := range result.Sessions {
		got[s.ID] = s.Title
	}
	want := map[string]string{
		"sess-a":       "A层客户端标题",      // A 命中，即使 C/B 存在也不覆盖
		"sess-c":       "Cron: 只在C层",   // A 缺 → C 的 label
		"sess-b":       "B层派生标题",       // A/C 缺 → B 派生
		"sess-empty":   "B层派生标题",       // 无 entry → B
		"sess-reorder": "reorder 派生标题", // assistant 行先于 user 行，文件级回填不丢
	}
	for sid, wantTitle := range want {
		if got[sid] != wantTitle {
			t.Errorf("title[%s] = %q, want %q", sid, got[sid], wantTitle)
		}
	}

	// A 层读取失败 → 整层降级（sess-a 落到 C 层标签A）。
	c2 := NewAutoClawCollector(cfg)
	c2.localTitleFn = func() (map[string]string, error) {
		return nil, errors.New("leveldb locked")
	}
	result2, err := c2.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect(2) failed: %v", err)
	}
	for _, s := range result2.Sessions {
		if s.ID == "sess-a" && s.Title != "C 层标签A" {
			t.Errorf("degraded title[sess-a] = %q, want C 层标签A", s.Title)
		}
	}

	// 全层失效（sessions.json 读失败 + localTitleFn 失败）→ B 层仍兜底；
	// 全包装行会话 + 全层失效 → Title 为空。
	c3 := NewAutoClawCollector(cfg)
	c3.localTitleFn = func() (map[string]string, error) {
		return nil, errors.New("leveldb locked")
	}
	// 让 sessions.json 读失败：替换为目录使 ReadFile 失败。
	if err := os.Remove(filepath.Join(sessionsPath, "sessions.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sessionsPath, "sessions.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	emptyWrapped := `{"type":"session","version":3,"id":"sess-wrapped","timestamp":"2026-08-27T03:21:36.693Z","cwd":"/tmp/aw"}
{"type":"message","id":"sess-wrapped-u1","timestamp":"2026-08-27T03:21:36.939Z","message":{"role":"user","content":"<system-reminder>包装</system-reminder>"}}
{"type":"message","id":"sess-wrapped-a1","timestamp":"2026-08-27T03:21:52.968Z","message":{"role":"assistant","content":[{"type":"text","text":"好的"}],"usage":{"input_tokens":100,"output_tokens":20}}}
`
	if err := os.WriteFile(filepath.Join(sessionsPath, "sess-wrapped.jsonl"), []byte(emptyWrapped), 0o644); err != nil {
		t.Fatal(err)
	}
	result3, err := c3.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect(3) failed: %v", err)
	}
	for _, s := range result3.Sessions {
		// B 层兜底：各会话派生标题不同，只断言非空（reorder=派生标题、其余=B层派生标题）；
		// 全包装行会话（sess-wrapped）允许为空，由下方独立断言。
		if s.ID != "sess-wrapped" && s.Title == "" {
			t.Errorf("fallback title[%s] = empty, want non-empty (B 层兜底)", s.ID)
		}
	}
	for _, s := range result3.Sessions {
		if s.ID == "sess-wrapped" && s.Title != "" {
			t.Errorf("wrapped-only session title = %q, want empty", s.Title)
		}
	}
}

// Latin-1 正面路径：displayName 含 0x80-0xFF 单字节时须逐字节转码点解码
// （UTF-8 透传实现会产出 U+FFFD，本用例可杀死该错误实现）。
func TestLoadAutoClawClientTitlesLatin1(t *testing.T) {
	// displayName 中嵌入 Latin-1 的 0xE9（é）：raw string 不解析转义，故逐段拼接。
	latin := append([]byte{0x01}, []byte(`[{"key":"agent:main:latin1only","displayName":"Caf`)...)
	latin = append(latin, 0xE9)
	latin = append(latin, []byte(` menu"}]`)...)
	dir := buildLocalLevelDB(t, map[string][]byte{
		"_app://autoclaw\x00\x01localSessions.v1": latin,
	})
	titles, err := loadAutoClawClientTitles(dir)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if got := titles["agent:main:latin1only"]; got != "Caf\u00e9 menu" {
		t.Errorf("titles[latin1only] = %q, want Caf\u00e9 menu", got)
	}
}
