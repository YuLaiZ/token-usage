// internal/control/lease_test.go
//
// lease.go 平台无关部分测试：
//   - GenerateInstanceID（非空、唯一、十六进制）
//   - FilterLeaseEnvVars（清除三项内部变量，保留其余）
//   - BuildChildEnv（先过滤后追加本次值）
//   - parseParentLeaseWith（平台组合校验：零散/平台不匹配一律忽略）
//   - leaseStateMachine（EOF 与 daemon-lock-commit 单一互斥状态机，回调恰好一次）
package control

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ---- instanceID 生成 ----

func TestGenerateInstanceID_NonEmptyAndUnique(t *testing.T) {
	a := GenerateInstanceID()
	b := GenerateInstanceID()
	if a == "" || b == "" {
		t.Fatal("instanceID 不应为空")
	}
	if a == b {
		t.Fatal("两次生成应不同（一次性标识）")
	}
	if len(a) < 16 {
		t.Errorf("instanceID 长度应足够（>=16 字符），实际 %d", len(a))
	}
}

func TestGenerateInstanceID_Hexish(t *testing.T) {
	id := GenerateInstanceID()
	for _, r := range id {
		ok := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !ok {
			t.Fatalf("instanceID %q 含非十六进制字符 %q", id, r)
		}
	}
}

// ---- env 过滤 ----

func TestFilterLeaseEnvVars_RemovesAllThree(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		envInstance + "=STALE",
		"FOO=bar",
		envLeaseFD + "=99",
		envLeaseHandle + "=12345",
		"HOME=/root",
	}
	out := FilterLeaseEnvVars(in)
	for _, kv := range out {
		if strings.HasPrefix(kv, envInstance+"=") ||
			strings.HasPrefix(kv, envLeaseFD+"=") ||
			strings.HasPrefix(kv, envLeaseHandle+"=") {
			t.Errorf("过滤后仍残留内部 lease 变量: %q", kv)
		}
	}
	if !containsKV(out, "PATH=/usr/bin") || !containsKV(out, "FOO=bar") || !containsKV(out, "HOME=/root") {
		t.Errorf("非内部变量应保留，out=%v", out)
	}
}

func TestFilterLeaseEnvVars_PreservesOrderOfOthers(t *testing.T) {
	in := []string{"A=1", envInstance + "=x", "B=2", "C=3"}
	out := FilterLeaseEnvVars(in)
	want := []string{"A=1", "B=2", "C=3"}
	if len(out) != len(want) {
		t.Fatalf("长度错 out=%v want=%v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("idx %d: out=%q want=%q", i, out[i], want[i])
		}
	}
}

func TestFilterLeaseEnvVars_EmptyInput(t *testing.T) {
	out := FilterLeaseEnvVars(nil)
	if len(out) != 0 {
		t.Errorf("nil 输入应返回空，out=%v", out)
	}
}

