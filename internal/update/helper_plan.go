package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// helper_plan.go 实现 Windows staged replacement 的 helper 执行计划：计划结构、
// 路径派生与严格校验。本文件平台无关（无 build tag），可单元测试。
//
// 设计约束（来自安全要求）：
//   - helper 计划必须是 helper 自身目录中的普通非 symlink 文件；
//   - 所有 target/stage/backup/result/helper 路径均从「helper 自身目录 + target basename + nonce」
//     派生，绝不接受 plan 中嵌入的任意路径；
//   - nonce 绑定本次替换的全部文件，校验时重新派生路径并要求精确匹配。
//
// 文件命名约定（base = target basename，nonce = 随机 hex）：
//
//	helper = "." + base + ".stage-helper-" + nonce + ".exe"
//	plan   = "." + base + ".update-plan-"   + nonce
//	stage  = "." + base + ".update-stage-"  + nonce
//	backup = "." + base + ".update-backup-" + nonce
//	result = "." + base + ".update-result-" + nonce + ".json"
//
// stage/backup 沿用 POSIX 事务的同名后缀（updateStageSuffix/updateBackupSuffix），
// 保证两类事务文件命名一致、清理前缀统一；helper/plan/result 是 Windows helper 专属。
// helper 后缀避开 "update"：helper.exe 会被 spawn 执行，而实测 Windows 安全策略
// 拦截文件名含 "update" 的可执行文件（与 stage 同因，见 download.go）。

// helperSuffix / planSuffix / resultSuffix 是 Windows helper 专属文件的后缀。
// stage/backup 复用 install_common.go 的 updateStageSuffix / updateBackupSuffix。
const (
	helperSuffix = ".stage-helper-" // helper.exe 后缀（接 nonce + ".exe"）
	planSuffix   = ".update-plan-"  // plan 文件后缀（接 nonce）
	resultSuffix = ".update-result-" // result 文件后缀（接 nonce + ".json"）
	helperExeExt = ".exe"           // Windows 可执行扩展名
	resultExt    = ".json"          // result 文件扩展名
)

// helperPlan 是 Windows staged replacement 的 helper 执行计划。
// 由父进程的 Windows Installer.Install 写入（权限收紧 0600），由后台 helper 读取。
// 不含任何完整路径字段——helper 据自身目录 + target basename + nonce 重新派生全部路径，
// 杜绝 plan 嵌入任意外部路径（plan 路径注入防御）。
// Parent 是父进程（拉起 helper 的 update 进程）的显式身份（PID + 创建时间），
// helper 据此等待精确的父进程实例退出，杜绝 PID 复用导致的 TOCTOU。Parent 是标量身份，
// 不是路径，不违反「plan 不含路径」的安全约束。
type helperPlan struct {
	Nonce          string          `json:"nonce"`           // 绑定本次替换的随机 hex（32 字符）
	TargetBasename string          `json:"target_basename"` // 被替换的目标二进制 basename
	OldSHA256      string          `json:"old_sha256"`      // 旧 target SHA256（backup 校验与回滚用）
	NewSHA256      string          `json:"new_sha256"`      // 新 stage SHA256（MoveFileEx 后校验用）
	WasRunning     bool            `json:"was_running"`     // 原 daemon 运行态（决定 helper 是否重启 daemon）
	Parent         ProcessIdentity `json:"parent"`          // 父进程身份（spawn 前由父进程捕获）
}

// helperPaths 是从 (selfDir, targetBasename, nonce) 派生的全部 helper 文件绝对路径。
// helper 用它定位 target/stage/backup/helper/plan/result，全部受控派生，无外部输入。
type helperPaths struct {
	Target string // 被替换的目标二进制（= selfDir/targetBasename）
	Stage  string // 已下载并校验的新版本 stage
	Backup string // 旧 target 备份
	Helper string // helper.exe 自身（= selfExe）
	Plan   string // 计划文件
	Result string // 结果文件
}

