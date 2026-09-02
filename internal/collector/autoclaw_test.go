package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// autoclawFixtureRoot 返回 testdata/autoclaw/agents 绝对路径（不假设工作目录是仓库根）。
func autoclawFixtureRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "testdata", "autoclaw", "agents"))
	if err != nil {
		t.Fatalf("解析 fixture 路径失败: %v", err)
	}
	return abs
}

// newAutoClawCfg 构造含 autoclaw enabled + sessions_dir 指向给定根的 cfg。
func newAutoClawCfg(t *testing.T, sessionsDir string) *config.Config {
	t.Helper()
	return &config.Config{
		Clients: map[string]config.Client{
			"autoclaw": {Enabled: true, Paths: map[string]string{"sessions_dir": sessionsDir}},
		},
	}
}

// acTS 用 UTC 正午生成毫秒时间戳，确保任何时区都落在同一日期。
func acTS(year, month, day int) int64 {
	return time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.UTC).UnixMilli()
}

// findAcMsg 在结果中按 ID 定位消息。
func findAcMsg(t *testing.T, msgs []model.Message, id string) model.Message {
	t.Helper()
	for _, m := range msgs {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("未找到 id=%q 的消息，现有 ids: %v", id, acMsgIDs(msgs))
	return model.Message{}
}

func acMsgIDs(msgs []model.Message) []string {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

// acParsedMsgIDs 提取解析层消息 ID 列表（autoclawParsedMessage），便于错误信息可读。
func acParsedMsgIDs(msgs []autoclawParsedMessage) []string {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

// writeAcFile 在 tmp 下构造 agents/{agent}/sessions/{sessionID}.jsonl 并写入内容，返回 sessions_dir 根（agents/）。
func writeAcFile(t *testing.T, tmp, agent, sessionID, content string) string {
	t.Helper()
	dir := filepath.Join(tmp, "agents", agent, "sessions")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(content), 0644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	return filepath.Join(tmp, "agents")
}

// acUsageLine 生成一条带 usage 的 assistant JSONL 行（顶层 id，无嵌套 message.id）。
func acUsageLine(id string, tsMs int64, provider, modelName string, in, out, cacheRead, cacheWrite, reasoning, total int64) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"timestamp":"2026-07-29T12:00:00.000Z","message":{"role":"assistant","api":"openai-completions","provider":%q,"model":%q,"usage":{"input":%d,"output":%d,"cacheRead":%d,"cacheWrite":%d,"reasoningTokens":%d,"totalTokens":%d,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"endTurn","timestamp":%d,"responseId":"resp-%s"}}`,
		id, provider, modelName, in, out, cacheRead, cacheWrite, reasoning, total, tsMs, id)
}

// ---- 用例 1：正常解析 ----

func TestAutoClaw_ParsesAssistantUsageMessages(t *testing.T) {
	cfg := newAutoClawCfg(t, autoclawFixtureRoot(t))
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}

	// 定位 main 的已知消息 aaa-m2（provider=zai, zai_auto）
	m2 := findAcMsg(t, result.Messages, "aaa-m2")
	if m2.SessionID != "session-aaa" {
		t.Errorf("SessionID = %q, want session-aaa（文件名去后缀）", m2.SessionID)
	}
	if m2.Client != model.ClientZhipuAutoClaw {
		t.Errorf("Client = %q, want %q（显示名常量，非 raw）", m2.Client, model.ClientZhipuAutoClaw)
	}
	if m2.ID != "aaa-m2" {
		t.Errorf("ID = %q, want aaa-m2（行顶层 id，非嵌套 message.id）", m2.ID)
	}
	if m2.InputTokens != 40507 {
		t.Errorf("InputTokens = %d, want 40507", m2.InputTokens)
	}
	if m2.OutputTokens != 155 {
		t.Errorf("OutputTokens = %d, want 155", m2.OutputTokens)
	}
	if m2.CacheReadTokens != 1536 {
		t.Errorf("CacheReadTokens = %d, want 1536", m2.CacheReadTokens)
	}
	if m2.CacheCreateTokens != 0 {
		t.Errorf("CacheCreateTokens = %d, want 0", m2.CacheCreateTokens)
	}
	if m2.ReasoningTokens != 94 {
		t.Errorf("ReasoningTokens = %d, want 94", m2.ReasoningTokens)
	}
	if m2.Model != "zai_auto" {
		t.Errorf("Model = %q, want zai_auto", m2.Model)
	}
	if m2.Provider != "智谱主实例" {
		t.Errorf("Provider = %q, want 智谱主实例（models.json name）", m2.Provider)
	}
	if m2.Directory != "/Users/test/openclaw/workspace" {
		t.Errorf("Directory = %q, want session 行的 cwd", m2.Directory)
	}
	if m2.Project != "workspace" {
		t.Errorf("Project = %q, want workspace", m2.Project)
	}
	// aaa-m4 用 main 的第二个 provider key（uuid 形式 zai__...），命中不同 name "智谱开放平台"。
	// 覆盖同 agent 多 provider key 场景（与 aaa-m2 的 zai→智谱主实例 区分）。
	m4 := findAcMsg(t, result.Messages, "aaa-m4")
	if m4.Provider != "智谱开放平台" {
		t.Errorf("aaa-m4 Provider = %q, want 智谱开放平台（main 第二个 provider key 的 name）", m4.Provider)
	}
	if m4.Model != "glm-5.2" {
		t.Errorf("aaa-m4 Model = %q, want glm-5.2", m4.Model)
	}
	// Session.Client 也写显示名
	var sessAAA *model.Session
	for i := range result.Sessions {
		if result.Sessions[i].ID == "session-aaa" {
			sessAAA = &result.Sessions[i]
		}
	}
	if sessAAA == nil {
		t.Fatalf("未找到 session-aaa")
	}
	if sessAAA.Client != model.ClientZhipuAutoClaw {
		t.Errorf("Session.Client = %q, want %q", sessAAA.Client, model.ClientZhipuAutoClaw)
	}
}

// ---- 用例 2 + 2b：usage 口径铁证 + TotalTokens 回退含 cacheRead ----

func TestAutoClaw_UsageSemantics_FreshInputNotSubtracted_TotalFromSource(t *testing.T) {
	cfg := newAutoClawCfg(t, autoclawFixtureRoot(t))
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}

	m2 := findAcMsg(t, result.Messages, "aaa-m2")
	// FreshInput == input（直接赋值，不扣减）
	if m2.FreshInputTokens != 40507 {
		t.Errorf("FreshInputTokens = %d, want 40507（直接赋值 input，非 SubtractCache）", m2.FreshInputTokens)
	}
	// TotalTokens == JSON 原值（不重算）
	if m2.TotalTokens != 42198 {
		t.Errorf("TotalTokens = %d, want 42198（JSON 原值）", m2.TotalTokens)
	}
	// 正常数据下 totalTokens == input + output + cacheRead（校验 fixture 正确性）
	if m2.TotalTokens != m2.InputTokens+m2.OutputTokens+m2.CacheReadTokens {
		t.Errorf("TotalTokens %d != input+output+cacheRead %d（fixture 校验）",
			m2.TotalTokens, m2.InputTokens+m2.OutputTokens+m2.CacheReadTokens)
	}
}

func TestAutoClaw_TotalTokensFallback_IncludesCacheRead(t *testing.T) {
	cfg := newAutoClawCfg(t, autoclawFixtureRoot(t))
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}

	// aaa-totalzero: totalTokens 字段缺失（JSON total:0 但无 totalTokens key）, cacheRead=200
	mz := findAcMsg(t, result.Messages, "aaa-totalzero")
	want := int64(500 + 100 + 200) // input + output + cacheRead（含 cacheRead）
	if mz.TotalTokens != want {
		t.Errorf("TotalTokens 回退 = %d, want %d（含 cacheRead，不是 input+output=%d）",
			mz.TotalTokens, want, mz.InputTokens+mz.OutputTokens)
	}
}

// ---- 用例 3：trajectory 双路径拒绝 ----

func TestAutoClaw_RejectsTrajectory_ScanAndChangedFile(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)

	// (a) 扫描路径不含 trajectory 的消息（traj-m1 不应出现）
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	for _, m := range result.Messages {
		if m.ID == "traj-m1" {
			t.Errorf("trajectory 文件的 traj-m1 不应出现在扫描结果中（双计）")
		}
	}

	// (b) isAutoClawValidChangedFile 精确验证
	trajPath := filepath.Join(root, "main", "sessions", "session-ccc.trajectory.jsonl")
	if isAutoClawValidChangedFile(trajPath, root) {
		t.Errorf("isAutoClawValidChangedFile 应拒绝 trajectory 路径")
	}

	// (c) Collect(ChangedFile=trajectory) 无消息、无 Session、无 PartialErr
	r2, err2 := c.Collect(context.Background(), CollectRequest{ChangedFile: trajPath}, slog.Default())
	if err2 != nil {
		t.Fatalf("Collect(trajectory) 失败: %v", err2)
	}
	if len(r2.Messages) != 0 || len(r2.Sessions) != 0 || r2.PartialErr != nil {
		t.Errorf("Collect(trajectory) 应返回空结果无 PartialErr, got msgs=%d sess=%d partial=%v",
			len(r2.Messages), len(r2.Sessions), r2.PartialErr)
	}
}

// ---- 用例 4：扫描路径拒绝非 sessions 子目录 ----

func TestAutoClaw_RejectsNonSessionsDir_ScanAndChangedFile(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)

	strayPath := filepath.Join(root, "main", "logs", "stray.jsonl")

	// (a) isAutoClawValidChangedFile 验证
	if isAutoClawValidChangedFile(strayPath, root) {
		t.Errorf("isAutoClawValidChangedFile 应拒绝非 sessions 子目录")
	}

	// (b) 全量扫描不含 stray 的消息（stray-m1 不应出现）
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	for _, m := range result.Messages {
		if m.ID == "stray-m1" {
			t.Errorf("logs/stray.jsonl 的 stray-m1 不应出现在扫描结果中")
		}
	}

	// (c) Collect(ChangedFile=stray) 无消息、无 Session、无 PartialErr
	r2, err2 := c.Collect(context.Background(), CollectRequest{ChangedFile: strayPath}, slog.Default())
	if err2 != nil {
		t.Fatalf("Collect(stray) 失败: %v", err2)
	}
	if len(r2.Messages) != 0 || len(r2.Sessions) != 0 || r2.PartialErr != nil {
		t.Errorf("Collect(stray) 应返回空结果无 PartialErr, got msgs=%d sess=%d partial=%v",
			len(r2.Messages), len(r2.Sessions), r2.PartialErr)
	}
}

// ---- 用例 5：ChangedFile 路径格式校验（拒绝） ----

func TestAutoClaw_ChangedFile_RejectsInvalidPaths(t *testing.T) {
	root := autoclawFixtureRoot(t)

	// 空 sessionId 需要实际写入可解析文件；用独立 t.TempDir 构造三层路径，
	// 不向版本化 testdata 写临时文件（避免 panic/fatal 时残留脏文件污染仓库）。
	emptyRoot := filepath.Join(t.TempDir(), "agents")
	emptySessionsDir := filepath.Join(emptyRoot, "main", "sessions")
	os.MkdirAll(emptySessionsDir, 0755)
	emptyIDPath := filepath.Join(emptySessionsDir, ".jsonl")
	if err := os.WriteFile(emptyIDPath,
		[]byte(acUsageLine("empty-sid-msg", acTS(2026, 7, 29), "zai", "zai_auto", 10, 5, 0, 0, 0, 15)+"\n"), 0644); err != nil {
		t.Fatalf("写入 .jsonl 失败: %v", err)
	}
	// sessions_dir 外用例：用 t.TempDir 派生路径，不依赖全局 os.TempDir
	outsideTmp := t.TempDir()

	tests := []struct {
		name        string
		changed     string
		sessionsDir string
	}{
		{"缺 agentId", filepath.Join(root, "sessions", "x.jsonl"), root},
		{"四层", filepath.Join(root, "main", "sub", "sessions", "x.jsonl"), root},
		{"非 sessions", filepath.Join(root, "main", "logs", "x.jsonl"), root},
		{"sessions_dir 外", filepath.Join(outsideTmp, "other", "agent", "sessions", "x.jsonl"), root},
		{"空 sessionId", emptyIDPath, emptyRoot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// (a) helper 返回 false
			if isAutoClawValidChangedFile(tt.changed, tt.sessionsDir) {
				t.Errorf("isAutoClawValidChangedFile 应拒绝 %s", tt.name)
			}
			// (b) Collect 返回空结果无 PartialErr
			cfg := newAutoClawCfg(t, tt.sessionsDir)
			c := NewAutoClawCollector(cfg)
			r, err := c.Collect(context.Background(), CollectRequest{ChangedFile: tt.changed}, slog.Default())
			if err != nil {
				t.Fatalf("Collect 失败: %v", err)
			}
			if len(r.Messages) != 0 || len(r.Sessions) != 0 || r.PartialErr != nil {
				t.Errorf("%s: 应返回空结果无 PartialErr, got msgs=%d sess=%d partial=%v",
					tt.name, len(r.Messages), len(r.Sessions), r.PartialErr)
			}
		})
	}

	// 全量扫描不产出空 SessionID：对 fixture root 和含 .jsonl 的 emptyRoot 都验证。
	for _, sd := range []string{root, emptyRoot} {
		cfg := newAutoClawCfg(t, sd)
		c := NewAutoClawCollector(cfg)
		result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
		if err != nil {
			t.Fatalf("Collect 失败: %v", err)
		}
		for _, m := range result.Messages {
			if m.SessionID == "" {
				t.Errorf("全量扫描不应产出空 SessionID（.jsonl 去后缀为空）: %+v", m)
			}
		}
	}
}

// ---- 用例 6：ChangedFile 路径格式校验（接受，覆盖规范化契约） ----

func TestAutoClaw_ChangedFile_AcceptsNormalizedPaths(t *testing.T) {
	root := autoclawFixtureRoot(t)
	sep := string(os.PathSeparator)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)

	// 使用相对 sessions_dir 测相对路径（root 须是绝对路径已满足）
	// (1) 规范绝对路径
	abs := filepath.Join(root, "main", "sessions", "session-aaa.jsonl")
	if !isAutoClawValidChangedFile(abs, root) {
		t.Errorf("规范绝对路径应被接受")
	}
	r1, err := c.Collect(context.Background(), CollectRequest{ChangedFile: abs}, slog.Default())
	if err != nil {
		t.Fatalf("Collect(abs) 失败: %v", err)
	}
	if len(r1.Messages) == 0 {
		t.Errorf("规范绝对路径应产出消息")
	}

	// (2) 含冗余 . 段（手工拼接保留未规范化片段）
	dotPath := root + sep + "main" + sep + "." + sep + "sessions" + sep + "session-aaa.jsonl"
	if filepath.Clean(dotPath) != abs {
		t.Fatalf("测试前置：Clean(dotPath) 应 == abs, got %s vs %s", filepath.Clean(dotPath), abs)
	}
	if !isAutoClawValidChangedFile(dotPath, root) {
		t.Errorf("含 '.' 段的路径（Clean 后合法）应被接受")
	}
	r2, err := c.Collect(context.Background(), CollectRequest{ChangedFile: dotPath}, slog.Default())
	if err != nil {
		t.Fatalf("Collect(dotPath) 失败: %v", err)
	}
	if len(r2.Messages) == 0 {
		t.Errorf("含 '.' 段的路径应产出消息")
	}

	// (3) 含 .. 抵消段（main/sub/../sessions）
	dotdotPath := root + sep + "main" + sep + "sub" + sep + ".." + sep + "sessions" + sep + "session-aaa.jsonl"
	if filepath.Clean(dotdotPath) != abs {
		t.Fatalf("测试前置：Clean(dotdotPath) 应 == abs, got %s vs %s", filepath.Clean(dotdotPath), abs)
	}
	if !isAutoClawValidChangedFile(dotdotPath, root) {
		t.Errorf("含 '..' 抵消段的路径（Clean 后合法）应被接受")
	}

	// (4) 相对路径（配合相对 sessions_dir）
	// 用 t.TempDir 作为相对基准不现实；改为验证 isAutoClawValidChangedFile 接受相对 changedFile + 相对 sessionsDir 的对称场景
	// 在 root 内用相对路径
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd 失败: %v", err)
	}
	relRoot, err := filepath.Rel(wd, root)
	if err != nil {
		t.Fatalf("Rel 失败: %v", err)
	}
	relChanged := filepath.Join(relRoot, "main", "sessions", "session-aaa.jsonl")
	if !isAutoClawValidChangedFile(relChanged, relRoot) {
		t.Errorf("相对路径（配合相对 sessions_dir）应被接受")
	}
}

// ---- 用例 7：role 过滤 ----

func TestAutoClaw_FiltersByRoleAndUsage(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	// aaa-m1 是 user 行，不应产出
	for _, m := range result.Messages {
		if m.ID == "aaa-m1" {
			t.Errorf("user 行 aaa-m1 不应产出 Message")
		}
	}
	// aaa-nousage 是 assistant 但无 usage，不应产出
	for _, m := range result.Messages {
		if m.ID == "aaa-nousage" {
			t.Errorf("无 usage 的 assistant 行 aaa-nousage 不应产出 Message")
		}
	}
}

// ---- 用例 8：顶层 id 去重 + 空 id 跳过 ----

func TestAutoClaw_DeduplicatesByTopLevelID_SkipsEmptyID(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	// aaa-m2 重复行只保留首条（input=40507，不是 99999）
	count := 0
	for _, m := range result.Messages {
		if m.ID == "aaa-m2" {
			count++
			if m.InputTokens != 40507 {
				t.Errorf("重复 id 应保留首条 input=40507, got %d", m.InputTokens)
			}
		}
	}
	if count != 1 {
		t.Errorf("重复顶层 id aaa-m2 应只出现 1 次, got %d", count)
	}
	// 空 id 行不应产出（空 id 行 input=100，确认无 input=100 的消息泄露）。
	// 不叠加 m.ID == "" 条件——parser 已跳过空 id 行，若该跳过逻辑被破坏，
	// 泄露的消息 ID 仍为 ""，但只按 input 值断言能无论 ID 是否为空都抓到回归。
	// 全量空 ID 防护由 TestAutoClaw_NoMessageWithEmptyID 承担。
	for _, m := range result.Messages {
		if m.InputTokens == 100 {
			t.Errorf("空顶层 id 的行不应产出 Message（防空 id 写入 (client,\"\")）")
		}
	}
}

// ---- 用例 9：session 行取 cwd ----

func TestAutoClaw_SessionLineCwd(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	// main 的 session cwd = /Users/test/openclaw/workspace
	m2 := findAcMsg(t, result.Messages, "aaa-m2")
	if m2.Directory != "/Users/test/openclaw/workspace" {
		t.Errorf("Directory = %q, want session 行的 cwd", m2.Directory)
	}
}

// ---- 用例 10：timestamp 双格式 ----

func TestAutoClaw_TimestampDualFormat(t *testing.T) {
	tmp := t.TempDir()
	// message.timestamp 存在（毫秒） -> 用它
	// message.timestamp 缺失 -> 回退顶层 RFC3339
	content := acUsageLine("ts-ms", acTS(2026, 7, 29), "zai", "zai_auto", 10, 5, 0, 0, 0, 15) + "\n" +
		`{"type":"message","id":"ts-rfc","timestamp":"2026-07-29T12:00:00.000Z","message":{"role":"assistant","api":"openai-completions","provider":"zai","model":"zai_auto","usage":{"input":20,"output":10,"cacheRead":0,"cacheWrite":0,"reasoningTokens":0,"totalTokens":30,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"endTurn","responseId":"r"}}` + "\n"
	root := writeAcFile(t, tmp, "a1", "sess-ts", content)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	ms := findAcMsg(t, result.Messages, "ts-ms")
	if ms.TS != acTS(2026, 7, 29) {
		t.Errorf("ts-ms.TS = %d, want %d（message.timestamp 毫秒）", ms.TS, acTS(2026, 7, 29))
	}
	rf := findAcMsg(t, result.Messages, "ts-rfc")
	wantRFC := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC).UnixMilli()
	if rf.TS != wantRFC {
		t.Errorf("ts-rfc.TS = %d, want %d（顶层 RFC3339 回退）", rf.TS, wantRFC)
	}
}

// ---- 用例 11：脏数据处理 ----

func TestAutoClaw_DirtyDataHandling(t *testing.T) {
	tmp := t.TempDir()
	// 单行 JSON 无效 -> 跳过不阻断
	// message.timestamp=0 且顶层 RFC3339 无效 -> 跳过（不产 TS=0 脏消息）
	content := acUsageLine("good", acTS(2026, 7, 29), "zai", "zai_auto", 10, 5, 0, 0, 0, 15) + "\n" +
		"this is not json\n" +
		`{"type":"message","id":"badts","timestamp":"not-a-date","message":{"role":"assistant","provider":"zai","model":"zai_auto","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"reasoningTokens":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}}` + "\n"
	root := writeAcFile(t, tmp, "a1", "sess-dirty", content)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	// 只应有 good 一条（脏行跳过，时间戳无效跳过）
	ids := acMsgIDs(result.Messages)
	if len(result.Messages) != 1 || result.Messages[0].ID != "good" {
		t.Errorf("脏数据处理后应只剩 good, got %v", ids)
	}
	// 不应产生 TS=0 脏消息
	for _, m := range result.Messages {
		if m.TS == 0 {
			t.Errorf("不应产出 TS=0 的脏消息: %+v", m)
		}
	}
}

// ---- 用例 12：部分结果保留（scanner 错误） ----

func TestAutoClaw_PartialResultRetention_ScannerError(t *testing.T) {
	tmp := t.TempDir()
	// 有效首行 + 超过 maxJSONLLineSize 的行 -> bufio.Scanner 报错
	content := `{"type":"session","version":3,"id":"s","timestamp":"2026-07-29T12:00:00.000Z","cwd":"/tmp"}` + "\n" +
		acUsageLine("retained", acTS(2026, 7, 29), "zai", "zai_auto", 10, 5, 0, 0, 0, 15) + "\n" +
		strings.Repeat("x", maxJSONLLineSize+1) + "\n"
	root := writeAcFile(t, tmp, "oversize-agent", "oversize", content)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	// 首条有效消息必须保留
	found := false
	for _, m := range result.Messages {
		if m.ID == "retained" {
			found = true
		}
	}
	if !found {
		t.Errorf("scanner 错误时应保留已解析的 retained 消息, got msgs=%v", acMsgIDs(result.Messages))
	}
	// 同时报告 PartialErr
	if result.PartialErr == nil {
		t.Errorf("scanner 错误应报告 PartialErr")
	}
}

// ---- 用例 13：date 过滤 + Session 范围 ----

func TestAutoClaw_DateFilterAndSessionRange(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)

	// 只取 2026-07-29（session-aaa 全部在 07-29）
	date := autoclawTsMsToDate(acTS(2026, 7, 29))
	result, err := c.Collect(context.Background(), CollectRequest{Dates: []string{date}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	// 所有消息都应命中 07-29
	for _, m := range result.Messages {
		if m.Date != date {
			t.Errorf("日期过滤后 message.Date = %q, want %q", m.Date, date)
		}
	}

	// session-bbb 跨 07-29 和 07-30；只命中 07-29 时仍产出 Session，
	// 且 FirstTS/LastTS 取全文件范围（含 07-30 的消息 ts）
	var sessBBB *model.Session
	for i := range result.Sessions {
		if result.Sessions[i].ID == "session-bbb" {
			sessBBB = &result.Sessions[i]
		}
	}
	if sessBBB == nil {
		t.Fatalf("session-bbb 在 07-29 有命中消息时应产出 Session")
	}
	day1TS := int64(1785326410000)
	day2TS := int64(1785412810000)
	if sessBBB.FirstTS != day1TS {
		t.Errorf("session-bbb FirstTS = %d, want %d（全文件最早，不被日期过滤缩窄）", sessBBB.FirstTS, day1TS)
	}
	if sessBBB.LastTS != day2TS {
		t.Errorf("session-bbb LastTS = %d, want %d（全文件最晚，含未命中日期的 07-30 消息）", sessBBB.LastTS, day2TS)
	}

	// 仅命中 07-30 时，session-bbb 的消息只有 07-30 那条，但 Session 范围仍含 07-29
	date2 := autoclawTsMsToDate(acTS(2026, 7, 30))
	r2, err := c.Collect(context.Background(), CollectRequest{Dates: []string{date2}}, slog.Default())
	if err != nil {
		t.Fatalf("Collect(07-30) 失败: %v", err)
	}
	var sessBBB2 *model.Session
	for i := range r2.Sessions {
		if r2.Sessions[i].ID == "session-bbb" {
			sessBBB2 = &r2.Sessions[i]
		}
	}
	if sessBBB2 == nil {
		t.Fatalf("session-bbb 在 07-30 有命中时应产出 Session")
	}
	if sessBBB2.FirstTS != day1TS || sessBBB2.LastTS != day2TS {
		t.Errorf("session-bbb(07-30) FirstTS=%d LastTS=%d, want %d/%d（范围不被过滤缩窄）",
			sessBBB2.FirstTS, sessBBB2.LastTS, day1TS, day2TS)
	}
	// 只命中 07-30 时，bbb-day2 出现，bbb-day1 不出现
	for _, m := range r2.Messages {
		if m.ID == "bbb-day1" && m.SessionID == "session-bbb" {
			t.Errorf("日期过滤 07-30 不应含 07-29 的 bbb-day1")
		}
	}
}

// ---- 用例 14：跨天 session ----

func TestAutoClaw_MultiDaySession(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	d1 := findAcMsg(t, result.Messages, "bbb-day1")
	d2 := findAcMsg(t, result.Messages, "bbb-day2")
	if d1.Date == d2.Date {
		t.Errorf("跨天 session 应产出不同 date 的 Message, got d1=%s d2=%s", d1.Date, d2.Date)
	}
}

// ---- 用例 15：多 Agent 隔离 + Name 非空分支 ----

func TestAutoClaw_MultiAgentIsolation(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	// main 的 aaa-m2 provider=zai -> 智谱主实例
	m2 := findAcMsg(t, result.Messages, "aaa-m2")
	if m2.Provider != "智谱主实例" {
		t.Errorf("main 的 message Provider = %q, want 智谱主实例", m2.Provider)
	}
	// designer 的 eee-m1 provider=zai -> 智谱设计器实例（同 key 不同 name）
	e1 := findAcMsg(t, result.Messages, "eee-m1")
	if e1.Provider != "智谱设计器实例" {
		t.Errorf("designer 的 message Provider = %q, want 智谱设计器实例（不串读 main 的 name）", e1.Provider)
	}
	if m2.Provider == e1.Provider {
		t.Errorf("main 与 designer 同 key provider 应输出不同 name（隔离失败）: %s", m2.Provider)
	}
}

// ---- 用例 16：provider 取值全场景（name 为空回退） ----

func TestAutoClaw_ProviderFallback_EmptyName(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	// main-noname 的 fff-m1 provider=zai，但 models.json 无 name -> 回退 message.provider 原值 "zai"
	f1 := findAcMsg(t, result.Messages, "fff-m1")
	if f1.Provider != "zai" {
		t.Errorf("provider name 为空时应回退 message.provider 原值 %q, got %q", "zai", f1.Provider)
	}
}

// ---- 用例 18：ctx 取消（解析层，scanner 循环内确定性取消点） ----

func TestAutoClaw_CtxCancel_ParserLayer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// 受控 reader：第一次 Read 放出首行（scanner 解析并 append），第二次 Read 才 cancel。
	// 这样循环回到顶部 ctx.Err() 检查时 ctx 已取消，返回（已 append 首条）+ Canceled。
	// 若循环内 ctx 检查被移除，scanner 会继续读完第二行（msgs 含 2 条），断言 len==1 失败。
	firstLine := acUsageLine("cancel-pre", acTS(2026, 7, 29), "zai", "zai_auto", 10, 5, 0, 0, 0, 15) + "\n"
	secondLine := acUsageLine("cancel-post", acTS(2026, 7, 29), "zai", "zai_auto", 20, 6, 0, 0, 0, 26) + "\n"
	consumedFirst := make(chan struct{})
	reader := &controlledReader{
		firstLine:     []byte(firstLine),
		rest:          []byte(secondLine),
		consumedFirst: consumedFirst,
		cancel:        cancel,
	}
	msgs, _, err := parseAutoClawJSONLReader(ctx, reader, "test.jsonl", slog.Default())
	// 确认首行已被 scanner 消费（栅栏），否则测试无效
	select {
	case <-consumedFirst:
	default:
		t.Fatal("首行未被消费，测试未覆盖到循环内取消点")
	}
	// 解析层取消应返回 context.Canceled
	if !errors.Is(err, context.Canceled) {
		t.Errorf("parser 取消应返回 context.Canceled, got %v", err)
	}
	// 首条有效行必须已保留（部分结果保留），且第二行未解析（循环内 ctx 检查中断）
	if len(msgs) != 1 || msgs[0].ID != "cancel-pre" {
		t.Errorf("取消时应保留首条且仅首条 cancel-pre, got %d 条: %v", len(msgs), acParsedMsgIDs(msgs))
	}
}

// ---- 用例 18：ctx 取消（Collect 注入 parser） ----

func TestAutoClaw_CtxCancel_CollectPropagates(t *testing.T) {
	tmp := t.TempDir()
	content := acUsageLine("cancel-collect", acTS(2026, 7, 29), "zai", "zai_auto", 10, 5, 0, 0, 0, 15) + "\n"
	root := writeAcFile(t, tmp, "a1", "sess-cancel", content)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消
	_, err := c.Collect(ctx, CollectRequest{}, slog.Default())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("预先取消 Collect 应返回 context.Canceled, got %v", err)
	}
}

// ---- 用例 18：ctx 取消（扫描层 walker seam 确定性） ----

func TestAutoClaw_CtxCancel_ScanLayer(t *testing.T) {
	root := autoclawFixtureRoot(t)
	ctx, cancel := context.WithCancel(context.Background())

	// 受控 walker：先报告一个合法文件（调用 fn），再 cancel ctx，模拟"回调已开始后取消"。
	// autoClawWalker 签名 = func(root string, fn filepath.WalkFunc) error
	callbackStarted := make(chan struct{})
	walk := func(root string, fn filepath.WalkFunc) error {
		// 报告一个合法的 agents/{agent}/sessions/{sid}.jsonl 文件
		legal := filepath.Join(root, "main", "sessions", "session-aaa.jsonl")
		info, _ := os.Stat(legal)
		close(callbackStarted) // 标记回调已开始
		// 在回调内取消 ctx（确定性取消点，不依赖 sleep 或大文件竞态）
		cancel()
		return fn(legal, info, nil)
	}
	_, scanErr := scanAutoClawSessionsWith(ctx, root, walk)
	<-callbackStarted // 确保回调被触发（否则测试无效）
	// 取消应返回 context.Canceled（不降级为 PartialErr）
	if !errors.Is(scanErr, context.Canceled) {
		t.Errorf("扫描层取消应返回 context.Canceled（不降级）, got %v", scanErr)
	}
}

// ---- 用例 18 补充：扫描取消时已发现的文件保留在返回值中 ----

func TestAutoClaw_ScanCancel_KeepsAlreadyFoundFiles(t *testing.T) {
	root := autoclawFixtureRoot(t)
	ctx, cancel := context.WithCancel(context.Background())

	// 受控 walker：第一次调 fn 报告合法文件（innerCallback append 到 files），
	// 第二次调 fn 前取消 ctx——innerCallback 首检 ctx 返回 Canceled，walk 结束。
	// 断言返回的 files 含第一次已 append 的文件（取消不丢已发现结果）。
	walk := func(root string, fn filepath.WalkFunc) error {
		legal := filepath.Join(root, "main", "sessions", "session-aaa.jsonl")
		info, _ := os.Stat(legal)
		// 第一次：报告合法文件，innerCallback 应 append 到 files（此时 ctx 未取消）
		if err := fn(legal, info, nil); err != nil {
			return err
		}
		// 第二次前取消 ctx
		cancel()
		// 报告第二个文件，innerCallback 首检 ctx 返回 Canceled
		legal2 := filepath.Join(root, "main", "sessions", "session-bbb.jsonl")
		info2, _ := os.Stat(legal2)
		return fn(legal2, info2, nil)
	}
	files, scanErr := scanAutoClawSessionsWith(ctx, root, walk)
	// 取消应返回 context.Canceled
	if !errors.Is(scanErr, context.Canceled) {
		t.Errorf("扫描取消应返回 context.Canceled, got %v", scanErr)
	}
	// 已发现的第一个文件必须保留在 files 中（取消不丢部分结果）
	foundAAA := false
	for _, f := range files {
		if strings.HasSuffix(filepath.ToSlash(f), "main/sessions/session-aaa.jsonl") {
			foundAAA = true
		}
	}
	if !foundAAA {
		t.Errorf("扫描取消时应保留已发现的 session-aaa.jsonl, got files=%v", files)
	}
}

// ---- 空 sessions_dir 不 panic（默认路径回填由 analyzer 层覆盖） ----

func TestAutoClaw_EmptySessionsDir_NoPanic(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"autoclaw": {Enabled: true, Paths: map[string]string{"sessions_dir": ""}},
		},
	}
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("空 sessions_dir 不应失败: %v", err)
	}
	if len(result.Messages) != 0 || len(result.Sessions) != 0 {
		t.Errorf("空 sessions_dir 应返回空结果, got msgs=%d sess=%d", len(result.Messages), len(result.Sessions))
	}
}

// ---- 用例 20：sessions_dir 不存在返回空结果无 error ----

func TestAutoClaw_NonexistentSessionsDir_EmptyNoError(t *testing.T) {
	cfg := newAutoClawCfg(t, filepath.Join(t.TempDir(), "does-not-exist"))
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("不存在的 sessions_dir 不应失败: %v", err)
	}
	if len(result.Messages) != 0 || len(result.Sessions) != 0 {
		t.Errorf("不存在的 sessions_dir 应返回空结果, got msgs=%d sess=%d", len(result.Messages), len(result.Sessions))
	}
}

