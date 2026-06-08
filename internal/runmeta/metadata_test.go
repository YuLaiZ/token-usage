// internal/runmeta/metadata_test.go
package runmeta

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/fileutil"
)

// ---- 路径构造 ----

func TestPIDPath(t *testing.T) {
	got := PIDPath("/data")
	want := "/data/token-usage.pid"
	if got != want {
		t.Errorf("PIDPath = %q, want %q", got, want)
	}
}

func TestStatePath(t *testing.T) {
	got := StatePath("/data")
	want := "/data/token-usage.runtime.json"
	if got != want {
		t.Errorf("StatePath = %q, want %q", got, want)
	}
}

// ---- WritePIDFile / ReadPIDFile 往返 ----

func TestWritePIDFile_ThenRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	if err := WritePIDFile(p, 12345, "inst-abc"); err != nil {
		t.Fatalf("WritePIDFile err=%v", err)
	}
	pid, inst, err := ReadPIDFile(p)
	if err != nil {
		t.Fatalf("ReadPIDFile err=%v", err)
	}
	if pid != 12345 || inst != "inst-abc" {
		t.Errorf("got pid=%d inst=%q, want 12345 inst-abc", pid, inst)
	}
}

func TestWritePIDFile_NewFormatHasTwoFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	if err := WritePIDFile(p, 99, "xyz"); err != nil {
		t.Fatalf("WritePIDFile err=%v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read err=%v", err)
	}
	want := "99 xyz"
	if string(data) != want {
		t.Errorf("file content = %q, want %q", string(data), want)
	}
}

// ---- 兼容旧格式 <pid> 单值（instanceID 返回空）----

func TestReadPIDFile_OldSingleValueFormat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	os.WriteFile(p, []byte("12345"), 0644)
	pid, inst, err := ReadPIDFile(p)
	if err != nil {
		t.Fatalf("旧格式应读成功 err=%v", err)
	}
	if pid != 12345 || inst != "" {
		t.Errorf("got pid=%d inst=%q, want 12345 空 inst", pid, inst)
	}
}

func TestReadPIDFile_OldSingleValueWithWhitespace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	os.WriteFile(p, []byte("  12345\n"), 0644)
	pid, inst, err := ReadPIDFile(p)
	if err != nil {
		t.Fatalf("应 TrimSpace err=%v", err)
	}
	if pid != 12345 || inst != "" {
		t.Errorf("got pid=%d inst=%q, want 12345 空 inst", pid, inst)
	}
}

// ---- 新格式含空白/换行 ----

func TestReadPIDFile_NewFormatTrailingNewline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	os.WriteFile(p, []byte("777 inst-z\n"), 0644)
	pid, inst, err := ReadPIDFile(p)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if pid != 777 || inst != "inst-z" {
		t.Errorf("got pid=%d inst=%q, want 777 inst-z", pid, inst)
	}
}

func TestReadPIDFile_RejectsExtraFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	if err := os.WriteFile(p, []byte("777 inst-z unexpected"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadPIDFile(p); err == nil {
		t.Fatal("超过新旧协议字段数的 PID 文件必须报错")
	}
}

func TestWritePIDFile_RejectsInvalidValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	if err := WritePIDFile(p, 0, "inst"); err == nil {
		t.Fatal("非正 PID 必须报错")
	}
	if err := WritePIDFile(p, 1, "two fields"); err == nil {
		t.Fatal("包含空白的 instanceID 必须报错")
	}
}

// ---- 读失败场景 ----

func TestReadPIDFile_Missing(t *testing.T) {
	_, _, err := ReadPIDFile(filepath.Join(t.TempDir(), "no.pid"))
	if err == nil {
		t.Fatal("缺失应报错")
	}
	if !os.IsNotExist(err) {
		t.Errorf("应暴露 IsNotExist，got %v", err)
	}
}

func TestReadPIDFile_InvalidNonNumeric(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	os.WriteFile(p, []byte("not-a-number"), 0644)
	if _, _, err := ReadPIDFile(p); err == nil {
		t.Fatal("非法 pid 应报错")
	}
}

func TestReadPIDFile_NegativePID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	os.WriteFile(p, []byte("-1"), 0644)
	if _, _, err := ReadPIDFile(p); err == nil {
		t.Fatal("负 pid 应报错")
	}
}

func TestReadPIDFile_ZeroPID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	os.WriteFile(p, []byte("0"), 0644)
	if _, _, err := ReadPIDFile(p); err == nil {
		t.Fatal("pid=0 应报错")
	}
}