// deriveHelperPaths 从 (selfDir, targetBasename, nonce) 派生全部 helper 文件绝对路径。
// selfDir 是 helper 自身所在目录（= target 所在目录）；targetBasename 是被替换二进制名；
// nonce 绑定本次替换的全部文件。
func deriveHelperPaths(selfDir, targetBasename, nonce string) helperPaths {
	prefix := "." + targetBasename
	return helperPaths{
		Target: filepath.Join(selfDir, targetBasename),
		Stage:  filepath.Join(selfDir, prefix+updateStageSuffix+nonce),
		Backup: filepath.Join(selfDir, prefix+updateBackupSuffix+nonce),
		Helper: filepath.Join(selfDir, prefix+helperSuffix+nonce+helperExeExt),
		Plan:   filepath.Join(selfDir, prefix+planSuffix+nonce),
		Result: filepath.Join(selfDir, prefix+resultSuffix+nonce+resultExt),
	}
}

// writeHelperPlan 把 plan 以 JSON 写入 path，权限 0600，原子替换（经 fileutil）。
func writeHelperPlan(path string, plan helperPlan) error {
	data, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("序列化 helper 计划失败: %w", err)
	}
	return writeJournalFile(path, data) // 复用 0600 原子写（fileutil.ReplaceCompleteFile）
}

// readHelperPlan 读取并解析 path 处的 helper 计划。
// 文件不存在返回 fs.ErrNotExist 包装错误（调用方据此判断无计划）。
func readHelperPlan(path string) (helperPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return helperPlan{}, fmt.Errorf("读取 helper 计划失败: %w", err)
	}
	var plan helperPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return helperPlan{}, fmt.Errorf("解析 helper 计划失败: %w", err)
	}
	return plan, nil
}

// HelperPlan 是 helperPlan 的导出版本（字段同 helperPlan），供 CLI 命令读取计划字段。
type HelperPlan = helperPlan

// ValidateHelperPlan 是 validateHelperPlan 的导出版本，供 CLI helper 命令在运行 runner 前
// 取得派生路径（例如成功后 spawn cleanup 需要 target 路径）。
func ValidateHelperPlan(selfExe, planPath string) (ValidatedHelperPlan, error) {
	v, err := validateHelperPlan(selfExe, planPath)
	if err != nil {
		return ValidatedHelperPlan{}, err
	}
	return ValidatedHelperPlan{Plan: v.Plan, Paths: v.Paths}, nil
}

// ValidateCleanupPlan 严格校验由新 target 执行的 cleanup 计划。
// cleanup 与 helper 使用同一份 nonce 绑定 plan，但它的自身可执行文件必须是派生的
// target，而不是临时 helper.exe；通过后调用方只能使用返回的受控派生字段做清理。
func ValidateCleanupPlan(selfExe, planPath string) (ValidatedHelperPlan, error) {
	v, err := validateCleanupPlan(selfExe, planPath)
	if err != nil {
		return ValidatedHelperPlan{}, err
	}
	return ValidatedHelperPlan{Plan: v.Plan, Paths: v.Paths}, nil
}

// ValidatedHelperPlan 是 validatedHelperPlan 的导出版本。
type ValidatedHelperPlan struct {
	Plan  HelperPlan
	Paths HelperPaths
}

// HelperPaths 是 helperPaths 的导出版本（类型别名，字段同 helperPaths）。
type HelperPaths = helperPaths

// validatedHelperPlan 是 validateHelperPlan 的返回：经校验的计划与派生路径。
// 调用方据此操作文件，杜绝使用任何未经派生的路径。
type validatedHelperPlan struct {
	Plan  helperPlan
	Paths helperPaths
}