// ---- 用例 21：空白 sessions_dir 拒绝 ----

func TestAutoClaw_BlankSessionsDir_Rejected(t *testing.T) {
	for _, blank := range []string{"", "  ", "\t"} {
		t.Run("blank="+repr(blank), func(t *testing.T) {
			cfg := &config.Config{
				Clients: map[string]config.Client{
					"autoclaw": {Enabled: true, Paths: map[string]string{"sessions_dir": blank}},
				},
			}
			c := NewAutoClawCollector(cfg)
			// 全量扫描
			result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
			if err != nil {
				t.Fatalf("Collect 失败: %v", err)
			}
			if len(result.Messages) != 0 || len(result.Sessions) != 0 {
				t.Errorf("空白 sessions_dir 应返回空结果, got msgs=%d sess=%d", len(result.Messages), len(result.Sessions))
			}
			// ChangedFile 校验也拒绝（changedFile 指向 cwd 下某文件）
			anyFile := filepath.Join(t.TempDir(), "x.jsonl")
			os.WriteFile(anyFile, []byte("{}"), 0644)
			r2, err := c.Collect(context.Background(), CollectRequest{ChangedFile: anyFile}, slog.Default())
			if err != nil {
				t.Fatalf("Collect(ChangedFile) 失败: %v", err)
			}
			if len(r2.Messages) != 0 || len(r2.Sessions) != 0 {
				t.Errorf("空白 sessions_dir 的 ChangedFile 应返回空结果, got msgs=%d", len(r2.Messages))
			}
		})
	}
	// isAutoClawValidChangedFile 对空白 sessionsDir 返回 false
	if isAutoClawValidChangedFile("/tmp/agent/sessions/x.jsonl", "") {
		t.Errorf("空白 sessionsDir 时 isAutoClawValidChangedFile 应返回 false")
	}
	if isAutoClawValidChangedFile("/tmp/agent/sessions/x.jsonl", "  ") {
		t.Errorf("仅空白 sessionsDir 时 isAutoClawValidChangedFile 应返回 false")
	}
}

