package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
)

// doctorLoad 构造以给定数据目录与启用客户端为有效配置的加载器(测试注入,
// 绕过真实 home 下的用户配置)。
func doctorLoad(dataDir string, enabled map[string]bool) func() (*config.Config, error) {
	return func() (*config.Config, error) {
		cfg := &config.Config{DataDir: dataDir, Clients: map[string]config.Client{}}
		for name, en := range enabled {
			cfg.Clients[name] = config.Client{Enabled: en}
		}
		return cfg, nil
	}
}

// runDoctorForTest 以注入依赖执行 doctor 并返回 stdout 全文;
// 统一断言输出不含连续两个空格(状态词与描述、双语拼接处均不得出现)。
func runDoctorForTest(t *testing.T, load func() (*config.Config, error), open func(string) (*db.DB, error)) string {
	t.Helper()
	cmd := newDoctorCmdWithDeps(load, open)
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("doctor Execute: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "  ") {
		t.Errorf("输出不得含连续两个空格:\n%s", out)
	}
	return out
}

// insertCollectionLogAge 以「距今 age」的时刻写入 collection_log:数据新鲜度
// 检查项按 time.Since 实时计算,固定日期会随运行日期漂移误判陈旧,故以相对
// 时间注入。写入前统一转 UTC 秒级文本,与 querier.Freshness 的解析口径一致
// (亚秒截断只会让 age 略增,不影响向下取整结果)。
func insertCollectionLogAge(t *testing.T, usageDB *db.DB, date, source string, age time.Duration) {
	t.Helper()
	collectedAtUTC := time.Now().UTC().Add(-age).Format(time.DateTime)
	insertCollectionLogAt(t, usageDB, date, source, collectedAtUTC)
}

// 全绿场景:有效配置 + 目录存在可写 + 已建库(含一条消息与一条采集记录)
// + 一个启用客户端 + 无未解决异常 → 各行 OK、结果 OK / 一切正常。
func TestDoctor_AllGreen(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "usage.db")
	usageDB, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertOneMessage(usageDB, "2026-09-01", "claude"); err != nil {
		t.Fatal(err)
	}
	insertCollectionLogAge(t, usageDB, "2026-09-01", "claude", 2*time.Hour)
	// doctor 将自行打开,先关闭测试连接。
	usageDB.Close()

	out := runDoctorForTest(t, doctorLoad(dataDir, map[string]bool{"claude": true}), db.Open)
	for _, want := range []string{
		"Health check / 健康检查",
		"Config / 配置: OK / 正常 ",
		"Data directory / 数据目录: OK / 正常 " + dataDir,
		"Database / 数据库: OK / 正常 ",
		"quick_check: ok",
		"Clients / 客户端: OK / 正常 1 (claude)",
		"Last collection / 最近采集: OK / 正常 ",
		"Data freshness / 数据新鲜度: OK / 正常 2 h ago / 2 小时前",
		"Unresolved errors / 未解决异常: OK / 正常 none / 无",
		"Daemon / 守护进程: INFO / 提示 ",
		"Result / 结果: OK / 一切正常",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺 %q:\n%s", want, out)
		}
	}
	// 成功路径探针零残留:数据目录内不得出现探针临时文件(usage.db 为正常产物)。
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".token-usage-doctor-") {
			t.Errorf("成功路径不得留下探针残留: %s", entry.Name())
		}
	}
}

// 配置加载失败:Config FAIL,后续依赖配置的五项全部 SKIPPED(不重复计数),
// Daemon 提示行照常输出;结果为 1 项失败。
func TestDoctor_ConfigLoadFailure(t *testing.T) {
	load := func() (*config.Config, error) { return nil, errors.New("boom-config") }
	out := runDoctorForTest(t, load, db.Open)

	if !strings.Contains(out, "Config / 配置: FAIL / 失败") || !strings.Contains(out, "boom-config") {
		t.Errorf("Config 应 FAIL 并携带错误:\n%s", out)
	}
	// Data directory / Database / Clients / Last collection / Data freshness /
	// Unresolved errors / Query definitions 共 7 项跳过。
	if n := strings.Count(out, "SKIPPED / 跳过"); n != 7 {
		t.Errorf("依赖配置的检查项应恰 7 行 SKIPPED,实际 %d:\n%s", n, out)
	}
	if !strings.Contains(out, "Daemon / 守护进程: INFO / 提示") {
		t.Errorf("Daemon 提示行不受配置失败影响:\n%s", out)
	}
	if !strings.Contains(out, "Result / 结果: 1 problems / 1 项失败") {
		t.Errorf("结果应为 1 项失败:\n%s", out)
	}
}

