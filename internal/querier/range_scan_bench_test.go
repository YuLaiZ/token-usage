package querier

// range_scan_bench_test.go 提供可重跑的验收数据集生成与单场景测量入口。
//
// 常规 `go test ./...` 不会执行它们：只有设置 PERF_GEN/PERF_MODE 环境变量时
// 才运行。数据集写入临时目录，不进入仓库；生成器使用固定种子，结果可复现。
//
// 用法示例：
//
//	PERF_GEN=b0 PERF_DB=/tmp/perf-b0.db go test ./internal/querier -run TestPerfGenerate -count=1
//	PERF_MODE=scan PERF_DB=/tmp/perf-b0.db PERF_SPECS=b0 go test ./internal/querier -run TestPerfMeasure -count=1 -v
//
// 峰值 RSS 由外部用 `/usr/bin/time -l`（macOS）或平台对应工具包住测量进程，
// 并与 PERF_MODE=noop 的空操作对照相减。

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"testing"

	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

const perfSeed = int64(20261023)

// perfDates 生成 214 个日历日（2026-03-23 起），B0 使用其中 166 个活跃日，
// 与真实库的采样范围一致。
func perfDates() []string {
	start := time.Date(2026, 3, 23, 0, 0, 0, 0, time.Local)
	out := make([]string, 214)
	for i := range out {
		out[i] = start.AddDate(0, 0, i).Format("2006-01-02")
	}
	return out
}

// perfMessage 是生成器的单行描述，避免反复构造完整 model.Message。
type perfMessage struct {
	id, session, client, date, model, provider, project string
	ts                                                  int64
	total, fresh, output, cacheRead, cacheReasoning     int64
}

func perfBatchInsert(ctx context.Context, t *testing.T, testDB *db.DB, rows []perfMessage) {
	t.Helper()
	const batch = 20000
	for start := 0; start < len(rows); start += batch {
		end := start + batch
		if end > len(rows) {
			end = len(rows)
		}
		msgs := make([]model.Message, 0, end-start)
		for _, r := range rows[start:end] {
			msgs = append(msgs, model.Message{
				ID: r.id, SessionID: r.session, Client: r.client, Date: r.date, TS: r.ts,
				Model: r.model, Provider: r.provider, Project: r.project,
				FreshInputTokens: r.fresh, OutputTokens: r.output,
				CacheReadTokens: r.cacheRead, ReasoningTokens: r.cacheReasoning,
				TotalTokens: r.total,
			})
		}
		if _, err := db.UpsertMessages(ctx, testDB, msgs); err != nil {
			t.Fatalf("UpsertMessages batch %d failed: %v", start, err)
		}
	}
}

// generateB0 生成约 13.6 万行、166 个活跃日、8 个规格的基准数据集。
func generateB0(t *testing.T, testDB *db.DB) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(perfSeed))
	dates := perfDates()
	clients := []string{"Claude Code", "Codex", "OpenCode", "ZCode", "MiMo Code", "WorkBuddy", "DSH", "OpenAI"}
	models := []string{"GLM-5.3", "GLM-5.3-Flash", "claude-sonnet-4", "claude-opus-4", "gpt-5", "gpt-5.5", "qwen-3", "deepseek-v3", "kimi-k2", "gemini-2.5-pro"}
	providers := []string{"Zhipu", "Anthropic", "OpenAI", "Google", "Moonshot", ""}

	sessions := make([]model.Session, 0, 3000)
	for i := 0; i < 3000; i++ {
		sessions = append(sessions, model.Session{
			ID: fmt.Sprintf("sess-%05d", i), Client: clients[rng.Intn(len(clients))],
			Project: fmt.Sprintf("proj-%03d", rng.Intn(200)),
			Title:   fmt.Sprintf("session title %d", i),
		})
	}
	if _, err := db.UpsertSessionMeta(ctx, testDB, sessions); err != nil {
		t.Fatalf("UpsertSessionMeta failed: %v", err)
	}

	const rows = 136110
	out := make([]perfMessage, 0, rows)
	for i := 0; i < rows; i++ {
		di := rng.Intn(166)
		date := dates[di]
		dayStart, _ := time.ParseInLocation("2006-01-02", date, time.Local)
		ts := dayStart.Add(time.Duration(rng.Intn(24*60*60)) * time.Second).UnixMilli()
		client := clients[rng.Intn(len(clients))]
		total := int64(rng.Intn(3000000))
		out = append(out, perfMessage{
			id: fmt.Sprintf("msg-%06d", i), session: fmt.Sprintf("sess-%05d", rng.Intn(3000)),
			client: client, date: date, model: models[rng.Intn(len(models))],
			provider: providers[rng.Intn(len(providers))],
			project:  fmt.Sprintf("proj-%03d", rng.Intn(200)),
			ts:       ts, total: total, fresh: total / 2, output: total / 4,
			cacheRead: total / 5, cacheReasoning: total / 20,
		})
	}
	perfBatchInsert(ctx, t, testDB, out)
}