func TestReadPIDFile_NewFormatBadPID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.pid")
	os.WriteFile(p, []byte("abc inst-1"), 0644)
	if _, _, err := ReadPIDFile(p); err == nil {
		t.Fatal("非数字 pid 首字段应报错")
	}
}

// ---- WritePIDFile 端到端用 ReplaceCompleteFile（检查残留 temp 不留）----

func TestWritePIDFile_NoTempLeftBehind(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token-usage.pid")
	if err := WritePIDFile(p, 1, "i"); err != nil {
		t.Fatalf("err=%v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, ".tmp-") {
			t.Errorf("残留 temp 文件未清理: %s", name)
		}
	}
}

// ---- RuntimeState 往返 ----

func TestWriteRuntimeState_ThenRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.runtime.json")
	in := RuntimeState{
		PID:             4242,
		InstanceID:      "inst-1",
		MonitorReady:    true,
		CatchUp:         "succeeded",
		CatchUpFailures: 0,
	}
	if err := WriteRuntimeState(p, in); err != nil {
		t.Fatalf("WriteRuntimeState err=%v", err)
	}
	out, err := ReadRuntimeState(p)
	if err != nil {
		t.Fatalf("ReadRuntimeState err=%v", err)
	}
	if out != in {
		t.Errorf("round-trip 失败:\n got  %+v\n want %+v", out, in)
	}
}

func TestWriteRuntimeState_Overwrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.runtime.json")
	WriteRuntimeState(p, RuntimeState{PID: 1, InstanceID: "a", MonitorReady: false, CatchUp: "pending"})
	WriteRuntimeState(p, RuntimeState{PID: 2, InstanceID: "b", MonitorReady: true, CatchUp: "succeeded"})
	out, err := ReadRuntimeState(p)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out.PID != 2 || out.InstanceID != "b" || !out.MonitorReady || out.CatchUp != "succeeded" {
		t.Errorf("覆盖写入失败: %+v", out)
	}
}

func TestReadRuntimeState_Missing(t *testing.T) {
	_, err := ReadRuntimeState(filepath.Join(t.TempDir(), "no.json"))
	if err == nil {
		t.Fatal("缺失应报错")
	}
}

func TestReadRuntimeState_InvalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.runtime.json")
	os.WriteFile(p, []byte("{not json"), 0644)
	_, err := ReadRuntimeState(p)
	if err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

func TestReadRuntimeState_EmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.runtime.json")
	os.WriteFile(p, []byte(""), 0644)
	_, err := ReadRuntimeState(p)
	if err == nil {
		t.Fatal("空文件应报错")
	}
}

// ---- 写失败：所有写入用 ReplaceCompleteFile，不破坏既有文件 ----

func TestWritePIDFile_AtomicDoesNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token-usage.pid")
	// 先写入有效内容。
	WritePIDFile(p, 10, "orig")
	// 再次写入用有效数据（模拟正常覆盖）。
	if err := WritePIDFile(p, 20, "new"); err != nil {
		t.Fatalf("err=%v", err)
	}
	pid, inst, err := ReadPIDFile(p)
	if err != nil || pid != 20 || inst != "new" {
		t.Errorf("覆盖后读取错: pid=%d inst=%q err=%v", pid, inst, err)
	}
}

// ---- CleanupStaleMetadata：lock 未持有时清理 PID + state + 两类 temp ----

func TestCleanupStaleMetadata_RemovesAllArtifactTypes(t *testing.T) {
	dir := t.TempDir()
	pidPath := PIDPath(dir)
	statePath := StatePath(dir)
	WritePIDFile(pidPath, 1, "stale")
	WriteRuntimeState(statePath, RuntimeState{PID: 1, InstanceID: "stale", CatchUp: "pending"})
	// 模拟 ReplaceCompleteFile 残留的两类 temp（精确前缀）。
	pidTemp := filepath.Join(dir, ".token-usage.pid.tmp-001")
	stateTemp := filepath.Join(dir, ".token-usage.runtime.json.tmp-001")
	os.WriteFile(pidTemp, []byte("x"), 0644)
	os.WriteFile(stateTemp, []byte("x"), 0644)

	if err := CleanupStaleMetadata(dir); err != nil {
		t.Fatalf("CleanupStaleMetadata err=%v", err)
	}
	for _, path := range []string{pidPath, statePath, pidTemp, stateTemp} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("应删除 %s", path)
		}
	}
}

func TestCleanupStaleMetadata_IdempotentWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := CleanupStaleMetadata(dir); err != nil {
		t.Fatalf("空目录应幂等成功 err=%v", err)
	}
}

