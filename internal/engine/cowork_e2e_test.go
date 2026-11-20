package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// isolateClaudeDesktopEnv 把 HOME/USERPROFILE/APPDATA 一并隔离到临时目录。
// 生产装配按当前进程的 APPDATA 构造 Cowork 根（Windows），只隔离 HOME 会让测试
// 查到真实应用数据目录、甚至读到本机 Cowork 文件。
func isolateClaudeDesktopEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	return home
}

// currentCoworkRoots 返回按当前进程环境（与生产装配同一取值方式）构造的 Cowork
// 根，测试夹具与采集器因此始终指向同一目录。
func currentCoworkRoots() []string {
	home, _ := os.UserHomeDir()
	return collector.ClaudeCoworkRoots(home, runtime.GOOS, os.Getenv)
}

// TestRunCollect_CoworkEndToEnd 在隔离环境 + 内存库上端到端验证 Cowork 采集写入。
// 4 个合成 transcript（含三种映射形态）经 RunCollect 全量采集后入库，并核对
// 三条验收基线：audit 消息 0、跨 client 同 message.id 0、首末 ts 为 0 的会话 0。
func TestRunCollect_CoworkEndToEnd(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根（如 Linux）")
	}
	root := roots[0]
	acct, profile := "acct-1", "profile-1"
	profileDir := filepath.Join(root, acct, profile)
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeMapping := func(name, cliID, title, folder, project string) {
		t.Helper()
		var b strings.Builder
		b.WriteString(`{"cliSessionId":"` + cliID + `","title":"` + title + `"`)
		if folder != "" {
			b.WriteString(`,"userSelectedFolders":["` + folder + `"]`)
			b.WriteString(`,"folderMountNames":{"` + folder + `":"` + project + `"}`)
		}
		b.WriteString("}")
		if err := os.WriteFile(filepath.Join(profileDir, name), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript := func(sessionDir, id string, msgIDs ...string) {
		t.Helper()
		proj := filepath.Join(profileDir, sessionDir, ".claude", "projects", "session")
		if err := os.MkdirAll(proj, 0o755); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for i, mid := range msgIDs {
			b.WriteString(`{"type":"assistant","sessionId":"` + id + `","timestamp":"2026-11-16T10:0`)
			b.WriteByte(byte('0' + i))
			b.WriteString(`:00+08:00","cwd":"/private/var/empty","message":{"id":"` + mid + `","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}`)
			b.WriteString("\n")
		}
		if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeMapping("local_aaaabbbb-1111.json", "sess-short", "当前模式查询", "", "")
	writeTranscript("aaaabbbb", "sess-short", "m-short-1", "m-short-2")
	writeMapping("local_ccccdddd-2222.json", "sess-with-folder", "当前模式功能", "/work/proj", "myproj")
	writeTranscript("ccccdddd", "sess-with-folder", "m-folder-1")
	writeMapping("local_72ae64f7-3333.json", "sess-archived", "What needs my attention", "", "")
	writeTranscript("local_72ae64f7-3333", "sess-archived", "m-a1", "m-a2", "m-a3", "m-a4", "m-a5", "m-a6")
	writeTranscript("local_72ae64f7-3333", "sess-fallback", "m-fb-1")

	projectsDir := t.TempDir()
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	res := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{}, false, false)
	if !res.Complete() {
		t.Fatalf("RunCollect 失败: %+v", res)
	}

	var desktopMsgs int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client=?`, model.ClientClaudeDesktop).Scan(&desktopMsgs); err != nil {
		t.Fatal(err)
	}
	if desktopMsgs != 10 {
		t.Fatalf("Claude Desktop 消息数 = %d, want 10", desktopMsgs)
	}
	var sessions int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE client=?`, model.ClientClaudeDesktop).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 4 {
		t.Fatalf("Claude Desktop 会话数 = %d, want 4", sessions)
	}
	var dir, proj string
	if err := usageDB.QueryRow(`SELECT directory, project FROM sessions WHERE id='sess-with-folder'`).Scan(&dir, &proj); err != nil {
		t.Fatal(err)
	}
	if dir != "/work/proj" || proj != "myproj" {
		t.Fatalf("sess-with-folder dir/project = %q/%q", dir, proj)
	}
	var audit int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id='audit'`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if audit != 0 {
		t.Fatalf("audit 消息数 = %d, want 0", audit)
	}
	var crossClient int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM (SELECT id FROM messages WHERE client IN (?,?) GROUP BY id HAVING COUNT(DISTINCT client)>1)`,
		model.ClientClaudeCode, model.ClientClaudeDesktop).Scan(&crossClient); err != nil {
		t.Fatal(err)
	}
	if crossClient != 0 {
		t.Fatalf("跨 client 同 message.id 数 = %d, want 0", crossClient)
	}
	var ghost int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE client IN (?,?) AND first_ts=0 AND last_ts=0`,
		model.ClientClaudeCode, model.ClientClaudeDesktop).Scan(&ghost); err != nil {
		t.Fatal(err)
	}
	if ghost != 0 {
		t.Fatalf("首末 ts=0 的 Claude 会话行 = %d, want 0", ghost)
	}
}

// 映射晚到后的两表一致性：首次无映射入库后补写映射，重读同一 transcript 时
// 同一消息的时间戳不变，会话行与消息行的 directory/project 都必须补齐，否则
// 会话列表与按项目汇总的用量会长期不一致。
func TestRunCollect_CoworkLateMappingFillsMessagesAttribution(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	sessionDir := filepath.Join(profile, "localsess")
	proj := filepath.Join(sessionDir, ".claude", "projects", "session")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"cowork-s","timestamp":"2026-11-16T10:00:00+08:00","message":{"id":"late-msg","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "late-id.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	projectsDir := t.TempDir()
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	run := func() {
		t.Helper()
		res := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
			collector.CollectRequest{}, false, false)
		if !res.Complete() {
			t.Fatalf("RunCollect 失败: %+v", res)
		}
	}
	// 1) 无映射首采：会话与消息都没有 directory/project。
	run()
	// 2) 补写映射后重扫（映射晚到的真实时序）。
	mapping := `{"cliSessionId":"late-id","title":"晚到映射","userSelectedFolders":["/work/late"],"folderMountNames":{"/work/late":"late-proj"}}`
	if err := os.WriteFile(filepath.Join(profile, "local_localsess.json"), []byte(mapping), 0o600); err != nil {
		t.Fatal(err)
	}
	run()

	var sessDir, sessProj string
	if err := usageDB.QueryRow("SELECT directory, project FROM sessions WHERE id=?", "late-id").Scan(&sessDir, &sessProj); err != nil {
		t.Fatal(err)
	}
	if sessDir != "/work/late" || sessProj != "late-proj" {
		t.Fatalf("session attribution = %q/%q", sessDir, sessProj)
	}
	var msgDir, msgProj string
	if err := usageDB.QueryRow("SELECT directory, project FROM messages WHERE id=?", "late-msg").Scan(&msgDir, &msgProj); err != nil {
		t.Fatal(err)
	}
	if msgDir != sessDir || msgProj != sessProj {
		t.Fatalf("messages attribution = %q/%q, want %q/%q（两表必须同口径）", msgDir, msgProj, sessDir, sessProj)
	}
}

// 门命中（文件未变）时也必须修好消息归因：跳过门不重读 transcript，晚到的
// 映射只能经 UPDATE-only 通道落到 sessions 与 messages 两表。
func TestRunCollect_CoworkGateHitFillsMessagesAttribution(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	proj := filepath.Join(profile, "gatesess", ".claude", "projects", "session")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"cowork-g","timestamp":"2026-11-16T11:00:00+08:00","message":{"id":"gate-msg","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "gate-id.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	mapping := `{"cliSessionId":"gate-id","title":"门命中","userSelectedFolders":["/work/gate"],"folderMountNames":{"/work/gate":"gate-proj"}}`
	if err := os.WriteFile(filepath.Join(profile, "local_gatesess.json"), []byte(mapping), 0o600); err != nil {
		t.Fatal(err)
	}
	projectsDir := t.TempDir()
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	deps := NewDepsWithCollectors(cfg, []collector.Collector{collector.NewClaudeCollector(cfg)}, nil)
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	gateRun := func() {
		t.Helper()
		res := RunCollect(context.Background(), deps, usageDB, silentLogger(), io.Discard, "claude",
			collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true}, false, false)
		if !res.Complete() {
			t.Fatalf("采集失败: %+v", res)
		}
	}
	// 第一次：无门记录，全读并写门记录。
	gateRun()
	// 模拟「会话行有归属、消息行仍为空」的历史状态。
	if _, err := usageDB.Exec("UPDATE messages SET directory='', project='' WHERE id=?", "gate-msg"); err != nil {
		t.Fatal(err)
	}
	// 第二次：文件未变，门命中走 UPDATE-only。
	gateRun()
	var sessDir, sessProj, msgDir, msgProj string
	if err := usageDB.QueryRow("SELECT directory, project FROM sessions WHERE id=?", "gate-id").Scan(&sessDir, &sessProj); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.QueryRow("SELECT directory, project FROM messages WHERE id=?", "gate-msg").Scan(&msgDir, &msgProj); err != nil {
		t.Fatal(err)
	}
	if sessDir != "/work/gate" || sessProj != "gate-proj" {
		t.Fatalf("session attribution = %q/%q", sessDir, sessProj)
	}
	if msgDir != sessDir || msgProj != sessProj {
		t.Fatalf("gate-hit must fill message attribution: got %q/%q, want %q/%q", msgDir, msgProj, sessDir, sessProj)
	}
}

// 映射被删除后重读：会话与消息的 directory/project 必须一起清空。
// 旧实现只清会话行，消息仍保留旧项目，按项目汇总与会话列表长期分叉。
func TestRunCollect_CoworkMappingDeletedClearsBothTables(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	proj := filepath.Join(profile, "dsess", ".claude", "projects", "session")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"cowork-d","timestamp":"2026-11-16T10:00:00+08:00","message":{"id":"del-msg","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "del-id.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	mapping := filepath.Join(profile, "local_dsess.json")
	body := `{"cliSessionId":"del-id","userSelectedFolders":["/work/del"],"folderMountNames":{"/work/del":"del-proj"}}`
	if err := os.WriteFile(mapping, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projectsDir := t.TempDir()
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	run := func() {
		t.Helper()
		res := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
			collector.CollectRequest{}, false, false)
		if !res.Complete() {
			t.Fatalf("采集失败: %+v", res)
		}
	}
	run()
	if err := os.Remove(mapping); err != nil {
		t.Fatal(err)
	}
	run()

	var sessDir, sessProj, msgDir, msgProj string
	if err := usageDB.QueryRow("SELECT directory, project FROM sessions WHERE id=?", "del-id").Scan(&sessDir, &sessProj); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.QueryRow("SELECT directory, project FROM messages WHERE id=?", "del-msg").Scan(&msgDir, &msgProj); err != nil {
		t.Fatal(err)
	}
	if sessDir != "" || sessProj != "" {
		t.Fatalf("session attribution should be cleared: %q/%q", sessDir, sessProj)
	}
	if msgDir != "" || msgProj != "" {
		t.Fatalf("message attribution should follow session: %q/%q", msgDir, msgProj)
	}
}

// 首份 transcript 无用量但留下门记录，第二份带有效用量到达时，
// 必须由第二份建立会话行（而不是 1 条消息、0 条会话）。
func TestRunCollect_CoworkGateOnEmptyCopyDoesNotStealFirstSeen(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	root := roots[0]
	profile := filepath.Join(root, "acct", "profile")
	sessA := filepath.Join(profile, "emptysess", ".claude", "projects", "session")
	if err := os.MkdirAll(sessA, 0o755); err != nil {
		t.Fatal(err)
	}
	// 首份只有 user 行，不产生消息与会话。
	userLine := `{"type":"user","sessionId":"s","timestamp":"2026-11-16T09:00:00+08:00","message":{"id":"u-1","role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessA, "gate-empty.jsonl"), []byte(userLine), 0o600); err != nil {
		t.Fatal(err)
	}
	mapA := `{"cliSessionId":"gate-empty","title":"空首份"}`
	if err := os.WriteFile(filepath.Join(profile, "local_emptysess.json"), []byte(mapA), 0o600); err != nil {
		t.Fatal(err)
	}
	projectsDir := t.TempDir()
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projectsDir}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	gateRun := func() Result {
		t.Helper()
		return RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
			collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true}, false, false)
	}
	// 第一轮补采：留下门记录，不建会话。
	if res := gateRun(); !res.Complete() {
		t.Fatalf("首轮失败: %+v", res)
	}
	var sessionsAfterFirst int
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM sessions WHERE id=?", "gate-empty").Scan(&sessionsAfterFirst); err != nil {
		t.Fatal(err)
	}
	if sessionsAfterFirst != 0 {
		t.Fatalf("空 transcript 不应建立会话: %d", sessionsAfterFirst)
	}

	// 第二份同 cliSessionId 的 transcript 带有效用量到达。
	sessB := filepath.Join(profile, "datasess", ".claude", "projects", "session")
	if err := os.MkdirAll(sessB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessB, "gate-empty.jsonl"), []byte(`{"type":"assistant","sessionId":"s","timestamp":"2026-11-16T10:00:00+08:00","message":{"id":"only-msg","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mapB := `{"cliSessionId":"gate-empty","title":"有消息"}`
	if err := os.WriteFile(filepath.Join(profile, "local_datasess.json"), []byte(mapB), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := gateRun(); !res.Complete() {
		t.Fatalf("第二轮失败: %+v", res)
	}
	var msgs, sessions int
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages WHERE id=?", "only-msg").Scan(&msgs); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM sessions WHERE id=?", "gate-empty").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if msgs != 1 || sessions != 1 {
		t.Fatalf("应留下 1 条消息 + 1 条会话（否则用量无会话可关联）: messages=%d sessions=%d", msgs, sessions)
	}
}

// 首份 transcript 只有用户消息、首轮留下门记录；后续补采时组内已有多份
// 现存文件，必须整组重读并按第一份有用量的副本建立会话，空副本不能挡住会话行。
func TestRunCollect_CoworkGateHitOnEmptyCopyKeepsFirstSeenFree(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	sessA := filepath.Join(profile, "emptysess", ".claude", "projects", "session")
	if err := os.MkdirAll(sessA, 0o755); err != nil {
		t.Fatal(err)
	}
	userLine := `{"type":"user","sessionId":"s","timestamp":"2026-11-16T09:00:00+08:00","message":{"id":"u-1","role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessA, "gate-empty.jsonl"), []byte(userLine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "local_emptysess.json"), []byte(`{"cliSessionId":"gate-empty","title":"空首份"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": t.TempDir()}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	run := func() Result {
		t.Helper()
		return RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
			collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true}, false, false)
	}
	run() // 首轮：写入门记录，不建会话
	run() // 第二轮：同一文件命中门（不重读），空副本不建会话

	// 第三份同 cliSessionId 的 transcript 带有效用量到达。
	sessB := filepath.Join(profile, "datasess", ".claude", "projects", "session")
	if err := os.MkdirAll(sessB, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"s","timestamp":"2026-11-16T10:00:00+08:00","message":{"id":"only-msg","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessB, "gate-empty.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "local_datasess.json"), []byte(`{"cliSessionId":"gate-empty","title":"有消息"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := run(); !res.Complete() {
		t.Fatalf("第三轮失败: %+v", res)
	}
	var msgs, sessions int
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages WHERE id=?", "only-msg").Scan(&msgs); err != nil {
		t.Fatal(err)
	}
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM sessions WHERE id=?", "gate-empty").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if msgs != 1 || sessions != 1 {
		t.Fatalf("空副本不得抢占元数据来源: messages=%d sessions=%d", msgs, sessions)
	}
	// 元数据必须来自真正产出会话的那份副本（空副本的标题不得落地）。
	var title string
	if err := usageDB.QueryRow("SELECT title FROM sessions WHERE id=?", "gate-empty").Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "有消息" {
		t.Fatalf("会话标题应来自有消息的副本，got %q（空副本抢占了来源）", title)
	}
}

// 两份副本同时存在时，有效副本建立的会话元数据不得被无用量副本覆盖。
func TestRunCollect_CoworkGateCandidatesDoNotOverrideProducer(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	write := func(sessionDir, title, msgID string) {
		t.Helper()
		dir := filepath.Join(profile, sessionDir, ".claude", "projects", "session")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"cliSessionId":"dup-id","title":"` + title + `"}`
		if err := os.WriteFile(filepath.Join(profile, "local_"+sessionDir+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		content := ""
		if msgID != "" {
			content = `{"type":"assistant","sessionId":"s","timestamp":"2026-11-16T10:00:00+08:00","message":{"id":"` + msgID + `","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
		} else {
			content = `{"type":"user","sessionId":"s","timestamp":"2026-11-16T09:00:00+08:00","message":{"id":"u-1","role":"user","content":"hi"}}` + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "dup-id.jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 无用量副本命名靠前（遍历先到），有效副本在后。
	write("aaa", "无用量的 a", "")
	write("zzz", "有效副本 z", "valid-msg")
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": t.TempDir()}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	run := func() Result {
		t.Helper()
		return RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
			collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true}, false, false)
	}
	if res := run(); !res.Complete() {
		t.Fatalf("采集失败: %+v", res)
	}
	var title string
	if err := usageDB.QueryRow("SELECT title FROM sessions WHERE id=?", "dup-id").Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "有效副本 z" {
		t.Fatalf("会话标题应来自产出会话的副本，got %q", title)
	}
	// 第二次补采（门命中路径）也不得改动。
	if res := run(); !res.Complete() {
		t.Fatalf("补采失败: %+v", res)
	}
	if err := usageDB.QueryRow("SELECT title FROM sessions WHERE id=?", "dup-id").Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "有效副本 z" {
		t.Fatalf("补采后被无用量副本覆盖: %q", title)
	}
}

// 首份有映射无用量，第二份有用量无映射（靠文件名得到同一会话 id）。
// 组裁决以有用量的第二份为来源，首份映射的标题/项目不得落进会话行。
func TestRunCollect_CoworkGateCandidateCannotBorrowProducer(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	// 会话目录名 = transcript 文件名 = 同一 cliSessionId。
	dir := filepath.Join(profile, "borrow", ".claude", "projects", "session")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 首份：有映射、只有用户消息。映射 cliSessionId = shared-borrow。
	if err := os.WriteFile(filepath.Join(profile, "local_borrow.json"), []byte(`{"cliSessionId":"shared-borrow","title":"无用量首份"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	user := `{"type":"user","sessionId":"s","timestamp":"2026-11-16T09:00:00+08:00","message":{"id":"u","role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "shared-borrow.jsonl"), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": t.TempDir()}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	run := func() Result {
		t.Helper()
		return RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
			collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true}, false, false)
	}
	run()
	// 第二份：无映射、有用量，文件名 = shared-borrow。
	dir2 := filepath.Join(profile, "other", ".claude", "projects", "session")
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	assistant := `{"type":"assistant","sessionId":"s","timestamp":"2026-11-16T10:00:00+08:00","message":{"id":"m-borrow","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir2, "shared-borrow.jsonl"), []byte(assistant), 0o600); err != nil {
		t.Fatal(err)
	}
	run()
	run()
	var title string
	if err := usageDB.QueryRow("SELECT title FROM sessions WHERE id=?", "shared-borrow").Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "" {
		t.Fatalf("无用量副本的标题不得覆盖有效副本: %q", title)
	}
}

// 两份均有用量，首次全扫按字典序取首份；随后只追加第二份，下一轮
// 首份命中门、第二份重读时，元数据不得从首份切换到第二份。
func TestRunCollect_CoworkGateHitVsRereadKeepFirst(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	write := func(profileName, title string) string {
		t.Helper()
		pd := filepath.Join(roots[0], "acct", profileName)
		d := filepath.Join(pd, "sess", ".claude", "projects", "session")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pd, "local_sess.json"), []byte(`{"cliSessionId":"switch-id","title":"`+title+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d1 := write("first", "首份")
	d2 := write("second", "次份")
	line := `{"type":"assistant","sessionId":"s","timestamp":"2026-11-16T10:00:00+08:00","message":{"id":"%s","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(d1, "switch-id.jsonl"), []byte(fmt.Sprintf(line, "m-first")), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": t.TempDir()}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	run := func() {
		t.Helper()
		res := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
			collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true}, false, false)
		if !res.Complete() {
			t.Fatalf("采集失败: %+v", res)
		}
	}
	run()
	// 只追加第二份，第一份不再变化。
	if err := os.WriteFile(filepath.Join(d2, "switch-id.jsonl"), []byte(fmt.Sprintf(line, "m-second")), 0o600); err != nil {
		t.Fatal(err)
	}
	run()
	var title string
	if err := usageDB.QueryRow("SELECT title FROM sessions WHERE id=?", "switch-id").Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "首份" {
		t.Fatalf("门命中与重读必须共用同一裁决来源，标题不应从首份切换到次份: %q", title)
	}
}

// writeCoworkGroupTranscript 写入一份文件名即逻辑会话 ID 的 Cowork transcript。
// mappingID 非空时同时写会话目录映射 JSON；为空表示无映射、以文件名兜底。
func writeCoworkGroupTranscript(t *testing.T, profileDir, sessionDir, transcriptID, mappingID, title, folder, project, body string) string {
	t.Helper()
	proj := filepath.Join(profileDir, sessionDir, ".claude", "projects", "session")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if mappingID != "" {
		var b strings.Builder
		b.WriteString(`{"cliSessionId":"` + mappingID + `"`)
		if title != "" {
			b.WriteString(`,"title":"` + title + `"`)
		}
		if folder != "" {
			b.WriteString(`,"userSelectedFolders":["` + folder + `"],"folderMountNames":{"` + folder + `":"` + project + `"}`)
		}
		b.WriteString("}")
		if err := os.WriteFile(filepath.Join(profileDir, "local_"+sessionDir+".json"), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(proj, transcriptID+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// coworkGroupAssistantLine 返回一条带 usage 的 Cowork assistant 行。
func coworkGroupAssistantLine(sessionID, msgID, ts string) string {
	return `{"type":"assistant","sessionId":"` + sessionID + `","timestamp":"` + ts + `","message":{"id":"` + msgID + `","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
}

// coworkGroupUserLine 返回一条只有用户消息的行（不产出用量）。
func coworkGroupUserLine(ts string) string {
	return `{"type":"user","sessionId":"s","timestamp":"` + ts + `","message":{"id":"u-1","role":"user","content":"hi"}}` + "\n"
}

// 无用量首份与有用量、无映射（文件名兜底）副本共存。会话元数据
// 必须来自第一份实际产出用量的副本（这里是次份），首份映射的标题/项目不得
// 落地；消息项目与会话项目保持同一口径。
func TestRunCollect_CoworkGroupPicksFirstUsageCopy(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	// aaa 有映射但无用量；zzz 无映射、有用量。字典序 aaa 在前。
	writeCoworkGroupTranscript(t, profile, "aaa", "group-id", "group-id", "首份标题", "/work/a", "proj-a",
		coworkGroupUserLine("2026-11-16T09:00:00+08:00"))
	writeCoworkGroupTranscript(t, profile, "zzz", "group-id", "", "", "", "",
		coworkGroupAssistantLine("s", "m-z", "2026-11-16T10:00:00+08:00"))
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": t.TempDir()}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	res := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{}, false, false)
	if !res.Complete() {
		t.Fatalf("采集失败: %+v", res)
	}
	var title, dir, proj string
	if err := usageDB.QueryRow("SELECT title, directory, project FROM sessions WHERE id=? AND client=?",
		"group-id", model.ClientClaudeDesktop).Scan(&title, &dir, &proj); err != nil {
		t.Fatal(err)
	}
	if title != "" || dir != "" || proj != "" {
		t.Fatalf("会话元数据必须来自有用量但无映射的次份，got title=%q dir=%q proj=%q", title, dir, proj)
	}
	var msgDir, msgProj string
	if err := usageDB.QueryRow("SELECT directory, project FROM messages WHERE id=?", "m-z").Scan(&msgDir, &msgProj); err != nil {
		t.Fatal(err)
	}
	if msgDir != "" || msgProj != "" {
		t.Fatalf("消息项目必须与会话项目同口径，got dir=%q proj=%q", msgDir, msgProj)
	}
}

// 首份命中跳过门、次份重读（启动补采）。组裁决必须整组重读并按
// 确定顺序选首份，随后的重读不得把会话标题/项目切换到次份；消息项目同口径。
func TestRunCollect_CoworkGroupGateHitAndRereadUseSameSource(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	writeCoworkGroupTranscript(t, profile, "aaa", "group-gate", "group-gate", "首份标题", "/work/a", "proj-a",
		coworkGroupAssistantLine("s", "m-a", "2026-11-16T09:00:00+08:00"))
	zPath := writeCoworkGroupTranscript(t, profile, "zzz", "group-gate", "group-gate", "次份标题", "/work/z", "proj-z",
		coworkGroupAssistantLine("s", "m-z", "2026-11-16T10:00:00+08:00"))
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": t.TempDir()}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	run := func() {
		t.Helper()
		res := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
			collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true}, false, false)
		if !res.Complete() {
			t.Fatalf("采集失败: %+v", res)
		}
	}
	run()
	// 只追加次份（首份不再变化）：下一轮首份命中门，次份重读。
	body := coworkGroupAssistantLine("s", "m-z", "2026-11-16T10:00:00+08:00") +
		coworkGroupAssistantLine("s", "m-z2", "2026-11-16T11:00:00+08:00")
	if err := os.WriteFile(zPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	run()
	var title, dir, proj string
	if err := usageDB.QueryRow("SELECT title, directory, project FROM sessions WHERE id=?",
		"group-gate").Scan(&title, &dir, &proj); err != nil {
		t.Fatal(err)
	}
	if title != "首份标题" || dir != "/work/a" || proj != "proj-a" {
		t.Fatalf("门命中与重读必须共用同一来源，got title=%q dir=%q proj=%q", title, dir, proj)
	}
	var msgDir, msgProj string
	if err := usageDB.QueryRow("SELECT directory, project FROM messages WHERE id=?", "m-z2").Scan(&msgDir, &msgProj); err != nil {
		t.Fatal(err)
	}
	if msgDir != "/work/a" || msgProj != "proj-a" {
		t.Fatalf("次份重读的消息项目必须与会话项目一致，got dir=%q proj=%q", msgDir, msgProj)
	}
}

// 单独收到次份 ChangedFile。ChangedFile 必须按与全扫相同的组与
// 顺序核对，不能只读变化的这一份把会话元数据切到次份。
func TestRunCollect_CoworkGroupChangedFileMatchesFullScan(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	writeCoworkGroupTranscript(t, profile, "aaa", "group-changed", "group-changed", "首份标题", "/work/a", "proj-a",
		coworkGroupAssistantLine("s", "m-a", "2026-11-16T09:00:00+08:00"))
	zPath := writeCoworkGroupTranscript(t, profile, "zzz", "group-changed", "group-changed", "次份标题", "/work/z", "proj-z",
		coworkGroupAssistantLine("s", "m-z", "2026-11-16T10:00:00+08:00"))
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": t.TempDir()}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	res := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{ChangedFile: zPath}, false, false)
	if !res.Complete() {
		t.Fatalf("ChangedFile 采集失败: %+v", res)
	}
	var title, dir, proj string
	if err := usageDB.QueryRow("SELECT title, directory, project FROM sessions WHERE id=?",
		"group-changed").Scan(&title, &dir, &proj); err != nil {
		t.Fatal(err)
	}
	if title != "首份标题" || dir != "/work/a" || proj != "proj-a" {
		t.Fatalf("ChangedFile 必须与全扫选出同一来源，got title=%q dir=%q proj=%q", title, dir, proj)
	}
	var msgDir, msgProj string
	if err := usageDB.QueryRow("SELECT directory, project FROM messages WHERE id=?", "m-z").Scan(&msgDir, &msgProj); err != nil {
		t.Fatal(err)
	}
	if msgDir != "/work/a" || msgProj != "proj-a" {
		t.Fatalf("ChangedFile 的消息项目必须与会话项目一致，got dir=%q proj=%q", msgDir, msgProj)
	}
}

// 按日期采集不得重新选择副本。首份只有 11-16 用量、
// 次份只有 11-17 用量；全量采集后只采集 11-17，会话标题/项目与 11-16 已入库
// 的消息项目都必须保持首份口径，而不能被过滤后仍有消息的次份改写。
func TestRunCollect_CoworkGroupDateModeKeepsSource(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	writeCoworkGroupTranscript(t, profile, "aaa", "group-date", "group-date", "首份标题", "/work/a", "proj-a",
		coworkGroupAssistantLine("s", "m-a", "2026-11-16T09:00:00+08:00"))
	writeCoworkGroupTranscript(t, profile, "zzz", "group-date", "group-date", "次份标题", "/work/z", "proj-z",
		coworkGroupAssistantLine("s", "m-z", "2026-11-17T10:00:00+08:00"))
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": t.TempDir()}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	full := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true}, false, false)
	if !full.Complete() {
		t.Fatalf("全量采集失败: %+v", full)
	}
	// 只采集 11-17：首份在当日没有消息，来源仍必须是首份。
	day := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-11-17"}}, false, false)
	if !day.Complete() {
		t.Fatalf("日期采集失败: %+v", day)
	}
	var title, dir, proj string
	if err := usageDB.QueryRow("SELECT title, directory, project FROM sessions WHERE id=?",
		"group-date").Scan(&title, &dir, &proj); err != nil {
		t.Fatal(err)
	}
	if title != "首份标题" || dir != "/work/a" || proj != "proj-a" {
		t.Fatalf("日期采集不得重新选择副本，got title=%q dir=%q proj=%q", title, dir, proj)
	}
	var dayDir, dayProj string
	if err := usageDB.QueryRow("SELECT directory, project FROM messages WHERE id=?", "m-z").Scan(&dayDir, &dayProj); err != nil {
		t.Fatal(err)
	}
	if dayDir != "/work/a" || dayProj != "proj-a" {
		t.Fatalf("当日消息项目必须沿用首份口径，got dir=%q proj=%q", dayDir, dayProj)
	}
	var oldProj string
	if err := usageDB.QueryRow("SELECT project FROM messages WHERE id=?", "m-a").Scan(&oldProj); err != nil {
		t.Fatal(err)
	}
	if oldProj != "proj-a" {
		t.Fatalf("历史消息项目不得被日期采集改写，got %q", oldProj)
	}
}

// 空库首次只按日期采集。首份只有 11-16 用量、次份只有
// 11-17 用量；只采 11-17 时消息按日期入库，但元数据来源（首份）在当日没有
// 消息。必须用首份的完整会话骨架生成会话行，否则会话排行查不到这批用量。
func TestRunCollect_CoworkGroupDateModeEmptyDBWritesSession(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	writeCoworkGroupTranscript(t, profile, "aaa", "group-empty", "group-empty", "首份标题", "/work/a", "proj-a",
		coworkGroupAssistantLine("s", "m-a", "2026-11-16T09:00:00+08:00"))
	writeCoworkGroupTranscript(t, profile, "zzz", "group-empty", "group-empty", "次份标题", "/work/z", "proj-z",
		coworkGroupAssistantLine("s", "m-z", "2026-11-17T10:00:00+08:00"))
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": t.TempDir()}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	res := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
		collector.CollectRequest{Dates: []string{"2026-11-17"}}, false, false)
	if !res.Complete() {
		t.Fatalf("日期采集失败: %+v", res)
	}
	var title, dir, proj string
	var first, last int64
	if err := usageDB.QueryRow("SELECT title, directory, project, first_ts, last_ts FROM sessions WHERE id=?",
		"group-empty").Scan(&title, &dir, &proj, &first, &last); err != nil {
		t.Fatalf("空库日期采集必须写入会话行: %v", err)
	}
	if title != "首份标题" || dir != "/work/a" || proj != "proj-a" {
		t.Fatalf("会话行必须使用首份副本的元数据，got title=%q dir=%q proj=%q", title, dir, proj)
	}
	if first == 0 || last == 0 {
		t.Fatalf("会话行首末时间必须来自完整 transcript，got %d/%d", first, last)
	}
	var msgDir, msgProj string
	if err := usageDB.QueryRow("SELECT directory, project FROM messages WHERE id=?", "m-z").Scan(&msgDir, &msgProj); err != nil {
		t.Fatal(err)
	}
	if msgDir != "/work/a" || msgProj != "proj-a" {
		t.Fatalf("当日消息项目必须与会话行一致，got dir=%q proj=%q", msgDir, msgProj)
	}
	// 会话排行按 sessions JOIN messages 查询：必须能关联到这条用量。
	var joined int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM sessions s JOIN messages m
		ON m.session_id = s.id AND m.client = s.client WHERE s.id = ?`, "group-empty").Scan(&joined); err != nil {
		t.Fatal(err)
	}
	if joined != 1 {
		t.Fatalf("会话排行 JOIN 行数 = %d, want 1（会话行缺失会让用量查不到）", joined)
	}
	// 日期外消息不得写入。
	var oldMsgs int
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages WHERE id=?", "m-a").Scan(&oldMsgs); err != nil {
		t.Fatal(err)
	}
	if oldMsgs != 0 {
		t.Fatalf("日期外消息不得写入: %d", oldMsgs)
	}
}

// 同 ID 首份副本的映射暂时损坏时，健康但无映射的次份不得接管归因。
// 全扫与 ChangedFile 都必须整组冻结并走部分失败，库中已有会话与历史消息的
// directory/project 保持原值；映射修复后下一轮全扫恢复采集。
func TestRunCollect_CoworkUnverifiableCopyKeepsHistoricalAttribution(t *testing.T) {
	for _, fault := range []string{"corrupt_mapping", "unreadable_profile", "unreadable_transcript"} {
		t.Run(fault, func(t *testing.T) {
			testCoworkUnverifiableCopyKeepsHistoricalAttribution(t, fault)
		})
	}
}

func testCoworkUnverifiableCopyKeepsHistoricalAttribution(t *testing.T, fault string) {
	t.Helper()
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	firstProfile := filepath.Join(roots[0], "acct", "profile-a")
	secondProfile := filepath.Join(roots[0], "acct", "profile-b")
	firstPath := writeCoworkGroupTranscript(t, firstProfile, "sess-a", "shared-corrupt", "shared-corrupt", "首份标题", "/work/a", "proj-a",
		coworkGroupAssistantLine("s", "m-a", "2026-11-16T09:00:00+08:00"))
	secondPath := writeCoworkGroupTranscript(t, secondProfile, "sess-b", "shared-corrupt", "", "", "", "",
		coworkGroupAssistantLine("s", "m-b", "2026-11-16T10:00:00+08:00"))
	projects := t.TempDir()
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projects}},
	}}
	usageDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()
	fullReq := collector.CollectRequest{Source: collector.CollectSourceClient, ScanExistingJSONL: true}
	full := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude", fullReq, false, false)
	if !full.Complete() {
		t.Fatalf("首轮全扫失败: %+v", full)
	}
	assertAttribution := func(tt *testing.T, want string) {
		tt.Helper()
		var sessProj string
		if err := usageDB.QueryRow("SELECT project FROM sessions WHERE id=?", "shared-corrupt").Scan(&sessProj); err != nil {
			tt.Fatal(err)
		}
		if sessProj != want {
			tt.Fatalf("会话 project = %q, want %q", sessProj, want)
		}
		for _, id := range []string{"m-a", "m-b"} {
			var p string
			if err := usageDB.QueryRow("SELECT project FROM messages WHERE id=?", id).Scan(&p); err != nil {
				tt.Fatal(err)
			}
			if p != want {
				tt.Fatalf("消息 %s project = %q, want %q", id, p, want)
			}
		}
	}
	assertAttribution(t, "proj-a")

	var restore func()
	switch fault {
	case "corrupt_mapping":
		mapping := filepath.Join(firstProfile, "local_sess-a.json")
		original, err := os.ReadFile(mapping)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mapping, []byte("not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		restore = func() {
			if err := os.WriteFile(mapping, original, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	case "unreadable_profile", "unreadable_transcript":
		blocked, mode := firstPath, fs.FileMode(0o600)
		if fault == "unreadable_profile" {
			blocked, mode = firstProfile, 0o755
		}
		if err := os.Chmod(blocked, 0); err != nil {
			t.Skipf("无法注入读取权限故障: %v", err)
		}
		restore = func() {
			if err := os.Chmod(blocked, mode); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(restore)
		var readErr error
		if fault == "unreadable_profile" {
			_, readErr = os.ReadDir(blocked)
		} else {
			_, readErr = os.ReadFile(blocked)
		}
		if !errors.Is(readErr, fs.ErrPermission) {
			t.Skipf("当前环境没有产生读取权限失败: %v", readErr)
		}
	}
	f, err := os.OpenFile(secondPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(coworkGroupAssistantLine("s", "m-b2", "2026-11-16T11:00:00+08:00")); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	nativeFile := filepath.Join(projects, "fresh.jsonl")
	if err := os.WriteFile(nativeFile, []byte(coworkGroupAssistantLine("native", "m-native", "2026-11-16T12:00:00+08:00")), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, req := range []collector.CollectRequest{{ChangedFile: secondPath}, fullReq} {
		broken := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude", req, false, false)
		if broken.Complete() {
			t.Fatalf("来源无法确认时必须返回部分失败: %+v", req)
		}
		assertAttribution(t, "proj-a")
		var frozen int
		if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages WHERE id=?", "m-b2").Scan(&frozen); err != nil {
			t.Fatal(err)
		}
		if frozen != 0 {
			t.Fatalf("冻结组不得写入新消息: m-b2=%d", frozen)
		}
	}

	var nativeCount int
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM messages WHERE id=? AND client=?", "m-native", model.ClientClaudeCode).Scan(&nativeCount); err != nil {
		t.Fatal(err)
	}
	if nativeCount != 1 {
		t.Fatalf("主根消息应在部分失败批次中正常入库: %d", nativeCount)
	}

	restore()
	recovered := RunCollect(context.Background(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude", fullReq, false, false)
	if !recovered.Complete() {
		t.Fatalf("映射修复后必须完整成功: %+v", recovered)
	}
	assertAttribution(t, "proj-a")
	var recoveredMsg, msgProj string
	if err := usageDB.QueryRow("SELECT id, project FROM messages WHERE id=?", "m-b2").Scan(&recoveredMsg, &msgProj); err != nil {
		t.Fatalf("映射修复后追加消息必须入库: %v", err)
	}
	if recoveredMsg != "m-b2" || msgProj != "proj-a" {
		t.Fatalf("追加消息 = %q/%q, want m-b2/proj-a", recoveredMsg, msgProj)
	}
}

func TestRunCollect_CoworkAliasKeepsNativeProjectsOwnership(t *testing.T) {
	isolateClaudeDesktopEnv(t)
	roots := currentCoworkRoots()
	if len(roots) == 0 {
		t.Skip("当前平台无 Cowork 默认根")
	}
	nativeHome := t.TempDir()
	projects := filepath.Join(nativeHome, "projects")
	nativeDir := filepath.Join(projects, "session")
	if err := os.MkdirAll(nativeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	nativeFile := filepath.Join(nativeDir, "native.jsonl")
	if err := os.WriteFile(nativeFile, []byte(coworkGroupAssistantLine("native", "m-native", "2026-11-16T09:00:00+08:00")), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(roots[0], "acct", "profile")
	sessionDir := filepath.Join(profile, "sess")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(sessionDir, ".claude")
	if err := os.Symlink(nativeHome, alias); err != nil {
		t.Skipf("无法创建目录链接: %v", err)
	}
	if err := os.WriteFile(filepath.Join(profile, "local_sess.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projects}},
	}}
	for _, entry := range []struct {
		name string
		path string
	}{
		{name: "full"},
		{name: "native", path: nativeFile},
		{name: "alias", path: filepath.Join(alias, "projects", "session", "native.jsonl")},
	} {
		t.Run(entry.name, func(t *testing.T) {
			usageDB, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer usageDB.Close()
			result := RunCollect(t.Context(), NewDeps(cfg), usageDB, silentLogger(), io.Discard, "claude",
				collector.CollectRequest{ChangedFile: entry.path}, false, false)
			if !result.Complete() {
				t.Fatalf("主根文件应完整采集: %+v", result)
			}
			var client string
			if err := usageDB.QueryRow("SELECT client FROM messages WHERE id=?", "m-native").Scan(&client); err != nil {
				t.Fatal(err)
			}
			if client != model.ClientClaudeCode {
				t.Fatalf("消息 client = %q，期望 %q", client, model.ClientClaudeCode)
			}
		})
	}
}
