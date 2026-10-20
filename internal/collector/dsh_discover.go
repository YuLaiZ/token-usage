package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// dshGenerationRe 匹配单个会话目录里的规范文件名：
// session.jsonl(v0)、session.vN.jsonl(N>=1)，以及对应的 .zstd 变体。
var dshGenerationRe = regexp.MustCompile(`^session(?:\.v([1-9][0-9]*))?\.jsonl(\.zstd)?$`)

// dshCompression 是从文件名后缀推断的物理编码。
type dshCompression string

const (
	dshCompressionNone dshCompression = "none"
	dshCompressionZstd dshCompression = "zstd"
)

// dshGeneration 是一个会话目录里的一份规范代际文件。
type dshGeneration struct {
	Version     int
	Compression dshCompression
	Path        string
}

// dshSessionFile 是发现结果中的一个候选会话文件。
// 这里只做路径级发现：header 解析、版本准入与重复 id 由有界读取链路负责。
type dshSessionFile struct {
	Path        string
	Version     int
	Compression dshCompression
}

// dshDiscovery 汇总一次根目录扫描：候选会话、告警与错误。
// 错误是「该会话/该文件」级别的，不阻断其他会话。
type dshDiscovery struct {
	Sessions []dshSessionFile
	Warnings []string
	Errors   []string
}

// discoverDSHSessions 扫描 <root>/<project>/<session>/session[.vN].jsonl[.zstd]。
//
// 规则：
//   - 只取数值最高的规范代际；
//   - 同一会话目录内出现任意 opposite 编码 → 该会话判失败（与 DSH encodingMismatch 对齐）；
//   - 本函数不打开文件、不读 header，避免绕过帧预算与单行上限。
func discoverDSHSessions(root string) dshDiscovery {
	var out dshDiscovery
	if root == "" {
		return out
	}
	projects, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			out.Errors = append(out.Errors, fmt.Sprintf("read sessions root %s: %v", root, err))
		}
		return out
	}

	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		projectDir := filepath.Join(root, project.Name())
		sessionDirs, err := os.ReadDir(projectDir)
		if err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("read project dir %s: %v", projectDir, err))
			continue
		}
		for _, sessionDir := range sessionDirs {
			if !sessionDir.IsDir() {
				continue
			}
			dir := filepath.Join(projectDir, sessionDir.Name())
			selected, warnings, errs := selectDSHGeneration(dir)
			out.Warnings = append(out.Warnings, warnings...)
			out.Errors = append(out.Errors, errs...)
			if selected != nil {
				out.Sessions = append(out.Sessions, dshSessionFile{
					Path:        selected.Path,
					Version:     selected.Version,
					Compression: selected.Compression,
				})
			}
		}
	}

	sort.Slice(out.Sessions, func(i, j int) bool { return out.Sessions[i].Path < out.Sessions[j].Path })
	sort.Strings(out.Warnings)
	sort.Strings(out.Errors)
	return out
}

// selectDSHGeneration 在一个会话目录内选最高代，并检测混合编码。
func selectDSHGeneration(dir string) (*dshGeneration, []string, []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, []string{fmt.Sprintf("read session dir %s: %v", dir, err)}
	}

	var generations []dshGeneration
	encodings := make(map[dshCompression]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "session.lock" {
			continue
		}
		version, compression, ok := parseDSHGenerationName(name)
		if !ok {
			continue
		}
		path := filepath.Join(dir, name)
		generations = append(generations, dshGeneration{Version: version, Compression: compression, Path: path})
		if _, exists := encodings[compression]; !exists {
			encodings[compression] = path
		}
	}
	if len(generations) == 0 {
		return nil, nil, nil
	}
	if len(encodings) > 1 {
		// DSH 运行时会以 encodingMismatch 拒绝该根；采集侧逐会话失败，不猜。
		return nil, nil, []string{fmt.Sprintf(
			"%s: mixed encodings in one session dir (zstd and plaintext), skipped", dir)}
	}

	sort.Slice(generations, func(i, j int) bool {
		if generations[i].Version != generations[j].Version {
			return generations[i].Version > generations[j].Version
		}
		return generations[i].Path < generations[j].Path
	})
	selected := generations[0]
	return &selected, nil, nil
}

// parseDSHGenerationName 解析规范文件名；version 0 对应 session.jsonl。
func parseDSHGenerationName(name string) (int, dshCompression, bool) {
	match := dshGenerationRe.FindStringSubmatch(name)
	if match == nil {
		return 0, "", false
	}
	version := 0
	if match[1] != "" {
		parsed, err := strconv.Atoi(match[1])
		if err != nil {
			return 0, "", false
		}
		version = parsed
	}
	compression := dshCompressionNone
	if match[2] == ".zstd" {
		compression = dshCompressionZstd
	}
	return version, compression, true
}
