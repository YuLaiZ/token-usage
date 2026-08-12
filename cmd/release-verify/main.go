// cmd/release-verify 校验 make release-build 产出的 dist/ 发布物与自更新合同一致。
//
// 它经 internal/update 的真实代码路径验证（与 updater 下载后解析清单、定位本机资产
// 使用同一套 ParseManifest / AssetName），杜绝「生成器输出格式」与「消费者解析合同」
// 之间漂移：若 release-build 产出的 SHA256SUMS 不能被 updater 解析，本工具会失败。
//
// 用法：go run ./cmd/release-verify -dist dist -version <注入的 tag>
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/update"
)

// supportedPlatforms 是当前正式分发的全部 (GOOS, GOARCH) 组合。
// 资产名始终经 update.AssetName 取（权威映射），这里只列举平台组合，
// 不硬编码资产名字符串，避免与 assets.go 的映射表脱钩。
var supportedPlatforms = [][2]string{
	{"darwin", "arm64"},
	{"darwin", "amd64"},
	{"windows", "amd64"},
}

func main() {
	distDir := flag.String("dist", "dist", "发布产物目录")
	version := flag.String("version", "", "注入的 VERSION（本机为受支持平台时用于 --version 校验；空则跳过该步）")
	flag.Parse()
	if err := run(*distDir, *version); err != nil {
		fmt.Fprintf(os.Stderr, "release-verify 失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("release-verify 通过：资产清单、hash 与本机版本均一致")
}

// run 执行全部校验，任一项失败返回非 nil 错误。
func run(distDir, version string) error {
	// 1. dist 目录恰好包含三个二进制 + SHA256SUMS，无多余、无缺失、无子目录。
	if err := verifyDistFileSet(distDir); err != nil {
		return err
	}

	// 2. 用 updater 的真实解析路径校验 SHA256SUMS 清单格式。
	manifestPath := filepath.Join(distDir, update.SumsAssetName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("读取 SHA256SUMS 失败: %w", err)
	}
	manifest, err := update.ParseManifest(data)
	if err != nil {
		return fmt.Errorf("SHA256SUMS 清单解析失败（updater 合同）: %w", err)
	}

	// 3. 对每个受支持平台资产重算 SHA256，与清单逐字节比对。
	for _, plat := range supportedPlatforms {
		name, ok := update.AssetName(plat[0], plat[1])
		if !ok {
			return fmt.Errorf("update.AssetName(%s,%s) 未返回资产名（映射表脱钩）", plat[0], plat[1])
		}
		wantHash, ok := manifest.HashFor(name)
		if !ok {
			return fmt.Errorf("清单缺少资产 %q 的 hash", name)
		}
		gotHash, err := sha256File(filepath.Join(distDir, name))
		if err != nil {
			return fmt.Errorf("重算 %q 的 hash 失败: %w", name, err)
		}
		if gotHash != wantHash {
			return fmt.Errorf("资产 %q hash 不符: 清单=%s 实算=%s", name, wantHash, gotHash)
		}
	}

	// 4. 本机若为受支持平台，运行对应二进制的 --version，断言其报告注入的 VERSION。
	if name, ok := update.AssetName(runtime.GOOS, runtime.GOARCH); ok && version != "" {
		if err := verifyNativeVersion(filepath.Join(distDir, name), version); err != nil {
			return err
		}
	}
	return nil
}

// verifyDistFileSet 确认 dist 目录文件集合恰好等于冻结的四个资产名。
func verifyDistFileSet(distDir string) error {
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return fmt.Errorf("读取 dist 目录失败: %w", err)
	}
	want := make(map[string]struct{})
	want[update.SumsAssetName] = struct{}{}
	for _, plat := range supportedPlatforms {
		name, ok := update.AssetName(plat[0], plat[1])
		if !ok {
			return fmt.Errorf("update.AssetName(%s,%s) 未返回资产名", plat[0], plat[1])
		}
		want[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			return fmt.Errorf("dist 含子目录 %q（发布物应为纯文件）", e.Name())
		}
		if _, ok := want[e.Name()]; !ok {
			return fmt.Errorf("dist 含未知文件 %q（只允许三二进制 + SHA256SUMS）", e.Name())
		}
		if _, dup := seen[e.Name()]; dup {
			return fmt.Errorf("dist 含重复条目 %q", e.Name())
		}
		seen[e.Name()] = struct{}{}
	}
	if len(seen) != len(want) {
		var missing []string
		for n := range want {
			if _, ok := seen[n]; !ok {
				missing = append(missing, n)
			}
		}
		return fmt.Errorf("dist 文件数不符: got %d want %d，缺失 %v", len(seen), len(want), missing)
	}
	return nil
}

// verifyNativeVersion 运行本机二进制的 --version，断言输出为 "token-usage <version>"。
func verifyNativeVersion(bin, version string) error {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return fmt.Errorf("执行本机 %s --version 失败: %w", filepath.Base(bin), err)
	}
	got := strings.TrimRight(string(out), "\n")
	want := fmt.Sprintf("token-usage %s", version)
	if got != want {
		return fmt.Errorf("本机 --version 不符: got %q want %q", got, want)
	}
	return nil
}

// sha256File 返回文件内容的 SHA256 小写十六进制摘要。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
