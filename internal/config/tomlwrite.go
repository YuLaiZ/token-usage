package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/YuLaiZ/token-usage/internal/fileutil"
	"github.com/YuLaiZ/token-usage/internal/ui"
	"github.com/pelletier/go-toml/v2"
)

// MarshalConfig 把配置序列化为 TOML(go-toml/v2),层级中性。
// config show 等读取 effective 配置的入口使用它;用户配置写盘仍用 MarshalUserConfig。
//
// raw query 状态不参与 struct 编码,由组合器手工写回:合法 [query] 表与问题态项
// 统一使用基本双引号键名/表头/字符串值;问题态根级标量固定写在既有根级键之后、
// 首个表头之前;普通非 query 表(含 provider_aliases)之后再输出合法 query 表或
// 问题态表,保证 TOML 合法性与确定字节输出。
func MarshalConfig(cfg *Config) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%s", ui.Bi("config must not be nil", "配置不能为 nil"))
	}
	// go-toml 会在 bare key 合法时省略引号。provider_aliases 的 key 是用户输入且
	// 匹配大小写敏感的 provider 名，统一用双引号输出，避免同一表出现混合格式。
	// raw query 载体同样从 struct 编码剥离,由下方组合器手工写回。
	copyCfg := *cfg
	aliases := copyCfg.ProviderAliases
	copyCfg.ProviderAliases = nil
	copyCfg.RawQuery = nil
	copyCfg.RawQueryTopLevelIssues = nil
	data, err := toml.Marshal(&copyCfg)
	if err != nil {
		return nil, err
	}

	// 问题态根级标量/数组:按原始键名字节序写在既有根级键之后、首个表头之前。
	rootIssueNames := make([]string, 0, len(cfg.RawQueryTopLevelIssues))
	for name, issue := range cfg.RawQueryTopLevelIssues {
		if _, isTable := issue.Value.(map[string]any); !isTable {
			rootIssueNames = append(rootIssueNames, name)
		}
	}
	if len(rootIssueNames) > 0 {
		sort.Strings(rootIssueNames)
		var issueLines []byte
		for _, name := range rootIssueNames {
			key, err := marshalTOMLBasicString(name)
			if err != nil {
				return nil, err
			}
			val, err := encodeRawQueryValue(cfg.RawQueryTopLevelIssues[name].Value, name)
			if err != nil {
				return nil, err
			}
			issueLines = append(issueLines, []byte(key+" = "+val+"\n")...)
		}
		data = insertBeforeFirstTableHeader(data, issueLines)
	}

	var b strings.Builder
	b.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteByte('\n')
	}
	if len(aliases) > 0 {
		b.WriteString("[provider_aliases]\n")
		keys := make([]string, 0, len(aliases))
		for key := range aliases {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			quotedKey, err := marshalTOMLBasicString(key)
			if err != nil {
				return nil, err
			}
			quotedValue, err := marshalTOMLBasicString(aliases[key])
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&b, "%s = %s\n", quotedKey, quotedValue)
		}
	}

	// 合法 query 表:整体序列化,内容为空(空段/仅空子表)时连表头一起跳过。
	if cfg.RawQuery != nil {
		block, err := encodeRawQueryTable([]string{"query"}, cfg.RawQuery)
		if err != nil {
			return nil, err
		}
		b.WriteString(block)
	}

	// 问题态表值项:按原始段名字节序,无条件写表头(空表也写,不能被空段规则吞掉)。
	tableIssueNames := make([]string, 0, len(cfg.RawQueryTopLevelIssues))
	for name, issue := range cfg.RawQueryTopLevelIssues {
		if _, isTable := issue.Value.(map[string]any); isTable {
			tableIssueNames = append(tableIssueNames, name)
		}
	}
	sort.Strings(tableIssueNames)
	for _, name := range tableIssueNames {
		table := cfg.RawQueryTopLevelIssues[name].Value.(map[string]any)
		header, err := encodeRawQueryHeader([]string{name})
		if err != nil {
			return nil, err
		}
		b.WriteString(header + "\n")
		content, err := encodeRawQueryTableContent([]string{name}, table)
		if err != nil {
			return nil, err
		}
		b.WriteString(content)
	}
	return []byte(b.String()), nil
}

// insertBeforeFirstTableHeader 把根级键值对行插入到 data 的首个表头行之前;
// data 无表头时追加到尾部(必要时补换行)。
func insertBeforeFirstTableHeader(data, lines []byte) []byte {
	pos := 0
	for pos < len(data) {
		lineEnd := bytes.IndexByte(data[pos:], '\n')
		var line []byte
		next := len(data)
		if lineEnd >= 0 {
			line = data[pos : pos+lineEnd]
			next = pos + lineEnd + 1
		} else {
			line = data[pos:]
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			break
		}
		pos = next
	}
	var out []byte
	if pos == len(data) && len(data) > 0 && data[len(data)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, data[:pos]...)
	out = append(out, lines...)
	out = append(out, data[pos:]...)
	return out
}

// encodeRawQueryTable 序列化一个 raw query 子表(表头 + 内容)。
// 内容为空(无标量键且子表全部为空)时返回空串,调用方跳过该表头。
func encodeRawQueryTable(path []string, table map[string]any) (string, error) {
	content, err := encodeRawQueryTableContent(path, table)
	if err != nil {
		return "", err
	}
	if content == "" {
		return "", nil
	}
	header, err := encodeRawQueryHeader(path)
	if err != nil {
		return "", err
	}
	return header + "\n" + content, nil
}