// ---- 用例 22：Walk 错误的部分结果保留（扫描 helper 层） ----

func TestAutoClaw_WalkError_PartialResultRetention(t *testing.T) {
	root := autoclawFixtureRoot(t)

	// 受控 walker：先报告一个合法文件（fn 处理后 collect），再返回非 ctx 错误。
	// autoClawWalker 签名 = func(root string, fn filepath.WalkFunc) error
	walkErr := errors.New("simulated walk failure")
	walk := func(root string, fn filepath.WalkFunc) error {
		legal := filepath.Join(root, "main", "sessions", "session-aaa.jsonl")
		info, _ := os.Stat(legal)
		_ = fn(legal, info, nil) // 先让收集回调处理合法文件
		return walkErr           // 再返回非 ctx 错误
	}

	// 扫描 helper 层：返回已发现文件 + 非 ctx 错误。
	// Collect 层该场景的端到端覆盖由 TestAutoClaw_Collect_WalkError_KeepsFoundMessagesAndPartialErr 承担。
	files, scanErr := scanAutoClawSessionsWith(context.Background(), root, walk)
	if len(files) == 0 {
		t.Errorf("Walk 错误时应保留已发现的文件")
	}
	if scanErr == nil {
		t.Errorf("Walk 非 ctx 错误应返回 error")
	}
}