// 数据目录不存在:目录 FAIL;数据库按「文件不存在」记 WARN(尚未创建);
// 依赖数据库的三项(最近采集/数据新鲜度/未解决异常)SKIPPED;结果为 1 项失败。
func TestDoctor_DataDirMissingFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-exist")
	out := runDoctorForTest(t, doctorLoad(missing, map[string]bool{"claude": true}), db.Open)

	if !strings.Contains(out, "Data directory / 数据目录: FAIL / 失败") ||
		!strings.Contains(out, "目录不存在") {
		t.Errorf("缺失数据目录应 FAIL:\n%s", out)
	}
	if !strings.Contains(out, "Database / 数据库: WARN / 警告") ||
		!strings.Contains(out, "尚未创建") {
		t.Errorf("数据库不存在应 WARN 尚未创建:\n%s", out)
	}
	if strings.Count(out, "SKIPPED / 跳过") != 3 {
		t.Errorf("依赖数据库的三项应 SKIPPED:\n%s", out)
	}
	if !strings.Contains(out, "Result / 结果: 1 problems / 1 项失败") {
		t.Errorf("结果应为 1 项失败:\n%s", out)
	}
}

// 目录存在但数据库未创建:目录 OK,数据库 WARN 尚未创建,结果恰 1 项警告。
func TestDoctor_DbMissingWarns(t *testing.T) {
	dataDir := t.TempDir()
	out := runDoctorForTest(t, doctorLoad(dataDir, map[string]bool{"claude": true}), db.Open)

	if !strings.Contains(out, "Data directory / 数据目录: OK / 正常 "+dataDir) {
		t.Errorf("存在的数据目录应 OK:\n%s", out)
	}
	if !strings.Contains(out, "Database / 数据库: WARN / 警告") ||
		!strings.Contains(out, "尚未创建") {
		t.Errorf("未创建的数据库应 WARN:\n%s", out)
	}
	if !strings.Contains(out, "Result / 结果: 1 warnings / 1 项警告") {
		t.Errorf("结果应为 1 项警告:\n%s", out)
	}
}

// 库中存在未解决 collection_errors:Unresolved errors WARN + 数量与重试提示,
// 结果恰 1 项警告。
func TestDoctor_UnresolvedErrorsWarn(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "usage.db")
	usageDB, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordError(context.Background(), usageDB, "2026-09-01", "claude", "boom", ""); err != nil {
		t.Fatal(err)
	}
	// 补一条采集记录,使 Last collection 为 OK,隔离出仅 Unresolved 一项警告。
	insertCollectionLogAge(t, usageDB, "2026-09-01", "claude", 2*time.Hour)
	usageDB.Close()

	out := runDoctorForTest(t, doctorLoad(dataDir, map[string]bool{"claude": true}), db.Open)
	if !strings.Contains(out, "Unresolved errors / 未解决异常: WARN / 警告") {
		t.Errorf("存在未解决异常应 WARN:\n%s", out)
	}
	for _, want := range []string{"1 条未解决", "collect retry"} {
		if !strings.Contains(out, want) {
			t.Errorf("警告应含 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Result / 结果: 1 warnings / 1 项警告") {
		t.Errorf("结果应为 1 项警告:\n%s", out)
	}
}

// 0 字节 usage.db 是「文件已建但从未初始化」的形态:与不存在同路径记 WARN,
// 且 doctor 不得打开它——断言文件仍为 0 字节即证明 doctor 未触发 schema 迁移
// (这是本测试的区分度锚点);依赖数据库的三项 SKIPPED。
func TestDoctor_EmptyDbFileNotInitialized(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "usage.db")
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	out := runDoctorForTest(t, doctorLoad(dataDir, map[string]bool{"claude": true}), db.Open)
	if !strings.Contains(out, "Database / 数据库: WARN / 警告") ||
		!strings.Contains(out, "empty database file") || !strings.Contains(out, "空数据库文件") {
		t.Errorf("空数据库文件应 WARN 且提示初始化:\n%s", out)
	}
	if strings.Count(out, "SKIPPED / 跳过") != 3 {
		t.Errorf("依赖数据库的三项应 SKIPPED:\n%s", out)
	}
	// 区分度锚点:doctor 不得初始化空库文件,大小必须仍为 0。
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("doctor 不得初始化 0 字节数据库文件,实际大小 %d", info.Size())
	}
	if !strings.Contains(out, "Result / 结果: 1 warnings / 1 项警告") {
		t.Errorf("结果应为 1 项警告:\n%s", out)
	}
}