// encodeRawQueryTableContent 输出表内容:先标量/数组键值对(键名字节序),
// 再按完整表头路径的键名字节序递归输出子表(空子表逐层跳过)。
func encodeRawQueryTableContent(path []string, table map[string]any) (string, error) {
	scalarKeys := make([]string, 0, len(table))
	childKeys := make([]string, 0, len(table))
	children := make(map[string]map[string]any, len(table))
	for k, v := range table {
		if m, ok := v.(map[string]any); ok {
			children[k] = m
			childKeys = append(childKeys, k)
			continue
		}
		scalarKeys = append(scalarKeys, k)
	}
	sort.Strings(scalarKeys)
	sort.Strings(childKeys)

	var b strings.Builder
	for _, k := range scalarKeys {
		key, err := marshalTOMLBasicString(k)
		if err != nil {
			return "", err
		}
		val, err := encodeRawQueryValue(table[k], joinRawQueryPath(path, k))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s = %s\n", key, val)
	}
	for _, k := range childKeys {
		subPath := append(append([]string(nil), path...), k)
		block, err := encodeRawQueryTable(subPath, children[k])
		if err != nil {
			return "", err
		}
		b.WriteString(block)
	}
	return b.String(), nil
}

// encodeRawQueryHeader 输出表头:路径段全部使用基本双引号,不假定 bare key。
func encodeRawQueryHeader(path []string) (string, error) {
	parts := make([]string, len(path))
	for i, p := range path {
		q, err := marshalTOMLBasicString(p)
		if err != nil {
			return "", err
		}
		parts[i] = q
	}
	return "[" + strings.Join(parts, ".") + "]", nil
}

// joinRawQueryPath 构造错误定位用的人类可读路径,如 query.subqueries.mpc。
func joinRawQueryPath(path []string, key string) string {
	return strings.Join(append(append([]string(nil), path...), key), ".")
}

// encodeRawQueryValue 编码标量或数组;数组元素递归,map 元素用 inline table。
func encodeRawQueryValue(v any, path string) (string, error) {
	switch t := v.(type) {
	case []any:
		parts := make([]string, 0, len(t))
		for i, el := range t {
			enc, err := encodeRawQueryValue(el, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return "", err
			}
			parts = append(parts, enc)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		return encodeRawQueryInlineTable(t, path)
	default:
		return encodeRawQueryScalar(v, path)
	}
}

// encodeRawQueryInlineTable 编码数组元素位置的 map 为 TOML inline table。
func encodeRawQueryInlineTable(m map[string]any, path string) (string, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		key, err := marshalTOMLBasicString(k)
		if err != nil {
			return "", err
		}
		val, err := encodeRawQueryValue(m[k], path+"."+k)
		if err != nil {
			return "", err
		}
		parts = append(parts, key+" = "+val)
	}
	return "{" + strings.Join(parts, ", ") + "}", nil
}

// encodeRawQueryScalar 编码 TOML 可表示的标量。集合外运行时类型报出路径与类型并拒绝。
func encodeRawQueryScalar(v any, path string) (string, error) {
	switch t := v.(type) {
	case string:
		return marshalTOMLBasicString(t)
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return formatTOMLFloat(t), nil
	case time.Time:
		return t.Format(time.RFC3339Nano), nil
	case toml.LocalDate:
		return t.String(), nil
	case toml.LocalDateTime:
		return t.String(), nil
	case toml.LocalTime:
		return t.String(), nil
	default:
		return "", fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("cannot serialize value of type %T at %s (not a TOML-representable type)", v, path),
			fmt.Sprintf("%s 处存在无法序列化为 TOML 的值类型 %T", path, v),
		))
	}
}

// formatTOMLFloat 保持浮点标记(1.0 不得写成 1);NaN 与正负无穷用合法 TOML 字面量。
func formatTOMLFloat(f float64) string {
	if math.IsNaN(f) {
		return "nan"
	}
	if math.IsInf(f, 1) {
		return "+inf"
	}
	if math.IsInf(f, -1) {
		return "-inf"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// marshalTOMLBasicString 输出 TOML 基本字符串。JSON 字符串转义是 TOML 基本字符串的有效子集，
// 因此既能强制双引号格式，也能正确保留输入中的双引号和控制字符。
func marshalTOMLBasicString(value string) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// MarshalUserConfig 把用户配置层序列化为 TOML(go-toml/v2)。
// 丢注释 + map 键字典序重排(决策记录);依赖 Config 的 toml tag(T1)。
func MarshalUserConfig(cfg *Config) ([]byte, error) {
	return MarshalConfig(cfg)
}

// WriteUserConfigAtomic 使用全仓统一的完整文件替换 helper。
func WriteUserConfigAtomic(path string, cfg *Config) error {
	data, err := MarshalUserConfig(cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to marshal config", "序列化配置失败"), err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to create config directory", "创建配置目录失败"), err)
	}
	if err := fileutil.ReplaceCompleteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to replace config file", "完整替换配置失败"), err)
	}
	return nil
}