func containsKV(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// ---- BuildChildEnv ----

func TestBuildChildEnv_FiltersOldAndAppendsNew(t *testing.T) {
	parentEnv := []string{
		"PATH=/usr/bin",
		envInstance + "=OLD", // 残留
		envLeaseFD + "=7",
		"HOME=/root",
	}
	// POSIX 追加 LEASE_FD=3（i=0 → 3+0=3）。
	out := BuildChildEnv(parentEnv, "abc123", func(env []string) []string {
		return append(env, envLeaseFD+"=3")
	})
	if !containsKV(out, "PATH=/usr/bin") || !containsKV(out, "HOME=/root") {
		t.Errorf("非内部变量应保留 out=%v", out)
	}
	if !containsKV(out, envInstance+"=abc123") {
		t.Errorf("应写入新 instance，out=%v", out)
	}
	if !containsKV(out, envLeaseFD+"=3") {
		t.Errorf("应写入 lease fd，out=%v", out)
	}
	if containsKV(out, envInstance+"=OLD") {
		t.Errorf("旧 instance 残留 out=%v", out)
	}
	if containsKV(out, envLeaseFD+"=7") {
		t.Errorf("旧 fd 残留 out=%v", out)
	}
}

// ---- ParseParentLease：组合校验 ----
//
// 平台专属字段（fd / handle）解析依赖 leaseReaderFromEnv（lease_unix.go /
// lease_windows.go）。本文件只断言平台无关的「instanceID 必须存在且与平台匹配的
// lease 标识同时出现」逻辑——非法/零散/平台不匹配一律 ok=false。
// 用 parseParentLeaseWith + 可注入 platformValidator 隔离平台实现。

func TestParseParentLease_NoInstanceID_Fails(t *testing.T) {
	in := []string{envLeaseFD + "=3", "PATH=/usr/bin"}
	_, ok := parseParentLeaseWith(in, fakePlatformValidator{ok: false})
	if ok {
		t.Error("没有 instanceID 时 ok 必须为 false（走独立加锁路径）")
	}
}

func TestParseParentLease_OnlyInstanceID_Fails(t *testing.T) {
	// 只有 instanceID、没有平台匹配的 lease 标识 → 视为零散变量，忽略。
	in := []string{envInstance + "=abc", "PATH=/usr/bin"}
	_, ok := parseParentLeaseWith(in, fakePlatformValidator{ok: false})
	if ok {
		t.Error("只有 instanceID 没有 lease 标识时 ok 必须为 false")
	}
}

func TestParseParentLease_PlatformMismatch_Fails(t *testing.T) {
	// POSIX fd 出现在 Windows 平台（fakePlatformValidator.ok=false）→ 忽略。
	in := []string{envInstance + "=abc", envLeaseFD + "=3"}
	_, ok := parseParentLeaseWith(in, fakePlatformValidator{ok: false})
	if ok {
		t.Error("平台不匹配的 lease 组合应 ok=false")
	}
}

func TestParseParentLease_ValidPlatformCombo_Succeeds(t *testing.T) {
	in := []string{envInstance + "=abc", envLeaseFD + "=4", "PATH=/usr/bin"}
	desc, ok := parseParentLeaseWith(in, fakePlatformValidator{ok: true})
	if !ok {
		t.Fatal("合法平台组合应 ok=true")
	}
	if desc.InstanceID != "abc" {
		t.Errorf("InstanceID=%q want abc", desc.InstanceID)
	}
}

// fakePlatformValidator 测试用：固定 ok 结果，模拟 leaseReaderFromEnv 的平台匹配。
type fakePlatformValidator struct {
	ok bool
}

func (f fakePlatformValidator) resolve(env []string) (leaseReader, bool) {
	return fakeLeaseReader{}, f.ok
}

// ---- leaseStateMachine ----
//
// 这是 核心：EOF 与 daemon-lock-commit 由单一互斥状态机处理，回调恰好一次。
// 两个关键顺序：
//   - daemon lock 先获得 + EOF 后 → commit 恰好一次，EOF 不触发取消
//   - EOF 先发生 + daemon lock 未获得 → 取消（LeaseLost 关闭），commit 不调用

func TestLeaseStateMachine_DaemonLockFirstThenEOF(t *testing.T) {
	sm := newLeaseStateMachine()
	commitCalls := int32(0)
	onCommit := func() { atomic.AddInt32(&commitCalls, 1) }

	// daemon lock 先获得 → commit。
	sm.markDaemonLockCommitted(onCommit)

	// 随后 EOF：child 已接管，不应触发取消。
	sm.notifyEOF()

	if atomic.LoadInt32(&commitCalls) != 1 {
		t.Errorf("OnDaemonLockCommit 应恰好 1 次，实际 %d", commitCalls)
	}
	select {
	case <-sm.leaseLost():
		t.Error("daemon lock 已获得后 EOF 不应触发 leaseLost")
	default:
	}
}

func TestLeaseStateMachine_EOFFirstBeforeDaemonLock(t *testing.T) {
	sm := newLeaseStateMachine()
	commitCalls := int32(0)
	onCommit := func() { atomic.AddInt32(&commitCalls, 1) }

	// EOF 先发生（父 lease 消失）。
	sm.notifyEOF()

	// 随后即使 daemon lock「获得」也不应 commit（取消优先）。
	sm.markDaemonLockCommitted(onCommit)

	if atomic.LoadInt32(&commitCalls) != 0 {
		t.Errorf("EOF 先到时不应调用 OnDaemonLockCommit，实际 %d", commitCalls)
	}
	select {
	case <-sm.leaseLost():
		// 期望 leaseLost 已关闭。
	default:
		t.Error("EOF 先到应触发 leaseLost")
	}
	// isCancelled 语义必须与 leaseLostCh 状态一致：EOF 先到（即使 markDaemonLockCommitted
	// 也被调用过）→ isCancelled() 应为 true。这锁定 Minor 7 修复：EOF 先到时不设 committed。
	if !sm.isCancelled() {
		t.Error("EOF 先到时 isCancelled 应为 true（与 leaseLostCh 已关闭一致）")
	}
}

func TestLeaseStateMachine_EOFConcurrentWithCommit(t *testing.T) {
	// 并发：markDaemonLockCommitted 与 notifyEOF 同时跑。
	// 不变量：onCommit 至多调用一次（互斥状态机保证）。
	// leaseLost 只在「EOF 先到 + 未 commit」时关闭；若 commit 先到则不关闭。
	// 这里用非阻塞 select 探测 leaseLost，避免阻塞。
	const n = 100
	for i := 0; i < n; i++ {
		sm := newLeaseStateMachine()
		commitCalls := int32(0)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			sm.markDaemonLockCommitted(func() { atomic.AddInt32(&commitCalls, 1) })
		}()
		go func() {
			defer wg.Done()
			sm.notifyEOF()
		}()
		wg.Wait()
		if atomic.LoadInt32(&commitCalls) > 1 {
			t.Fatalf("iter %d: onCommit 调用 %d 次，应至多 1", i, commitCalls)
		}
		// leaseLost 至多关闭一次（非阻塞探测，不假设一定关闭）。
		select {
		case <-sm.leaseLost():
		default:
		}
	}
}

