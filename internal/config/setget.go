package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ErrDataDirNeedsConfirm 表示 set data_dir 需调用方二次确认(--confirm-migrate)。
var ErrDataDirNeedsConfirm = fmt.Errorf("修改 data_dir 需 --confirm-migrate 确认(usage.db/logs 需手动迁移,且须先停守护进程)")

// Set 按 dotted key 在 cfg(用户配置层,内存)上设置 value。
// value 按目标字段类型推断:bool / int / string。data_dir 返回 ErrDataDirNeedsConfirm。
// 不写文件——调用方拿到改后的 cfg 再 WriteUserConfigAtomic。
func Set(cfg *Config, key, value string) error {
	if cfg == nil {
		return fmt.Errorf("配置不能为 nil")
	}
	segs, err := ParseDottedKey(key)
	if err != nil {
		return err
	}
	if len(segs) == 1 && segs[0] == "data_dir" {
		if cfg.DataDir == value {
			return nil
		}
		return ErrDataDirNeedsConfirm
	}
	return setByPath(cfg, segs, value)
}

// Get 按 dotted key 从 cfg 读值,返回字符串形式。
func Get(cfg *Config, key string) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("配置不能为 nil")
	}
	segs, err := ParseDottedKey(key)
	if err != nil {
		return "", err
	}
	return getByPath(cfg, segs)
}

// ParseDottedKey 按 Set/Get 的 key 规则把 dotted key 解析为段列表：
// 按 '.' 分段,引号内的 '.' 不分段;段支持双引号包裹(含特殊字符)。
// 导出给需要与 Set/Get 口径一致的调用方复用(如按 key 预判写入目标的校验),
// 避免调用方自行 strings.Split 造成引号段解析漂移。
func ParseDottedKey(key string) ([]string, error) {
	var segs []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == '.' && !inQuote:
			if cur.Len() == 0 {
				return nil, fmt.Errorf("dotted key 段为空: %q", key)
			}
			segs = append(segs, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("dotted key 引号未闭合: %q", key)
	}
	if cur.Len() == 0 {
		return nil, fmt.Errorf("dotted key 末段为空: %q", key)
	}
	segs = append(segs, cur.String())
	return segs, nil
}

func setByPath(cfg *Config, segs []string, value string) error {
	switch segs[0] {
	case "daemon":
		if len(segs) != 2 {
			return fmt.Errorf("未知路径: %v", segs)
		}
		switch segs[1] {
		case "poll_interval":
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("poll_interval 需要 int,得到 %q", value)
			}
			cfg.Daemon.PollInterval = n
		case "autostart":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("autostart 需要 bool 值（true/false），得到 %q", value)
			}
			cfg.Daemon.AutoStart = b
		default:
			return fmt.Errorf("未知 daemon 字段: %q", segs[1])
		}
		return nil
	case "log":
		if len(segs) != 2 {
			return fmt.Errorf("未知路径: %v", segs)
		}
		switch segs[1] {
		case "level":
			if value == "default" {
				cfg.Log.Level = ""
			} else {
				cfg.Log.Level = value
			}
		case "dir":
			cfg.Log.Dir = value
		case "max_days":
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("max_days 需要 int,得到 %q", value)
			}
			cfg.Log.MaxDays = n
		default:
			return fmt.Errorf("未知 log 字段: %q", segs[1])
		}
		return nil
	case "clients":
		if len(segs) < 3 {
			return fmt.Errorf("clients 路径需至少 3 段: clients.<name>.<field>")
		}
		name := segs[1]
		if cfg.Clients == nil {
			cfg.Clients = map[string]Client{}
		}
		c, ok := cfg.Clients[name]
		if !ok {
			c = Client{Paths: map[string]string{}}
		}
		switch segs[2] {
		case "enabled":
			if len(segs) != 3 {
				return fmt.Errorf("enabled 需 3 段: clients.<name>.enabled")
			}
			b, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("enabled 需要 bool,得到 %q", value)
			}
			c.Enabled = b
		case "router":
			if len(segs) != 3 {
				return fmt.Errorf("router 需 3 段: clients.<name>.router")
			}
			c.Router = value
		case "paths":
			if len(segs) != 4 {
				return fmt.Errorf("paths 需 4 段: clients.<name>.paths.<key>")
			}
			if c.Paths == nil {
				c.Paths = map[string]string{}
			}
			c.Paths[segs[3]] = value
		default:
			return fmt.Errorf("未知 client 字段: %q", segs[2])
		}
		cfg.Clients[name] = c // map value 不可寻址,回写
		return nil
	case "routers":
		if len(segs) != 3 || segs[2] != "db_path" {
			return fmt.Errorf("routers 路径需 routers.<name>.db_path")
		}
		name := segs[1]
		if cfg.Routers == nil {
			cfg.Routers = map[string]RouterConfig{}
		}
		r, ok := cfg.Routers[name]
		if !ok {
			r = RouterConfig{}
		}
		r.DBPath = value
		cfg.Routers[name] = r
		return nil
	case "provider_aliases":
		if len(segs) != 2 {
			return fmt.Errorf("provider_aliases 需 2 段: provider_aliases.<alias>")
		}
		if cfg.ProviderAliases == nil {
			cfg.ProviderAliases = map[string]string{}
		}
		cfg.ProviderAliases[segs[1]] = value
		return nil
	default:
		return fmt.Errorf("未知顶层段: %q", segs[0])
	}
}