// 数据目录只读:可写探针失败 → FAIL,且探针不留任何文件残留。
// 以非 root 运行才生效(root 无视文件权限);Windows 目录权限语义不同,跳过。
func TestDoctor_ReadOnlyDataDirProbe(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows 目录写权限语义不同,只读探针场景仅在类 Unix 平台验证")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 无视文件权限,只读探针场景不适用")
	}
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o755) })

	out := runDoctorForTest(t, doctorLoad(dataDir, map[string]bool{"claude": true}), db.Open)
	if !strings.Contains(out, "Data directory / 数据目录: FAIL / 失败") ||
		!strings.Contains(out, "不可写") {
		t.Errorf("只读数据目录应 FAIL 不可写:\n%s", out)
	}
	// 探针文件必须即建即删:目录内不得留下任何残留。
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("只读探针不得留下残留文件: %v", entries)
	}
}

// doctor 不接受位置参数:Args 校验报双语错误;零参数通过。
func TestDoctor_RejectsPositionalArgs(t *testing.T) {
	cmd := newDoctorCmd()
	if err := cmd.Args(cmd, nil); err != nil {
		t.Errorf("零参数应通过: %v", err)
	}
	err := cmd.Args(cmd, []string{"extra"})
	if err == nil {
		t.Fatal("位置参数应报错")
	}
	msg := err.Error()
	for _, want := range []string{"/", "1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误应为双语并含数量 %q: %q", want, msg)
		}
	}
}

// Query definitions 检查项:合法定义 OK;坏定义 WARN 并携带问题计数与首个
// 诊断路径;仅 WARN 不 FAIL(配置是纯展示态,不阻断采集与其他命令)。
func TestDoctor_QueryDefinitions(t *testing.T) {
	// dataDir 用已存在的临时目录:避免目录缺失的 FAIL 干扰「仅 WARN 不 FAIL」断言。
	dataDir := t.TempDir()

	// 合法定义(一个子查询)。
	good := func() (*config.Config, error) {
		cfg := &config.Config{
			DataDir: dataDir,
			Clients: map[string]config.Client{},
			RawQuery: map[string]any{
				"subqueries": map[string]any{"mp": "model,provider"},
			},
		}
		return cfg, nil
	}
	out := runDoctorForTest(t, good, db.Open)
	if !strings.Contains(out, "Query definitions / 查询视图: OK / 正常") || !strings.Contains(out, "定义合法") {
		t.Errorf("合法定义应 OK:\n%s", out)
	}

	// 非法定义(子查询引用未知视图 g)。
	bad := func() (*config.Config, error) {
		cfg := &config.Config{
			DataDir: dataDir,
			Clients: map[string]config.Client{},
			RawQuery: map[string]any{
				"subqueries": map[string]any{"bad": "model,g"},
			},
		}
		return cfg, nil
	}
	out = runDoctorForTest(t, bad, db.Open)
	if !strings.Contains(out, "Query definitions / 查询视图: WARN / 警告") {
		t.Errorf("坏定义应 WARN:\n%s", out)
	}
	if !strings.Contains(out, "1 issue(s) / 项问题") || !strings.Contains(out, "query.subqueries.bad") {
		t.Errorf("WARN 应含问题计数与首个诊断路径:\n%s", out)
	}
	if strings.Contains(out, "problems / 项失败") {
		t.Errorf("仅 WARN 不应计入失败:\n%s", out)
	}
	if !strings.Contains(out, "3 warnings / 3 项警告") {
		t.Errorf("结果应恰 3 项警告(数据库未创建 + 未启用客户端 + 坏视图定义):\n%s", out)
	}
	if !strings.Contains(out, "运行 `token-usage query list` 查看详情") {
		t.Errorf("WARN 应指向 query list:\n%s", out)
	}
}