// generateB1 在 B0 分布基础上扩展到约 50 万行。
func generateB1(t *testing.T, testDB *db.DB) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(perfSeed + 1))
	dates := perfDates()
	clients := []string{"Claude Code", "Codex", "OpenCode", "ZCode", "MiMo Code", "WorkBuddy", "DSH", "OpenAI"}
	models := []string{"GLM-5.3", "GLM-5.3-Flash", "claude-sonnet-4", "claude-opus-4", "gpt-5", "gpt-5.5", "qwen-3", "deepseek-v3", "kimi-k2", "gemini-2.5-pro"}
	providers := []string{"Zhipu", "Anthropic", "OpenAI", "Google", "Moonshot", ""}

	sessions := make([]model.Session, 0, 12000)
	for i := 0; i < 12000; i++ {
		sessions = append(sessions, model.Session{
			ID: fmt.Sprintf("sess-%05d", i), Client: clients[rng.Intn(len(clients))],
			Project: fmt.Sprintf("proj-%03d", rng.Intn(800)),
			Title:   fmt.Sprintf("session title %d", i),
		})
	}
	if _, err := db.UpsertSessionMeta(ctx, testDB, sessions); err != nil {
		t.Fatalf("UpsertSessionMeta failed: %v", err)
	}

	const rows = 500000
	out := make([]perfMessage, 0, rows)
	for i := 0; i < rows; i++ {
		di := rng.Intn(214)
		date := dates[di]
		dayStart, _ := time.ParseInLocation("2006-01-02", date, time.Local)
		ts := dayStart.Add(time.Duration(rng.Intn(24*60*60)) * time.Second).UnixMilli()
		total := int64(rng.Intn(3000000))
		out = append(out, perfMessage{
			id: fmt.Sprintf("msg-%06d", i), session: fmt.Sprintf("sess-%05d", rng.Intn(12000)),
			client: clients[rng.Intn(len(clients))], date: date,
			model: models[rng.Intn(len(models))], provider: providers[rng.Intn(len(providers))],
			project: fmt.Sprintf("proj-%03d", rng.Intn(800)), ts: ts,
			total: total, fresh: total / 2, output: total / 4, cacheRead: total / 5, cacheReasoning: total / 20,
		})
	}
	perfBatchInsert(ctx, t, testDB, out)
}

// generateB2 生成高基数数据：约 20 万行，project/model/provider 基数高且
// 含大小写变体，使单规格分组数接近消息行数。
func generateB2(t *testing.T, testDB *db.DB) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(perfSeed + 2))
	dates := perfDates()[:166]
	clients := []string{"Claude Code", "Codex", "OpenCode", "ZCode"}
	providers := []string{"Provider-A", "provider-a", "Provider-B", "provider-b", "", "Provider-C"}

	const rows = 200000
	out := make([]perfMessage, 0, rows)
	for i := 0; i < rows; i++ {
		date := dates[rng.Intn(len(dates))]
		dayStart, _ := time.ParseInLocation("2006-01-02", date, time.Local)
		ts := dayStart.Add(time.Duration(rng.Intn(24*60*60)) * time.Second).UnixMilli()
		total := int64(rng.Intn(1000000))
		// project/model 用消息序号派生高位唯一值，制造接近行数的分组基数。
		out = append(out, perfMessage{
			id: fmt.Sprintf("msg-%06d", i), session: fmt.Sprintf("sess-%05d", rng.Intn(5000)),
			client: clients[rng.Intn(len(clients))], date: date,
			model:    fmt.Sprintf("model-%05d", i%50000),
			provider: providers[rng.Intn(len(providers))],
			project:  fmt.Sprintf("proj-%06d", i%50000),
			ts:       ts, total: total, fresh: total / 2, output: total / 4,
			cacheRead: total / 5, cacheReasoning: total / 20,
		})
	}
	perfBatchInsert(ctx, t, testDB, out)
}

