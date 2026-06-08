// internal/service/service_windows_test.go
//go:build windows

package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildRegistryValue_Format(t *testing.T) {
	opts := Options{Label: Label, BinPath: `C:\Program Files\tu.exe`, DataDir: `C:\data`, Args: []string{"_run"}}
	got := buildRegistryValue(opts)
	want := `"C:\Program Files\tu.exe" _run`
	if got != want {
		t.Errorf("buildRegistryValue=%q want %q", got, want)
	}
}

func TestParseRegistryValue_NewRun(t *testing.T) {
	bin, args, err := parseRegistryValue(`"C:\tu.exe" _run`)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if bin != `C:\tu.exe` {
		t.Errorf("bin=%q want C:\\tu.exe", bin)
	}
	if len(args) != 1 || args[0] != "_run" {
		t.Errorf("args=%v want [_run]", args)
	}
}

func TestParseRegistryValue_LegacyRunDaemon(t *testing.T) {
	bin, args, err := parseRegistryValue(`"C:\tu.exe" run --daemon`)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if bin != `C:\tu.exe` {
		t.Errorf("bin=%q", bin)
	}
	if len(args) != 2 || args[0] != "run" || args[1] != "--daemon" {
		t.Errorf("args=%v want [run --daemon]", args)
	}
}

func TestParseRegistryValue_InvalidNoQuote(t *testing.T) {
	if _, _, err := parseRegistryValue(`C:\tu.exe _run`); err == nil {
		t.Error("应以双引号开头，应报错")
	}
}

func TestStopRunningInstance_MissingPIDFileFailsWithoutTaskkill(t *testing.T) {
	dir := t.TempDir()
	opts := Options{BinPath: `C:\nonexistent-token-usage-test.exe`, DataDir: dir}

	original := runTaskkill
	defer func() { runTaskkill = original }()
	called := false
	runTaskkill = func(args ...string) ([]byte, error) {
		called = true
		return nil, nil
	}

	err := stopRunningInstanceByPID(opts)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
	if called {
		t.Fatal("PID 文件缺失时不得按名称或未知 PID 调 taskkill")
	}
}

func TestStopRunningInstance_CorruptPIDFailsWithoutTaskkill(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "token-usage.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-number"), 0644); err != nil {
		t.Fatalf("写入损坏 PID 文件失败: %v", err)
	}
	opts := Options{BinPath: `C:\nonexistent-token-usage-test.exe`, DataDir: dir}

	original := runTaskkill
	defer func() { runTaskkill = original }()
	called := false
	runTaskkill = func(args ...string) ([]byte, error) {
		called = true
		return nil, nil
	}

	if err := stopRunningInstanceByPID(opts); err == nil {
		t.Fatal("PID 文件损坏时应返回错误")
	}
	if called {
		t.Fatal("PID 文件损坏时不得按名称或未知 PID 调 taskkill")
	}
}

func TestStopRunningInstance_TaskkillFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "token-usage.pid")
	if err := os.WriteFile(pidPath, []byte("321"), 0644); err != nil {
		t.Fatalf("写入 PID 文件失败: %v", err)
	}

	original := runTaskkill
	defer func() { runTaskkill = original }()
	var gotArgs []string
	runTaskkill = func(args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte("denied"), errors.New("exit 1")
	}

	err := stopRunningInstanceByPID(Options{DataDir: dir})
	if err == nil {
		t.Fatal("taskkill 失败必须传播")
	}
	if want := []string{"/F", "/PID", "321"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("taskkill args = %v, want %v", gotArgs, want)
	}
}

func TestStopRunningInstance_ParsesFirstFieldOfNewFormat(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "token-usage.pid")
	if err := os.WriteFile(pidPath, []byte("654 inst-abc"), 0644); err != nil {
		t.Fatalf("写入 PID 文件失败: %v", err)
	}

	original := runTaskkill
	defer func() { runTaskkill = original }()
	var gotArgs []string
	runTaskkill = func(args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return nil, nil
	}

	if err := stopRunningInstanceByPID(Options{DataDir: dir}); err != nil {
		t.Fatalf("新双字段格式应读取准确 PID: %v", err)
	}
	if want := []string{"/F", "/PID", "654"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("taskkill args = %v, want %v", gotArgs, want)
	}
}