func TestCleanupStaleMetadata_OnlyRemovesExactTempPrefixes(t *testing.T) {
	dir := t.TempDir()
	// 近似但不精确的 basename 不应被删:ReplaceCompleteFile 的 temp 模式是
	// ".token-usage.pid.tmp-<random>"(前导点 + 尾随连字符 + 随机后缀)。
	// 下面两个文件都缺关键成分,不属于精确前缀,必须保留。
	keep := filepath.Join(dir, "token-usage.pid.tmp") // 无前导点、无尾随连字符+随机后缀
	os.WriteFile(keep, []byte("x"), 0644)
	other := filepath.Join(dir, "other.tmp-001")
	os.WriteFile(other, []byte("x"), 0644)
	if err := CleanupStaleMetadata(dir); err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("不应删除近似前缀文件 keep: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("不应删除无关文件 other: %v", err)
	}
}

func TestCleanupStaleMetadata_DoesNotRemoveUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	keep1 := filepath.Join(dir, "usage.db")
	keep2 := filepath.Join(dir, "token-usage.lock")
	keep3 := filepath.Join(dir, "daemon.err.log")
	os.WriteFile(keep1, []byte("db"), 0644)
	os.WriteFile(keep2, []byte("lock"), 0644)
	os.WriteFile(keep3, []byte("log"), 0644)
	if err := CleanupStaleMetadata(dir); err != nil {
		t.Fatalf("err=%v", err)
	}
	for _, p := range []string{keep1, keep2, keep3} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("不应删除 %s", p)
		}
	}
}

// ---- CleanupOwnedMetadata：按 PID/instanceID 归属清理 ----

func TestCleanupOwnedMetadata_RemovesOwnPIDAndState(t *testing.T) {
	dir := t.TempDir()
	pidPath := PIDPath(dir)
	statePath := StatePath(dir)
	WritePIDFile(pidPath, 123, "mine")
	WriteRuntimeState(statePath, RuntimeState{PID: 123, InstanceID: "mine", CatchUp: "pending"})

	if err := CleanupOwnedMetadata(dir, 123, "mine"); err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("应删除 PID 文件")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("应删除 state 文件")
	}
}

func TestCleanupOwnedMetadata_DoesNotRemoveOthersPID(t *testing.T) {
	dir := t.TempDir()
	pidPath := PIDPath(dir)
	statePath := StatePath(dir)
	WritePIDFile(pidPath, 123, "theirs")
	WriteRuntimeState(statePath, RuntimeState{PID: 123, InstanceID: "theirs", CatchUp: "pending"})

	// 不同 PID：不删。
	if err := CleanupOwnedMetadata(dir, 999, "mine"); err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("不应删他人 PID 文件")
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("不应删他人 state 文件")
	}
}

func TestCleanupOwnedMetadata_SamePIDDifferentInstanceID_DoesNotRemove(t *testing.T) {
	// PID 复用：同 PID 但不同 instanceID 不接受旧 state。
	dir := t.TempDir()
	pidPath := PIDPath(dir)
	statePath := StatePath(dir)
	WritePIDFile(pidPath, 777, "old-gen")
	WriteRuntimeState(statePath, RuntimeState{PID: 777, InstanceID: "old-gen", CatchUp: "pending"})

	if err := CleanupOwnedMetadata(dir, 777, "new-gen"); err != nil {
		t.Fatalf("err=%v", err)
	}
	// 归属不匹配：不删（正常退出顺序由调用方先确认所有权）。
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("PID 不匹配 instanceID 时不应删 PID 文件")
	}
}

func TestCleanupOwnedMetadata_NoFiles_NoError(t *testing.T) {
	dir := t.TempDir()
	if err := CleanupOwnedMetadata(dir, 123, "mine"); err != nil {
		t.Fatalf("无文件应幂等成功 err=%v", err)
	}
}

func TestCleanupOwnedMetadata_PIDOnly_StateFromOther_NotRemoved(t *testing.T) {
	// PID 是自己的，state 是他人的：只删 PID，不删他人 state。
	dir := t.TempDir()
	pidPath := PIDPath(dir)
	statePath := StatePath(dir)
	WritePIDFile(pidPath, 555, "mine")
	WriteRuntimeState(statePath, RuntimeState{PID: 999, InstanceID: "other", CatchUp: "pending"})

	if err := CleanupOwnedMetadata(dir, 555, "mine"); err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("应删自己的 PID 文件")
	}
	// state 归属 999/other，不删。
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("不应删他人 state 文件")
	}
}

func TestCleanupMetadataRejectsEmptyDataDir(t *testing.T) {
	if err := CleanupStaleMetadata(""); err == nil {
		t.Fatal("空 data_dir 不能隐式清理当前工作目录")
	}
	if err := CleanupOwnedMetadata("", 123, "mine"); err == nil {
		t.Fatal("空 data_dir 不能隐式清理当前工作目录")
	}
}