// generateB3 生成 20 万条历史会话、少量消息的数据：选区只命中少量会话，
// 用于检验 sessions 全表映射的成本与“无会话规格不加载”的对照。
func generateB3(t *testing.T, testDB *db.DB) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(perfSeed + 3))
	sessions := make([]model.Session, 0, 200000)
	for i := 0; i < 200000; i++ {
		title := fmt.Sprintf("t%d", i)
		if i%100 == 0 {
			title = fmt.Sprintf("a deliberately long session title used for mapping cost %06d", i)
		}
		sessions = append(sessions, model.Session{
			ID: fmt.Sprintf("sess-%06d", i), Client: "Claude Code",
			Project: fmt.Sprintf("proj-%05d", i%50000), Title: title,
		})
	}
	for start := 0; start < len(sessions); start += 20000 {
		end := start + 20000
		if end > len(sessions) {
			end = len(sessions)
		}
		if _, err := db.UpsertSessionMeta(ctx, testDB, sessions[start:end]); err != nil {
			t.Fatalf("UpsertSessionMeta batch %d failed: %v", start, err)
		}
	}

	out := make([]perfMessage, 0, 10000)
	for i := 0; i < 10000; i++ {
		date := "2026-10-22"
		dayStart, _ := time.ParseInLocation("2006-01-02", date, time.Local)
		ts := dayStart.Add(time.Duration(rng.Intn(24*60*60)) * time.Second).UnixMilli()
		out = append(out, perfMessage{
			id: fmt.Sprintf("msg-%06d", i), session: fmt.Sprintf("sess-%06d", rng.Intn(10)),
			client: "Claude Code", date: date, model: "m", provider: "p", project: "proj-00000",
			ts: ts, total: 100, fresh: 50, output: 25, cacheRead: 20, cacheReasoning: 5,
		})
	}
	perfBatchInsert(ctx, t, testDB, out)
}

// TestPerfGenerate 按 PERF_GEN 生成对应数据集到 PERF_DB。
func TestPerfGenerate(t *testing.T) {
	kind := os.Getenv("PERF_GEN")
	if kind == "" {
		t.Skip("set PERF_GEN=b0|b1|b2|b3 to generate a benchmark dataset")
	}
	path := os.Getenv("PERF_DB")
	if path == "" {
		t.Fatal("PERF_DB is required")
	}
	_ = os.Remove(path)
	testDB, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open %s failed: %v", path, err)
	}
	defer testDB.Close()

	switch kind {
	case "b0":
		generateB0(t, testDB)
	case "b1":
		generateB1(t, testDB)
	case "b2":
		generateB2(t, testDB)
	case "b3":
		generateB3(t, testDB)
	default:
		t.Fatalf("unknown PERF_GEN %q", kind)
	}
	t.Logf("generated %s at %s", kind, path)
}

// perfSpecs 按 B2 需要的 N 构造维度组合规格；N=5 使用前 5 个组合。
func perfSpecs(kind string) []AggSpec {
	aliases := map[string]string{"provider-a": "Provider-A"}
	switch kind {
	case "b0":
		// 与用户真实配置的 8 规格一致：4 内置维度 + 2 自定义视图 + dayhour + session。
		return []AggSpec{
			{Name: "client", Kind: AggDimension, Dimensions: []string{"client"}, Aliases: aliases},
			{Name: "model", Kind: AggDimension, Dimensions: []string{"model"}, Aliases: aliases},
			{Name: "provider", Kind: AggDimension, Dimensions: []string{"provider"}, Aliases: aliases},
			{Name: "project", Kind: AggDimension, Dimensions: []string{"project"}, Aliases: aliases},
			{Name: "mpc", Kind: AggDimension, Dimensions: []string{"model", "provider", "client"}, Aliases: aliases},
			{Name: "mc", Kind: AggDimension, Dimensions: []string{"model", "client"}, Aliases: aliases},
			{Kind: AggDayHour},
			{Kind: AggSession},
		}
	case "b3", "session-only":
		return []AggSpec{{Kind: AggSession}}
	case "b3-nosession":
		return []AggSpec{
			{Name: "project", Kind: AggDimension, Dimensions: []string{"project"}, Aliases: aliases},
		}
	}

	dims := [][]string{
		{"client"}, {"model"}, {"provider"}, {"project"},
		{"model", "client"}, {"model", "provider"}, {"project", "client"},
		{"model", "provider", "client"}, {"project", "model", "client"},
		{"project", "model", "provider", "client"}, {"weekday", "hour"},
		{"day"}, {"month"}, {"weekday"}, {"hour"},
		{"client", "project"}, {"provider", "client"}, {"model", "project"},
		{"provider", "project"}, {"day", "client"},
	}
	n := 8
	switch kind {
	case "b2-5":
		n = 5
	case "b2-10":
		n = 10
	case "b2-20":
		n = 20
	}
	specs := make([]AggSpec, 0, n)
	for i := 0; i < n; i++ {
		specs = append(specs, AggSpec{
			Name: fmt.Sprintf("v%d", i), Kind: AggDimension, Dimensions: dims[i],
			Aliases: aliases,
		})
	}
	return specs
}

