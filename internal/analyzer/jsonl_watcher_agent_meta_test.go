// internal/analyzer/jsonl_watcher_agent_meta_test.go
package analyzer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// === Claude agent meta.json → JSONL 事件映射 ===
//
// meta 晚于 JSONL 落盘时，watcher 必须把 agent-<id>.meta.json 的 Create/Write
// 事件映射到同目录 agent-<id>.jsonl 触发采集，使标题无需重启即可补齐。

// newAgentMetaWatcher 在 root 上启动一个指定 client 的 watcher，返回提交通道。
// 就绪经 signalReady seam 精确等待，不用固定 sleep（避免慢 CI 下 flaky）。
func newAgentMetaWatcher(t *testing.T, root, client string) (<-chan collector.CollectRequest, func()) {
	t.Helper()
	requests := make(chan collector.CollectRequest, 8)
	watcher, err := NewJSONLWatcher(
		[]string{root}, nil, client,
		50*time.Millisecond,
		func(_ string, req collector.CollectRequest) { requests <- req },
		nil,
	)
	if err != nil {
		t.Fatalf("NewJSONLWatcher: %v", err)
	}
	// 测试的监控根即采集根：受控判定以它为边界（与生产 analyzer 装配一致——
	// 生产根集合由 collector.ClaudeProjectsRoots 规范为绝对形态，这里同样先
	// Abs，使相对路径输入的行为与生产一致）。
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("Abs(%q): %v", root, err)
	}
	watcher.SetClaudeAgentRoots([]string{absRoot}, func() []string { return []string{absRoot} })
	ready := make(chan struct{})
	watcher.signalReady = func() { close(ready) }
	// 总超时需覆盖最长用例的多次等待窗口（含 debounce），留足慢 CI 余量。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	go watcher.Run(ctx)
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("watcher 未在超时内就绪")
	}
	stop := func() {
		cancel()
		watcher.Stop()
	}
	return requests, stop
}