func TestLeaseStateMachine_OnCommitNilSafe(t *testing.T) {
	sm := newLeaseStateMachine()
	// onCommit=nil 在 daemon lock 先获得时不应 panic。
	sm.markDaemonLockCommitted(nil)
	sm.notifyEOF()
}

func TestLeaseStateMachine_MultipleEOFIdempotent(t *testing.T) {
	sm := newLeaseStateMachine()
	sm.notifyEOF()
	sm.notifyEOF()
	sm.notifyEOF()
	// 多次 EOF 不 panic；leaseLost 关闭一次。
	select {
	case <-sm.leaseLost():
	default:
		t.Error("leaseLost 应已关闭")
	}
}

func TestLeaseStateMachine_MultipleCommitIdempotent(t *testing.T) {
	sm := newLeaseStateMachine()
	calls := int32(0)
	onCommit := func() { atomic.AddInt32(&calls, 1) }
	sm.markDaemonLockCommitted(onCommit)
	sm.markDaemonLockCommitted(onCommit)
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("重复 commit 应只调用一次 onCommit，实际 %d", calls)
	}
	sm.notifyEOF()
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("commit 后 EOF 不应再调 onCommit，实际 %d", calls)
	}
}

// ---- 测试桩 ----

// fakeLeaseReader 实现 leaseReader：waitForEOF 阻塞到 close（模拟 EOF）。
type fakeLeaseReader struct {
	closeCh chan struct{}
}

func (f fakeLeaseReader) WaitForEOF() {
	if f.closeCh != nil {
		<-f.closeCh
	}
}

func (f fakeLeaseReader) Close() {}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