// ---- 辅助：controlledReader（受控 reader seam，构造确定性取消点） ----

// controlledReader 分两次 Read 放数据：第一次只给 firstLine（scanner 解析并 append 后），
// 第二次 Read 时 close(consumedFirst) 标记首行已消费、cancel 触发取消，再返回 rest。
// 这样 parser 的 scanner 循环在下一轮顶部检查到 ctx 已取消，返回（已 append 首条）+ Canceled，
// 确定性验证循环内 ctx 检查（不依赖 sleep 或大文件竞态）。
type controlledReader struct {
	firstLine     []byte
	rest          []byte
	firstReturned bool
	consumedFirst chan struct{}
	cancel        context.CancelFunc
}

func (r *controlledReader) Read(p []byte) (int, error) {
	if !r.firstReturned {
		// 第一次 Read：只返回 firstLine 字节（即使 p 更大，强制 scanner 下次再来 Read）
		r.firstReturned = true
		n := copy(p, r.firstLine)
		return n, nil
	}
	// 第二次 Read：首行已被 scanner 消费，标记栅栏并 cancel，再返回 rest
	select {
	case <-r.consumedFirst:
	default:
		close(r.consumedFirst)
	}
	r.cancel()
	if len(r.rest) == 0 {
		return 0, nil
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

func repr(s string) string {
	if s == "" {
		return "<empty>"
	}
	return fmt.Sprintf("%q", s)
}

// ---- ChangedFile 单文件路径正确产出消息（综合） ----

func TestAutoClaw_ChangedFile_ProducesMessages(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)
	target := filepath.Join(root, "main", "sessions", "session-aaa.jsonl")
	result, err := c.Collect(context.Background(), CollectRequest{ChangedFile: target}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if len(result.Messages) == 0 {
		t.Fatalf("ChangedFile=合法文件应产出消息")
	}
	// aaa-m2 应存在
	findAcMsg(t, result.Messages, "aaa-m2")
}

// ---- 用例 17 敏感信息（断言式）----

func TestAutoClaw_Fixtures_NoApiKey(t *testing.T) {
	root := autoclawFixtureRoot(t)
	var buf bytes.Buffer
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(data), "apiKey") {
			buf.WriteString(path + " 含 apiKey\n")
		}
		return nil
	})
	if buf.Len() > 0 {
		t.Errorf("fixture 不应含 apiKey 字段:\n%s", buf.String())
	}
}

