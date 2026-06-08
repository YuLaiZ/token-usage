package config

import (
	"errors"
	"testing"
)

func TestParseDottedKey(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"data_dir", []string{"data_dir"}},
		{"daemon.poll_interval", []string{"daemon", "poll_interval"}},
		{`provider_aliases."Zhipu AI Coding Plan"`, []string{"provider_aliases", "Zhipu AI Coding Plan"}},
		{`clients.codex.paths.db`, []string{"clients", "codex", "paths", "db"}},
	}
	for _, c := range cases {
		got, err := parseDottedKey(c.in)
		if err != nil {
			t.Fatalf("parseDottedKey(%q) err: %v", c.in, err)
		}
		if len(got) != len(c.want) {
			t.Errorf("parseDottedKey(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDottedKey_Errors(t *testing.T) {
	for _, bad := range []string{`"unclosed`, `a..b`, `a.`, `.a`} {
		if _, err := parseDottedKey(bad); err == nil {
			t.Errorf("parseDottedKey(%q) 应报错", bad)
		}
	}
}

func TestSet_Get_RoundTrip(t *testing.T) {
	cfg := &Config{Clients: map[string]Client{}, Routers: map[string]RouterConfig{}, ProviderAliases: map[string]string{}}
	cases := []struct{ key, val string }{
		{"daemon.poll_interval", "15"},
		{"log.level", "debug"},
		{"log.max_days", "3"},
		{"clients.codex.enabled", "true"},
		{"clients.codex.paths.db", "/custom/db"},
		{"clients.codex.router", "cc_switch"},
		{"routers.cc_switch.db_path", "/r/db"},
		{`provider_aliases."Zhipu AI Coding Plan"`, "Zhipu GLM"},
	}
	for _, c := range cases {
		if err := Set(cfg, c.key, c.val); err != nil {
			t.Fatalf("Set(%q=%q): %v", c.key, c.val, err)
		}
		got, err := Get(cfg, c.key)
		if err != nil {
			t.Fatalf("Get(%q): %v", c.key, err)
		}
		if got != c.val {
			t.Errorf("Get(%q) = %q, want %q", c.key, got, c.val)
		}
	}
}

func TestSet_NewClient(t *testing.T) {
	cfg := &Config{Clients: map[string]Client{}, Routers: map[string]RouterConfig{}, ProviderAliases: map[string]string{}}
	if err := Set(cfg, "clients.newclient.enabled", "true"); err != nil {
		t.Fatalf("Set 新 client: %v", err)
	}
	if !cfg.Clients["newclient"].Enabled {
		t.Error("新 client 应创建并 enabled")
	}
}

func TestSet_DataDirIntercepted(t *testing.T) {
	cfg := &Config{}
	err := Set(cfg, "data_dir", "/new")
	if !errors.Is(err, ErrDataDirNeedsConfirm) {
		t.Fatalf("Set data_dir 应返回 ErrDataDirNeedsConfirm,实际 %v", err)
	}
}

func TestSet_DataDirSameValueDoesNotRequireConfirmation(t *testing.T) {
	cfg := &Config{DataDir: "/same"}
	if err := Set(cfg, "data_dir", "/same"); err != nil {
		t.Fatalf("相同 data_dir 不构成迁移，不应要求确认: %v", err)
	}
}

func TestSet_LogLevelDefaultStoresUserLayerEmpty(t *testing.T) {
	cfg := &Config{}
	if err := Set(cfg, "log.level", "default"); err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != "" {
		t.Errorf("default 应规范化为用户层空值，got %q", cfg.Log.Level)
	}
}

func TestSet_ProviderAliasInitializesNilMap(t *testing.T) {
	cfg := &Config{}
	if err := Set(cfg, `provider_aliases."new provider"`, "Display"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProviderAliases["new provider"]; got != "Display" {
		t.Errorf("nil map 新增 alias 失败，got %q", got)
	}
}

func TestSet_TypeErrors(t *testing.T) {
	cfg := &Config{Clients: map[string]Client{}}
	if err := Set(cfg, "daemon.poll_interval", "not-int"); err == nil {
		t.Error("非 int 应报错")
	}
	if err := Set(cfg, "clients.codex.enabled", "maybe"); err == nil {
		t.Error("非 bool 应报错")
	}
}

func TestSetGet_NilConfigRejected(t *testing.T) {
	if err := Set(nil, "daemon.poll_interval", "30"); err == nil {
		t.Error("Set nil 配置应报错")
	}
	if _, err := Get(nil, "daemon.poll_interval"); err == nil {
		t.Error("Get nil 配置应报错")
	}
}

func TestSetGet_ClientScalarRejectsExtraSegments(t *testing.T) {
	cfg := &Config{Clients: map[string]Client{
		"codex": {Enabled: true, Router: "cc_switch"},
	}}
	for _, key := range []string{
		"clients.codex.enabled.extra",
		"clients.codex.router.extra",
	} {
		if err := Set(cfg, key, "false"); err == nil {
			t.Errorf("Set(%q) 多余段应报错", key)
		}
		if _, err := Get(cfg, key); err == nil {
			t.Errorf("Get(%q) 多余段应报错", key)
		}
	}
}

func TestGet_UnknownPath(t *testing.T) {
	cfg := &Config{Clients: map[string]Client{}}
	for _, bad := range []string{"unknown.x", "daemon.bad", "clients.codex.enabled", "data_dir.foo"} {
		if _, err := Get(cfg, bad); err == nil {
			t.Errorf("Get(%q) 应报错(未知路径)", bad)
		}
	}
}