func getByPath(cfg *Config, segs []string) (string, error) {
	switch segs[0] {
	case "data_dir":
		if len(segs) != 1 {
			return "", fmt.Errorf("未知路径: %v", segs)
		}
		return cfg.DataDir, nil
	case "daemon":
		if len(segs) != 2 {
			return "", fmt.Errorf("未知路径: %v", segs)
		}
		switch segs[1] {
		case "poll_interval":
			return strconv.Itoa(cfg.Daemon.PollInterval), nil
		case "autostart":
			return strconv.FormatBool(cfg.Daemon.AutoStart), nil
		default:
			return "", fmt.Errorf("未知 daemon 字段: %q", segs[1])
		}
	case "log":
		if len(segs) != 2 {
			return "", fmt.Errorf("未知路径: %v", segs)
		}
		switch segs[1] {
		case "level":
			return cfg.Log.Level, nil
		case "dir":
			return cfg.Log.Dir, nil
		case "max_days":
			return strconv.Itoa(cfg.Log.MaxDays), nil
		default:
			return "", fmt.Errorf("未知 log 字段: %q", segs[1])
		}
	case "clients":
		if len(segs) < 3 {
			return "", fmt.Errorf("clients 需至少 3 段")
		}
		c, ok := cfg.Clients[segs[1]]
		if !ok {
			return "", fmt.Errorf("未知 client: %q", segs[1])
		}
		switch segs[2] {
		case "enabled":
			if len(segs) != 3 {
				return "", fmt.Errorf("enabled 需 3 段: clients.<name>.enabled")
			}
			return strconv.FormatBool(c.Enabled), nil
		case "router":
			if len(segs) != 3 {
				return "", fmt.Errorf("router 需 3 段: clients.<name>.router")
			}
			return c.Router, nil
		case "paths":
			if len(segs) != 4 {
				return "", fmt.Errorf("paths 需 4 段")
			}
			v, ok := c.Paths[segs[3]]
			if !ok {
				return "", fmt.Errorf("未知 path 键: %q", segs[3])
			}
			return v, nil
		default:
			return "", fmt.Errorf("未知 client 字段: %q", segs[2])
		}
	case "routers":
		if len(segs) != 3 || segs[2] != "db_path" {
			return "", fmt.Errorf("routers 需 routers.<name>.db_path")
		}
		r, ok := cfg.Routers[segs[1]]
		if !ok {
			return "", fmt.Errorf("未知 router: %q", segs[1])
		}
		return r.DBPath, nil
	case "provider_aliases":
		if len(segs) != 2 {
			return "", fmt.Errorf("provider_aliases 需 2 段")
		}
		v, ok := cfg.ProviderAliases[segs[1]]
		if !ok {
			return "", fmt.Errorf("未知 alias: %q", segs[1])
		}
		return v, nil
	default:
		return "", fmt.Errorf("未知顶层段: %q", segs[0])
	}
}