// ---- Minor-2：Collect 逐文件 parser 返回 ctx.Canceled → 透传且 PartialErr 为空 ----

func TestAutoClaw_Collect_ParserCancel_PropagatedNoPartialErr(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)

	// parseFn 注入：返回 ctx.Canceled 模拟 parser 在文件解析中途被取消。
	// 这覆盖 Collect 的 parseErr ctx 分支（autoclaw.go Collect 内 parseFile 后的判断），
	// 而非入口 ctx 检查。
	c.parseFn = func(ctx context.Context, path string, logger *slog.Logger) ([]autoclawParsedMessage, FileScanStatus, error) {
		return nil, FileScanStatus{}, context.Canceled
	}

	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	// Collect 应返回 context.Canceled（不降级为 PartialErr）
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Collect(parser=ctx.Canceled) 应返回 context.Canceled, got err=%v", err)
	}
	if result.PartialErr != nil {
		t.Errorf("ctx 取消不应记 PartialErr, got %v", result.PartialErr)
	}
}

// ---- Minor-3：Collect 层 Walk 错误 → 保留已发现文件消息 + PartialErr ----

func TestAutoClaw_Collect_WalkError_KeepsFoundMessagesAndPartialErr(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)

	// walkFn 注入：先让收集回调处理一个合法文件（main/sessions/session-aaa.jsonl），
	// 再返回非 ctx 错误模拟扫描中途故障。验证 Collect 保留该文件消息并记 PartialErr。
	walkErr := errors.New("simulated walk failure")
	c.walkFn = func(root string, fn filepath.WalkFunc) error {
		legal := filepath.Join(root, "main", "sessions", "session-aaa.jsonl")
		info, statErr := os.Stat(legal)
		if statErr != nil {
			t.Fatalf("测试前置：stat 合法文件失败: %v", statErr)
		}
		_ = fn(legal, info, nil) // 让 scanAutoClawSessionsWith 的收集回调 append 该文件
		return walkErr
	}

	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 不应返回 error（非 ctx 扫描错误降级为 PartialErr）: %v", err)
	}
	// 已发现文件的消息必须保留
	found := false
	for _, m := range result.Messages {
		if m.ID == "aaa-m2" {
			found = true
		}
	}
	if !found {
		t.Errorf("Walk 错误时应保留已发现文件的消息 aaa-m2, got msgs=%v", acMsgIDs(result.Messages))
	}
	// 非 ctx 扫描错误应记入 PartialErr
	if result.PartialErr == nil {
		t.Errorf("Walk 非 ctx 错误应记入 PartialErr")
	}
}