// 数据新鲜度陈旧分支:time.Since 不可注入,以 8 天前的采集记录触发
// (距阈值 7 天有整日余量,秒级截断误差不影响判定)。
func TestDoctor_DataFreshness_Stale(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "usage.db")
	usageDB, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertOneMessage(usageDB, "2026-09-01", "claude"); err != nil {
		t.Fatal(err)
	}
	insertCollectionLogAge(t, usageDB, "2026-09-01", "claude", 8*24*time.Hour)
	usageDB.Close()

	out := runDoctorForTest(t, doctorLoad(dataDir, map[string]bool{"claude": true}), db.Open)
	if !strings.Contains(out, "Data freshness / 数据新鲜度: WARN / 警告") {
		t.Errorf("距最近采集超过 7 天应 WARN:\n%s", out)
	}
	for _, want := range []string{"8 d ago", "collect"} {
		if !strings.Contains(out, want) {
			t.Errorf("WARN 应含 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Result / 结果: 1 warnings / 1 项警告") {
		t.Errorf("结果应恰 1 项警告(仅数据新鲜度):\n%s", out)
	}
}

// 无采集记录:数据新鲜度 SKIPPED(上一项 Last collection 已 WARN,不重复计数)。
func TestDoctor_DataFreshness_NoCollection(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "usage.db")
	usageDB, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertOneMessage(usageDB, "2026-09-01", "claude"); err != nil {
		t.Fatal(err)
	}
	usageDB.Close()

	out := runDoctorForTest(t, doctorLoad(dataDir, map[string]bool{"claude": true}), db.Open)
	if !strings.Contains(out, "Data freshness / 数据新鲜度: SKIPPED / 跳过") ||
		!strings.Contains(out, "no collection recorded / 无采集记录") {
		t.Errorf("无采集记录应 SKIPPED:\n%s", out)
	}
	if !strings.Contains(out, "Result / 结果: 1 warnings / 1 项警告") {
		t.Errorf("结果应恰 1 项警告(仅 Last collection):\n%s", out)
	}
}

// 阈值判断边界(直接单测阈值函数,不经过整条 doctor):恰 7 天不算陈旧,
// 7 天+1ns 即陈旧。
func TestDoctorFreshnessStale_Threshold(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want bool
	}{
		{0, false},
		{6*24*time.Hour + 23*time.Hour + 59*time.Minute, false},
		{7 * 24 * time.Hour, false},
		{7*24*time.Hour + time.Nanosecond, true},
		{8 * 24 * time.Hour, true},
	}
	for _, tc := range cases {
		if got := doctorFreshnessStale(tc.age); got != tc.want {
			t.Errorf("doctorFreshnessStale(%v) = %v, want %v", tc.age, got, tc.want)
		}
	}
}

// 人性化时长三段与边界:<1h 刚刚;1h–<24h 小时;≥24h 天(X 向下取整)。
func TestDoctorFreshnessDesc_Table(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want string
	}{
		{0, "just now / 刚刚"},
		{59 * time.Minute, "just now / 刚刚"},
		{59*time.Minute + 59*time.Second, "just now / 刚刚"},
		{time.Hour, "1 h ago / 1 小时前"},
		{23*time.Hour + 59*time.Minute, "23 h ago / 23 小时前"},
		{24 * time.Hour, "1 d ago / 1 天前"},
		{47*time.Hour + 59*time.Minute, "1 d ago / 1 天前"},
		{8 * 24 * time.Hour, "8 d ago / 8 天前"},
	}
	for _, tc := range cases {
		if got := doctorFreshnessDesc(tc.age); got != tc.want {
			t.Errorf("doctorFreshnessDesc(%v) = %q, want %q", tc.age, got, tc.want)
		}
	}
}