// validateHelperPlan 严格校验 helper 计划，返回经派生的路径。
//
// 校验顺序（任一失败返回 error，绝不放宽）：
//  1. selfExe 存在且为普通非 symlink 文件（helper 自身必须可信）；
//  2. selfExeDir = filepath.Dir(filepath.Clean(selfExe))；
//  3. planPath 必须在 selfExeDir 内（Clean 后 filepath.Dir 相等），杜绝跨目录注入；
//  4. planPath 存在且为普通非 symlink 文件；
//  5. 解析 plan；nonce 非空且为合法 hex；targetBasename 非空且不含路径分隔符；
//     Parent 身份（PID + 创建时间）均为非零（helper 据显式身份等待父进程，杜绝 PID 复用）；
//  6. 从 (selfExeDir, targetBasename, nonce) 重新派生 paths；
//  7. planPath 必须精确等于派生的 paths.Plan；
//  8. selfExe 必须精确等于派生的 paths.Helper（确认 helper 自身就是 nonce 绑定的 helper.exe）。
//
// 通过后，调用方只能使用返回的 paths（全部受控派生），绝不直接信任 plan 中字段做路径操作。
func validateHelperPlan(selfExe, planPath string) (validatedHelperPlan, error) {
	// 1. helper 自身必须可信。
	selfInfo, err := os.Lstat(selfExe)
	if err != nil {
		return validatedHelperPlan{}, fmt.Errorf("无法读取 helper 自身元信息 %s: %w", selfExe, err)
	}
	if selfInfo.Mode()&fs.ModeSymlink != 0 {
		return validatedHelperPlan{}, fmt.Errorf("helper 自身 %s 是符号链接，拒绝执行", selfExe)
	}
	if !selfInfo.Mode().IsRegular() {
		return validatedHelperPlan{}, fmt.Errorf("helper 自身 %s 不是普通文件（mode %s）", selfExe, selfInfo.Mode())
	}

	// 2. helper 自身目录。
	selfDir := filepath.Dir(filepath.Clean(selfExe))

	// 3. planPath 必须在 helper 自身目录内（精确目录匹配，杜绝 ../ 注入）。
	planClean := filepath.Clean(planPath)
	if filepath.Dir(planClean) != selfDir {
		return validatedHelperPlan{}, fmt.Errorf("计划文件 %s 不在 helper 自身目录 %s 内，拒绝执行", planPath, selfDir)
	}

	// 4. planPath 必须是普通非 symlink 文件。
	planInfo, err := os.Lstat(planClean)
	if err != nil {
		return validatedHelperPlan{}, fmt.Errorf("无法读取计划文件元信息 %s: %w", planClean, err)
	}
	if planInfo.Mode()&fs.ModeSymlink != 0 {
		return validatedHelperPlan{}, fmt.Errorf("计划文件 %s 是符号链接，拒绝执行", planClean)
	}
	if !planInfo.Mode().IsRegular() {
		return validatedHelperPlan{}, fmt.Errorf("计划文件 %s 不是普通文件（mode %s）", planClean, planInfo.Mode())
	}

	// 5. 解析计划并校验字段。
	plan, perr := readHelperPlan(planClean)
	if perr != nil {
		return validatedHelperPlan{}, perr
	}
	if err := validateHelperPlanFields(plan); err != nil {
		return validatedHelperPlan{}, err
	}

	// 6. 重新派生全部路径。
	paths := deriveHelperPaths(selfDir, plan.TargetBasename, plan.Nonce)

	// 7. planPath 必须精确等于派生的 plan 路径。
	if planClean != paths.Plan {
		return validatedHelperPlan{}, fmt.Errorf("计划文件路径 %s 与派生路径 %s 不一致，拒绝执行", planClean, paths.Plan)
	}

	// 8. helper 自身必须精确等于派生的 helper 路径（确认 nonce 与 helper.exe 绑定）。
	if filepath.Clean(selfExe) != paths.Helper {
		return validatedHelperPlan{}, fmt.Errorf("helper 自身 %s 与派生 helper 路径 %s 不一致，拒绝执行", selfExe, paths.Helper)
	}

	return validatedHelperPlan{Plan: plan, Paths: paths}, nil
}