// ---- Minor-4：provider key 不匹配 / models.json 损坏 → 回退 message.provider 原值 ----

func TestAutoClaw_ProviderFallback_KeyMismatchAndCorruptJSON(t *testing.T) {
	tmp := t.TempDir()

	// 场景 1：message.provider 用一个 models.json 中不存在的 key
	content1 := acUsageLine("pm-mismatch", acTS(2026, 7, 29), "unknown_provider", "zai_auto", 10, 5, 0, 0, 0, 15) + "\n"
	// 用 a1 agent，其 models.json 含 zai provider（但 message.provider=unknown_provider 不匹配）
	modelsJSON := `{"providers":{"zai":{"name":"智谱主实例","baseUrl":"https://x"}}}`
	agentDir := filepath.Join(tmp, "agents", "a1", "agent")
	os.MkdirAll(agentDir, 0755)
	os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(modelsJSON), 0644)
	root1 := writeAcFile(t, tmp, "a1", "sess-mismatch", content1)

	cfg1 := newAutoClawCfg(t, root1)
	c1 := NewAutoClawCollector(cfg1)
	r1, err := c1.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	mm := findAcMsg(t, r1.Messages, "pm-mismatch")
	if mm.Provider != "unknown_provider" {
		t.Errorf("key 不匹配应回退 message.provider 原值 %q, got %q", "unknown_provider", mm.Provider)
	}

	// 场景 2：models.json 损坏（非合法 JSON）
	tmp2 := t.TempDir()
	content2 := acUsageLine("pm-corrupt", acTS(2026, 7, 29), "zai", "zai_auto", 10, 5, 0, 0, 0, 15) + "\n"
	agentDir2 := filepath.Join(tmp2, "agents", "a2", "agent")
	os.MkdirAll(agentDir2, 0755)
	os.WriteFile(filepath.Join(agentDir2, "models.json"), []byte("not json {"), 0644)
	root2 := writeAcFile(t, tmp2, "a2", "sess-corrupt", content2)

	cfg2 := newAutoClawCfg(t, root2)
	c2 := NewAutoClawCollector(cfg2)
	r2, err := c2.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败（损坏 models.json 不应阻断采集）: %v", err)
	}
	mc := findAcMsg(t, r2.Messages, "pm-corrupt")
	if mc.Provider != "zai" {
		t.Errorf("models.json 损坏应回退 message.provider 原值 %q, got %q", "zai", mc.Provider)
	}
}

// ---- Minor-5：全局断言无空 Message.ID（防空 id 脏行写入主键）----

func TestAutoClaw_NoMessageWithEmptyID(t *testing.T) {
	root := autoclawFixtureRoot(t)
	cfg := newAutoClawCfg(t, root)
	c := NewAutoClawCollector(cfg)
	result, err := c.Collect(context.Background(), CollectRequest{}, slog.Default())
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	for _, m := range result.Messages {
		if m.ID == "" {
			t.Errorf("不应产出空 Message.ID（防空 id 写入 (client,\"\") 主键）: %+v", m)
		}
		if m.SessionID == "" {
			t.Errorf("不应产出空 Message.SessionID: %+v", m)
		}
	}
}