// TestPerfMeasure 按 PERF_MODE 执行一次场景并输出耗时；由外部统计峰值 RSS。
func TestPerfMeasure(t *testing.T) {
	mode := os.Getenv("PERF_MODE")
	if mode == "" {
		t.Skip("set PERF_MODE=noop|legacy|scan to run a measurement")
	}
	path := os.Getenv("PERF_DB")
	if path == "" {
		t.Fatal("PERF_DB is required")
	}
	specKind := os.Getenv("PERF_SPECS")
	if specKind == "" {
		specKind = "b0"
	}
	testDB, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open %s failed: %v", path, err)
	}
	defer testDB.Close()
	q := New(testDB)
	ctx := context.Background()
	// B0 使用完整 214 天区间，与真实库采样范围一致（命中 166 个活跃日）。
	dates := perfDates()
	if specKind == "b3" || specKind == "b3-nosession" {
		dates = []string{"2026-10-22"}
	}

	if mode == "noop" {
		t.Logf("noop: db=%s", path)
		reportPeakRSS(t)
		return
	}
	defer reportPeakRSS(t)

	start := time.Now()
	switch mode {
	case "scan":
		specs := perfSpecs(specKind)
		snap, err := q.ScanRange(ctx, dates, specs)
		if err != nil {
			t.Fatalf("ScanRange failed: %v", err)
		}
		_ = snap.Totals()
		rows := 0
		for _, spec := range specs {
			if spec.Kind != AggDimension {
				continue
			}
			if r, ok := snap.Dimension(spec.Name); ok {
				rows += len(r.Rows)
			}
		}
		t.Logf("scan %s: %v rows=%d", specKind, time.Since(start), rows)
	case "legacy":
		// 与旧 dashboard 相同的调用序列：按规格逐项查询，不重复内置维度。
		if _, err := q.StatsBetween(ctx, dates[0], dates[len(dates)-1]); err != nil {
			t.Fatalf("StatsBetween failed: %v", err)
		}
		hasSession, hasDayHour := false, false
		for _, spec := range perfSpecs(specKind) {
			switch spec.Kind {
			case AggDimension:
				if _, _, err := q.AggregateDimensionView(ctx, dates, DimensionView{
					Dimensions: spec.Dimensions, Aliases: spec.Aliases,
				}); err != nil {
					t.Fatalf("AggregateDimensionView(%v) failed: %v", spec.Dimensions, err)
				}
			case AggSession:
				hasSession = true
			case AggDayHour:
				hasDayHour = true
			}
		}
		if hasSession {
			if _, err := q.SessionRows(ctx, dates); err != nil {
				t.Fatalf("SessionRows failed: %v", err)
			}
		}
		if hasDayHour {
			if _, err := q.DayHourMatrix(ctx, dates); err != nil {
				t.Fatalf("DayHourMatrix failed: %v", err)
			}
		}
		t.Logf("legacy %s: %v", specKind, time.Since(start))
	default:
		t.Fatalf("unknown PERF_MODE %q", mode)
	}
}

// reportPeakRSS 在 Linux 上输出进程峰值 RSS（/proc/self/status 的 VmHWM，
// 单位 kB），供容器内资源验收；其他平台无该文件，静默跳过。
func reportPeakRSS(t *testing.T) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			t.Logf("peak-rss: %s", strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:")))
			return
		}
	}
}
