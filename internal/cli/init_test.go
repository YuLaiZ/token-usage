package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitCmd_GeneratesConfigAtFixedPath config init 始终在固定的
// ~/.token-usage/config.toml 创建配置并初始化 usage.db（默认目录分支）。
// 隔离 HOME 使 os.UserHomeDir 指向临时目录，避免污染真实环境。
func TestInitCmd_GeneratesConfigAtFixedPath(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome) // Windows 上 os.UserHomeDir 读 USERPROFILE

	cmd := newInitCmd()
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init 失败: %v", err)
	}

	cfgDir := filepath.Join(tmpHome, ".token-usage")
	cfgPath := filepath.Join(cfgDir, "config.toml")
	dbPath := filepath.Join(cfgDir, "usage.db")

	// 目录、配置、db 都应生成在固定 ~/.token-usage 下
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("config.toml 未生成: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("usage.db 未生成: %v", err)
	}
}

// TestInitCmd_ConfigAlwaysAtFixedPathDespiteCustomDataDir config 路径固定不随
// data_dir 变化：预置一份 data_dir 指向自定义目录的配置后跑 init，配置仍只应出现在
// 固定的 ~/.token-usage/config.toml，绝不在自定义 data_dir 下生成第二份 config.toml；
// 数据文件 usage.db 应初始化在配置声明的 data_dir 下。
func TestInitCmd_ConfigAlwaysAtFixedPathDespiteCustomDataDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	// 预置一份配置（位于固定路径），data_dir 指向自定义目录
	cfgDir := filepath.Join(tmpHome, ".token-usage")
	customData := filepath.Join(tmpHome, "my-data")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfgContent := `data_dir = "` + customData + `"
[log]
level = "info"
dir = "` + customData + `/logs"
max_days = 7
`
	fixedCfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(fixedCfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := newInitCmd()
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init 失败: %v", err)
	}

	// db 应建在配置声明的 customData 下，而非默认 ~/.token-usage
	if _, err := os.Stat(filepath.Join(customData, "usage.db")); err != nil {
		t.Errorf("应在配置的 data_dir 下建 db，stat 失败: %v", err)
	}
	// 固定路径配置仍应存在且内容未被覆盖（已存在则保留）
	got, err := os.ReadFile(fixedCfgPath)
	if err != nil {
		t.Fatalf("固定配置应存在: %v", err)
	}
	if string(got) != cfgContent {
		t.Errorf("已存在的固定 config.toml 被覆盖\nwant: %q\ngot:  %q", cfgContent, string(got))
	}
	// data_dir 下绝不应生成第二份 config.toml
	if _, err := os.Stat(filepath.Join(customData, "config.toml")); err == nil {
		t.Errorf("data_dir 下不应生成 config.toml（config 路径固定为 ~/.token-usage/config.toml）")
	}
}

// TestInitCmd_ConfigExistsSkipsOverwrite 已有固定 config.toml 时 init 不覆盖它，
// 但仍可初始化缺失的数据文件（usage.db）。幂等：再次跑也不改写 config。
func TestInitCmd_ConfigExistsSkipsOverwrite(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cfgDir := filepath.Join(tmpHome, ".token-usage")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatal(err)
	}
	original := []byte("# user-marker\ndata_dir = \"" + cfgDir + "\"\n")
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, original, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := newInitCmd()
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init 失败: %v", err)
	}

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("已存在的 config.toml 被覆盖\nwant: %q\ngot:  %q", original, got)
	}
	// 即便 config 已存在，data 文件 usage.db 仍应被初始化（data_dir=cfgDir）
	if _, err := os.Stat(filepath.Join(cfgDir, "usage.db")); err != nil {
		t.Errorf("已有 config 时仍应初始化 usage.db: %v", err)
	}
}

// TestInitCmd_NoArgs config init 拒绝任何位置参数（cobra.NoArgs）。
func TestInitCmd_NoArgs(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cmd := newInitCmd()
	cmd.SetArgs([]string{"unexpected-arg"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	err := cmd.Execute()
	if err == nil {
		t.Fatal("传多余参数应报错（NoArgs），实际无错")
	}
	// cobra.NoArgs 错误信息含 "args" 字样
	if !strings.Contains(err.Error(), "arg") {
		t.Errorf("NoArgs 错误应提及 args，got: %v", err)
	}
}

// TestInitCmd_UsesReplaceCompleteFile config init 的配置写入复用
// fileutil.ReplaceCompleteFile：写后同目录不应残留 temp 文件（.config.toml.tmp-*）。
func TestInitCmd_UsesReplaceCompleteFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cmd := newInitCmd()
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init 失败: %v", err)
	}

	cfgDir := filepath.Join(tmpHome, ".token-usage")
	matches, err := filepath.Glob(filepath.Join(cfgDir, ".config.toml.tmp-*"))
	if err != nil {
		t.Fatalf("glob temp: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("fileutil.ReplaceCompleteFile 应清理 temp，残留: %v", matches)
	}
}

func TestInitCmd_InvalidExistingConfigDoesNotInitializeDefaultDB(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	cfgDir := filepath.Join(tmpHome, ".token-usage")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "config.toml")
	bad := []byte("this is not = valid toml [")
	if err := os.WriteFile(cfgPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newInitCmd()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.Execute(); err == nil {
		t.Fatal("已有配置损坏时必须报错，不能静默回退默认 data_dir")
	}
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bad) {
		t.Error("损坏的已有配置也不能被 init 覆盖")
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "usage.db")); !os.IsNotExist(err) {
		t.Errorf("配置无效时不应在默认目录初始化 DB，stat err=%v", err)
	}
}