// validateCleanupPlan 校验 cleanup 所在的新 target 与计划严格绑定。
// 它独立于 validateHelperPlan：后者要求 selfExe 是临时 helper.exe，而 cleanup
// 运行在已经替换完成的 target 上。两条路径都必须拒绝任意目录、symlink、非法字段和
// nonce/路径不匹配的 plan，不能把 JSON 中的字段直接传给清理函数。
func validateCleanupPlan(selfExe, planPath string) (validatedHelperPlan, error) {
	selfClean := filepath.Clean(selfExe)
	selfInfo, err := os.Lstat(selfClean)
	if err != nil {
		return validatedHelperPlan{}, fmt.Errorf("无法读取 cleanup 自身元信息 %s: %w", selfExe, err)
	}
	if selfInfo.Mode()&fs.ModeSymlink != 0 {
		return validatedHelperPlan{}, fmt.Errorf("cleanup 自身 %s 是符号链接，拒绝执行", selfExe)
	}
	if !selfInfo.Mode().IsRegular() {
		return validatedHelperPlan{}, fmt.Errorf("cleanup 自身 %s 不是普通文件（mode %s）", selfExe, selfInfo.Mode())
	}

	selfDir := filepath.Dir(selfClean)
	planClean := filepath.Clean(planPath)
	if filepath.Dir(planClean) != selfDir {
		return validatedHelperPlan{}, fmt.Errorf("计划文件 %s 不在 cleanup 自身目录 %s 内，拒绝执行", planPath, selfDir)
	}
	planInfo, err := os.Lstat(planClean)
	if err != nil {
		return validatedHelperPlan{}, fmt.Errorf("无法读取计划文件元信息 %s: %w", planClean, err)
	}
	if planInfo.Mode()&fs.ModeSymlink != 0 {
		return validatedHelperPlan{}, fmt.Errorf("计划文件 %s 是符号链接，拒绝执行", planClean)
	}
	if !planInfo.Mode().IsRegular() {
		return validatedHelperPlan{}, fmt.Errorf("计划文件 %s 不是普通文件（mode %s）", planClean, planInfo.Mode())
	}

	plan, err := readHelperPlan(planClean)
	if err != nil {
		return validatedHelperPlan{}, err
	}
	if err := validateHelperPlanFields(plan); err != nil {
		return validatedHelperPlan{}, err
	}
	paths := deriveHelperPaths(selfDir, plan.TargetBasename, plan.Nonce)
	if planClean != paths.Plan {
		return validatedHelperPlan{}, fmt.Errorf("计划文件路径 %s 与派生路径 %s 不一致，拒绝执行", planClean, paths.Plan)
	}
	if selfClean != paths.Target {
		return validatedHelperPlan{}, fmt.Errorf("cleanup 自身 %s 与派生 target 路径 %s 不一致，拒绝执行", selfExe, paths.Target)
	}
	return validatedHelperPlan{Plan: plan, Paths: paths}, nil
}

// validateHelperPlanFields 校验所有参与路径与进程身份安全合同的 plan 字段。
func validateHelperPlanFields(plan helperPlan) error {
	if plan.Nonce == "" {
		return errors.New("计划缺少 nonce")
	}
	if !isHexNonce(plan.Nonce) {
		// 接受 32 字符（16 字节，与 POSIX generateNonce 一致）或 64 字符 hex。
		return fmt.Errorf("计划 nonce %q 不是合法 hex", plan.Nonce)
	}
	if plan.TargetBasename == "" {
		return errors.New("计划缺少 target_basename")
	}
	if strings.ContainsAny(plan.TargetBasename, `/\\`) {
		return fmt.Errorf("target_basename %q 含路径分隔符", plan.TargetBasename)
	}
	// Parent 身份必须为合法非零值（防降级或缺失：helper 必须据显式身份等待精确父进程实例）。
	if !plan.Parent.Valid() {
		return fmt.Errorf("计划缺少合法父进程身份（PID=%d CreationTime=%d）", plan.Parent.PID, plan.Parent.CreationTime)
	}
	return nil
}

// CleanupHelperTempFiles 删除一次 helper 替换遗留的临时文件（helper.exe / plan / stage / backup）。
// 由 cleanup 命令在 helper 退出后调用。
//
// 严格安全：只删除 (selfDir, targetBasename, nonce) 派生的精确路径，且只删普通文件
// （拒绝 symlink / 目录）；不存在的文件视为已清理（幂等）。绝不按前缀模糊匹配。
// 不删除 result 文件——它供下一次 update --check 展示，由后续更新覆盖或人工查看。
func CleanupHelperTempFiles(selfDir, targetBasename, nonce string) error {
	if selfDir == "" || targetBasename == "" || nonce == "" {
		return errors.New("清理 helper 临时文件的 selfDir/targetBasename/nonce 不能为空")
	}
	paths := deriveHelperPaths(selfDir, targetBasename, nonce)
	var cleanupErr error
	for _, p := range []string{paths.Helper, paths.Plan, paths.Stage, paths.Backup} {
		if err := removeRegularFile(p); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("清理 helper 临时文件失败 %s: %w", p, err))
		}
	}
	return cleanupErr
}

// isHexNonce 报告 s 是否为合法 nonce：32 字符（16 字节，generateNonce 输出）或 64 字符，
// 全部为小写 hex。nonce 仅用于本次替换的文件命名绑定，不参与安全决策（安全走 SHA256/provenance）。
func isHexNonce(s string) bool {
	if len(s) != 32 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