func TestCleanupOwnedMetadata_ReportsUnreadableOwnershipMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(PIDPath(dir), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := CleanupOwnedMetadata(dir, 123, "mine")
	if err == nil {
		t.Fatal("无法验证所有权时必须报告错误")
	}
	if _, statErr := os.Stat(PIDPath(dir)); statErr != nil {
		t.Fatalf("无法验证归属时不得删除 PID 文件: %v", statErr)
	}
	if _, statErr := os.Stat(StatePath(dir)); statErr != nil {
		t.Fatalf("无法验证归属时不得删除 runtime-state 文件: %v", statErr)
	}
}

// ---- reader 绝不返回 ready（降级语义）----
// ReadRuntimeState 失败返回 error，不返回半成品；调用方据此降级。

func TestReadRuntimeState_PartialJSON_Error(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token-usage.runtime.json")
	os.WriteFile(p, []byte(`{"pid": 1`), 0644) // 截断 JSON
	_, err := ReadRuntimeState(p)
	if err == nil {
		t.Fatal("截断 JSON 应报错，绝不返回 ready 半成品")
	}
}

func TestReadRuntimeState_RejectsSemanticallyEmptyObject(t *testing.T) {
	// 空 JSON 对象缺少协议必需字段，必须整体拒绝而非返回“可用”半成品。
	p := filepath.Join(t.TempDir(), "token-usage.runtime.json")
	os.WriteFile(p, []byte(`{}`), 0644)
	if _, err := ReadRuntimeState(p); err == nil {
		t.Fatal("空对象必须报错")
	}
}

func TestRuntimeState_RejectsInvalidSemanticFields(t *testing.T) {
	tests := []RuntimeState{
		{PID: 0, InstanceID: "i", CatchUp: "pending"},
		{PID: 1, InstanceID: "", CatchUp: "pending"},
		{PID: 1, InstanceID: "two fields", CatchUp: "pending"},
		{PID: 1, InstanceID: "i", CatchUp: "unknown"},
		{PID: 1, InstanceID: "i", CatchUp: "succeeded", CatchUpFailures: 1},
		{PID: 1, InstanceID: "i", CatchUp: "failed", CatchUpFailures: 0},
	}
	for _, state := range tests {
		path := filepath.Join(t.TempDir(), "state.json")
		if err := WriteRuntimeState(path, state); err == nil {
			t.Errorf("WriteRuntimeState 应拒绝 %+v", state)
		}
		raw, _ := json.Marshal(state)
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadRuntimeState(path); err == nil {
			t.Errorf("ReadRuntimeState 应拒绝 %+v", state)
		}
	}
}

// ---- 多错误聚合：CleanupStaleMetadata 主因优先 ----

func TestCleanupStaleMetadata_ReadDirError(t *testing.T) {
	// 路径存在但不是目录：ReadDir 的结构错误必须传播。
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := CleanupStaleMetadata(path)
	if err == nil {
		t.Fatal("应报错")
	}
}

// ---- helpers：错误包装不丢底层 ----

func TestReadPIDFile_ErrorNotExists(t *testing.T) {
	_, _, err := ReadPIDFile(filepath.Join(t.TempDir(), "x"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("底层 IsNotExist 应可识别: %v", err)
		}
	}
}

func TestCleanupStaleMetadata_MissingDataDirIsAlreadyClean(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-created")
	if err := CleanupStaleMetadata(missing); err != nil {
		t.Fatalf("不存在的 data_dir 应视为已清理，实际错误: %v", err)
	}
}

// ---- temp 前缀与 fileutil 保持同步（跨包耦合保护）----
// runmeta 的 pidTempPrefix/stateTempPrefix 必须与 fileutil.TempPrefix 推导一致,
// 防止 fileutil 改 temp 命名模式后 runmeta 清理前缀静默过时。

func TestTempPrefixes_MatchFileUtilTempPrefix(t *testing.T) {
	// 模拟 runmeta 实际清理 target:fileutil.TempPrefix 按 target basename 推导。
	wantPID := fileutil.TempPrefix("token-usage.pid")
	wantState := fileutil.TempPrefix("token-usage.runtime.json")
	if pidTempPrefix != wantPID {
		t.Errorf("pidTempPrefix = %q, want fileutil.TempPrefix %q", pidTempPrefix, wantPID)
	}
	if stateTempPrefix != wantState {
		t.Errorf("stateTempPrefix = %q, want fileutil.TempPrefix %q", stateTempPrefix, wantState)
	}
}