// waitForChangedFile 在 timeout 内等待指定 ChangedFile 的提交。
func waitForChangedFile(t *testing.T, requests <-chan collector.CollectRequest, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case req := <-requests:
			if req.ChangedFile == want {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// expectNoRequests 断言等待窗口内没有任何提交（窗口内出现即失败）。
func expectNoRequests(t *testing.T, requests <-chan collector.CollectRequest, wait time.Duration) {
	t.Helper()
	select {
	case req := <-requests:
		t.Fatalf("不应有提交，got %q", req.ChangedFile)
	case <-time.After(wait):
	}
}

// drainRequests 清空等待窗口内已到达的提交。
func drainRequests(requests <-chan collector.CollectRequest, wait time.Duration) {
	deadline := time.After(wait)
	for {
		select {
		case <-requests:
		case <-deadline:
			return
		}
	}
}

// 只写 meta、JSONL 不变 → meta 事件映射到 JSONL 触发采集。
func TestJSONLWatcher_ClaudeAgentMetaMapsToJSONL(t *testing.T) {
	root := t.TempDir()
	subagentsDir := filepath.Join(root, "-proj", "parent-uuid", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	requests, stop := newAgentMetaWatcher(t, root, "claude")
	defer stop()

	jsonlPath := filepath.Join(subagentsDir, "agent-aaaaaaaaaaaaaaaaa.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"type":"assistant"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForChangedFile(t, requests, jsonlPath, 800*time.Millisecond) {
		t.Fatalf("JSONL 自身创建事件应触发采集")
	}
	drainRequests(requests, 150*time.Millisecond)

	// JSONL 不再变化，只写 meta。
	metaPath := filepath.Join(subagentsDir, "agent-aaaaaaaaaaaaaaaaa.meta.json")
	if err := os.WriteFile(metaPath, []byte(`{"description":"晚到标题"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForChangedFile(t, requests, jsonlPath, 800*time.Millisecond) {
		t.Fatalf("meta 事件应映射到 JSONL 触发采集")
	}
}

// workflows/<runId>/ 下的 meta 同样映射。
func TestJSONLWatcher_ClaudeWorkflowAgentMetaMapsToJSONL(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "-proj", "parent-uuid", "subagents", "workflows", "run-1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	requests, stop := newAgentMetaWatcher(t, root, "claude")
	defer stop()

	jsonlPath := filepath.Join(runDir, "agent-bbbbbbbbbbbbbbbbb.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"type":"assistant"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForChangedFile(t, requests, jsonlPath, 800*time.Millisecond) {
		t.Fatalf("JSONL 创建事件应触发采集")
	}
	drainRequests(requests, 150*time.Millisecond)

	metaPath := filepath.Join(runDir, "agent-bbbbbbbbbbbbbbbbb.meta.json")
	if err := os.WriteFile(metaPath, []byte(`{"description":"工作流晚到标题"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForChangedFile(t, requests, jsonlPath, 800*time.Millisecond) {
		t.Fatalf("workflow meta 事件应映射到 JSONL 触发采集")
	}
}

// 对应 JSONL 不存在时 meta 事件不触发（JSONL 自身的创建事件是唯一入口，避免
// 提交不存在的文件形成采集失败记录）。
func TestJSONLWatcher_ClaudeAgentMetaWithoutJSONLNoTrigger(t *testing.T) {
	root := t.TempDir()
	subagentsDir := filepath.Join(root, "-proj", "parent-uuid", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	requests, stop := newAgentMetaWatcher(t, root, "claude")
	defer stop()

	metaPath := filepath.Join(subagentsDir, "agent-ccccccccccccccccc.meta.json")
	if err := os.WriteFile(metaPath, []byte(`{"description":"孤儿 meta"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	expectNoRequests(t, requests, 600*time.Millisecond)
}

// 非 agent meta 形态（custom-title.json、workflows/wf_*.json、journal.json 等）
// 不触发映射。
func TestJSONLWatcher_NonAgentMetaIgnored(t *testing.T) {
	root := t.TempDir()
	subagentsDir := filepath.Join(root, "-proj", "parent-uuid", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 让 agent JSONL 存在：若映射错误地匹配非 agent meta，会对某个 JSONL 触发。
	if err := os.WriteFile(filepath.Join(subagentsDir, "agent-ddddddddddddddddd.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests, stop := newAgentMetaWatcher(t, root, "claude")
	defer stop()
	drainRequests(requests, 150*time.Millisecond)

	for name, content := range map[string]string{
		filepath.Join(subagentsDir, "custom-title.json"):                      `{"title":"x"}`,
		filepath.Join(subagentsDir, "random.meta.json"):                       `{"description":"非 agent 名"}`,
		filepath.Join(subagentsDir, "journal.json"):                           `{}`,
		filepath.Join(root, "-proj", "parent-uuid", "workflows", "wf_x.json"): `{}`,
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	expectNoRequests(t, requests, 600*time.Millisecond)
}

// 非 claude client 的 watcher 不启用 meta 映射（映射是 Claude 采集链专属语义）。
func TestJSONLWatcher_NonClaudeClientAgentMetaIgnored(t *testing.T) {
	root := t.TempDir()
	subagentsDir := filepath.Join(root, "-proj", "parent-uuid", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(subagentsDir, "agent-eeeeeeeeeeeeeeeee.jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests, stop := newAgentMetaWatcher(t, root, "workbuddy")
	defer stop()
	// 建立期间 JSONL 事件已提交，清空。
	drainRequests(requests, 300*time.Millisecond)

	metaPath := filepath.Join(subagentsDir, "agent-eeeeeeeeeeeeeeeee.meta.json")
	if err := os.WriteFile(metaPath, []byte(`{"description":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	expectNoRequests(t, requests, 600*time.Millisecond)
}

// 受控布局外的 .jsonl 事件不触发采集：journal.jsonl 与嵌套 subagents 下的
// agent 形态文件不属于受控范围（与全扫和 ChangedFile 判定一致）。
func TestJSONLWatcher_UncontrolledSubagentsJSONLIgnored(t *testing.T) {
	root := t.TempDir()
	subagentsDir := filepath.Join(root, "-proj", "parent-uuid", "subagents")
	nested := filepath.Join(subagentsDir, "x", "subagents")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	requests, stop := newAgentMetaWatcher(t, root, "claude")
	defer stop()

	// 受控文件作为哨兵：若过滤错误地放行非受控文件，会收到额外提交。
	sentinel := filepath.Join(subagentsDir, "agent-fffffffffffffffff.jsonl")
	if err := os.WriteFile(sentinel, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForChangedFile(t, requests, sentinel, 800*time.Millisecond) {
		t.Fatalf("受控 agent JSONL 创建事件应触发采集")
	}
	drainRequests(requests, 150*time.Millisecond)

	if err := os.WriteFile(filepath.Join(subagentsDir, "journal.jsonl"), []byte(`{"type":"started"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "agent-ggggggggggggggggg.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectNoRequests(t, requests, 600*time.Millisecond)
}

// 采集根本身位于名为 subagents 的目录下：主会话与 agent 的 .jsonl 事件都
// 正常触发采集（受控判定以采集根为边界，根祖先目录名不影响）。
func TestJSONLWatcher_RootUnderSubagentsDir(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "subagents", "projects")
	subagentsDir := filepath.Join(root, "-proj", "parent-uuid", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	requests, stop := newAgentMetaWatcher(t, root, "claude")
	defer stop()

	mainJSONL := filepath.Join(root, "-proj", "parent-uuid.jsonl")
	if err := os.WriteFile(mainJSONL, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForChangedFile(t, requests, mainJSONL, 800*time.Millisecond) {
		t.Fatalf("根祖先含 subagents 时主会话事件应触发采集")
	}

	agentJSONL := filepath.Join(subagentsDir, "agent-hhhhhhhhhhhhhhhhh.jsonl")
	if err := os.WriteFile(agentJSONL, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForChangedFile(t, requests, agentJSONL, 800*time.Millisecond) {
		t.Fatalf("根祖先含 subagents 时 agent 事件应触发采集")
	}
	drainRequests(requests, 150*time.Millisecond)

	// 受控边界仍生效：根祖先含 subagents 时，subagents 下的 journal.jsonl
	// 仍属非受控文件，不触发采集。
	if err := os.WriteFile(filepath.Join(subagentsDir, "journal.jsonl"), []byte(`{"type":"started"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectNoRequests(t, requests, 600*time.Millisecond)
}

// 运行期动态接管新目录后刷新受控判定根集合：初始根集合不含新账号 projects
// 目录时事件被拒绝，接管触发 refresh 后 JSONL 与 meta 事件均正常触发采集。
func TestJSONLWatcher_RefreshAgentRootsOnTakeover(t *testing.T) {
	base := t.TempDir()
	accountsRoot := filepath.Join(base, "claude-accounts")
	if err := os.MkdirAll(accountsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	accountProjects := filepath.Join(accountsRoot, "acct", "config", "projects")
	requests := make(chan collector.CollectRequest, 8)
	watcher, err := NewJSONLWatcher([]string{accountsRoot}, nil, "claude", 50*time.Millisecond,
		func(_ string, req collector.CollectRequest) { requests <- req }, nil)
	if err != nil {
		t.Fatalf("NewJSONLWatcher: %v", err)
	}
	// 初始根集合只含账号根（相对它是 7 段、非受控）；refresh 返回账号 projects 根。
	watcher.SetClaudeAgentRoots([]string{accountsRoot}, func() []string { return []string{accountProjects} })
	ready := make(chan struct{})
	watcher.signalReady = func() { close(ready) }
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	go watcher.Run(ctx)
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("watcher 未在超时内就绪")
	}
	defer func() {
		cancel()
		watcher.Stop()
	}()

	// 动态创建账号目录树：Create 事件触发接管与根集合刷新。反复写探测文件
	// 直到收到其提交，确定性地等待「接管完成 + 根集合刷新生效」（接管前写入
	// 可能无事件；refresh 未生效时探测文件事件会被受控判定拒绝）。
	subagentsDir := filepath.Join(accountProjects, "-proj", "parent-refresh", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(subagentsDir, "agent-probe0000000000000.jsonl")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := os.WriteFile(probe, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if waitForChangedFile(t, requests, probe, 100*time.Millisecond) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("动态接管未完成：探测文件事件未触发（根集合刷新未生效？）")
		}
	}
	drainRequests(requests, 150*time.Millisecond)
	jsonlPath := filepath.Join(subagentsDir, "agent-rrrrrrrrrrrrrrrrr.jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForChangedFile(t, requests, jsonlPath, 800*time.Millisecond) {
		t.Fatalf("刷新后 agent JSONL 事件应触发采集")
	}
	drainRequests(requests, 150*time.Millisecond)

	// meta 映射依赖受控判定：未刷新时会被拒绝，刷新后应映射成功。
	metaPath := filepath.Join(subagentsDir, "agent-rrrrrrrrrrrrrrrrr.meta.json")
	if err := os.WriteFile(metaPath, []byte(`{"description":"刷新后标题"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForChangedFile(t, requests, jsonlPath, 800*time.Millisecond) {
		t.Fatalf("刷新后 meta 事件应映射到 JSONL 触发采集")
	}
}

// 相对 projects_dir：watcher 监控根与受控判定根统一为绝对形态，agent 的
// .jsonl 事件与 meta 映射都正常触发采集（防止相对采集根与绝对事件路径再次
// 形态不一致，导致运行期 agent 的标题与父归属丢失）。
func TestJSONLWatcher_RelativeProjectsDir(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	root := "relative-projects"
	subagentsDir := filepath.Join(root, "-proj", "parent-uuid", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	requests, stop := newAgentMetaWatcher(t, root, "claude")
	defer stop()

	absSubagents, err := filepath.Abs(subagentsDir)
	if err != nil {
		t.Fatal(err)
	}
	agentJSONL := filepath.Join(absSubagents, "agent-qqqqqqqqqqqqqqqqq.jsonl")
	if err := os.WriteFile(agentJSONL, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForChangedFile(t, requests, agentJSONL, 800*time.Millisecond) {
		t.Fatalf("相对 projects_dir 下 agent 事件应触发采集")
	}
	drainRequests(requests, 150*time.Millisecond)

	metaPath := strings.TrimSuffix(agentJSONL, ".jsonl") + ".meta.json"
	if err := os.WriteFile(metaPath, []byte(`{"description":"相对根标题"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForChangedFile(t, requests, agentJSONL, 800*time.Millisecond) {
		t.Fatalf("相对 projects_dir 下 meta 事件应映射到 JSONL")
	}
}
