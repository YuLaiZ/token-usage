package config

import (
	"strings"
	"testing"
)

// TestRefreshMarshalOmittedWhenZero:两字段全零时 [refresh] 段整体省略——
// 旧配置缺段在「原样保存」时不产生任何写盘差异(读取不补默认写盘)。
func TestRefreshMarshalOmittedWhenZero(t *testing.T) {
	data, err := MarshalUserConfig(&Config{DataDir: "~/.token-usage"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "refresh") {
		t.Errorf("全零 refresh 不得写出 [refresh] 段:\n%s", data)
	}
}

// TestRefreshMarshalAndRoundTrip:非零值写出 [refresh] 段,解析回读一致;
// 单边配置(只写 dashboard_interval)不丢 watch 语义(0=默认)。
func TestRefreshMarshalAndRoundTrip(t *testing.T) {
	data, err := MarshalUserConfig(&Config{
		DataDir: "~/.token-usage",
		Refresh: RefreshConfig{DashboardInterval: 20, WatchInterval: 45},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[refresh]", "dashboard_interval = 20", "watch_interval = 45"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("TOML 应含 %q:\n%s", want, data)
		}
	}
	parsed, err := ParseUserConfig(data)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if parsed.Refresh.DashboardInterval != 20 || parsed.Refresh.WatchInterval != 45 {
		t.Errorf("round-trip 应为 20/45,实际 %+v", parsed.Refresh)
	}

	// 单边配置:watch 缺省省略(omitempty),读回 0(默认语义)。
	partial, err := MarshalUserConfig(&Config{
		DataDir: "~/.token-usage",
		Refresh: RefreshConfig{DashboardInterval: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(partial), "watch_interval") {
		t.Errorf("零值 watch_interval 应省略:\n%s", partial)
	}
	parsed2, err := ParseUserConfig(partial)
	if err != nil {
		t.Fatal(err)
	}
	if parsed2.Refresh.DashboardInterval != 20 || parsed2.Refresh.WatchInterval != 0 {
		t.Errorf("单边 round-trip 应为 20/0,实际 %+v", parsed2.Refresh)
	}
}

// TestRefreshSetGet:config set/get 的 refresh 段路径;非法字段与非法值拒绝。
func TestRefreshSetGet(t *testing.T) {
	cfg := &Config{}
	if err := Set(cfg, "refresh.dashboard_interval", "20"); err != nil {
		t.Fatal(err)
	}
	if err := Set(cfg, "refresh.watch_interval", "60"); err != nil {
		t.Fatal(err)
	}
	if cfg.Refresh.DashboardInterval != 20 || cfg.Refresh.WatchInterval != 60 {
		t.Errorf("set 后应为 20/60,实际 %+v", cfg.Refresh)
	}
	for _, key := range []string{"refresh.dashboard_interval", "refresh.watch_interval"} {
		v, err := Get(cfg, key)
		if err != nil {
			t.Fatalf("get %s 失败: %v", key, err)
		}
		want := "20"
		if key == "refresh.watch_interval" {
			want = "60"
		}
		if v != want {
			t.Errorf("get %s 应为 %s,实际 %s", key, want, v)
		}
	}
	if _, err := Get(cfg, "refresh.nosuch"); err == nil {
		t.Error("未知 refresh 字段应拒绝")
	}
	if err := Set(cfg, "refresh.dashboard_interval", "abc"); err == nil {
		t.Error("非整数应拒绝")
	}
	if err := Set(cfg, "refresh", "x"); err == nil {
		t.Error("refresh 段路径必须两段")
	}
}

// TestDefaultTemplateCarriesRefreshSection:默认模板显式给出 30/30(与
// daemon.poll_interval 同样式,新装机用户在文件中可见产品缺省)。
func TestDefaultTemplateCarriesRefreshSection(t *testing.T) {
	tpl := DefaultConfigTemplate()
	for _, want := range []string{"[refresh]", "dashboard_interval = 30", "watch_interval = 30"} {
		if !strings.Contains(tpl, want) {
			t.Errorf("默认模板应含 %q", want)
		}
	}
	parsed, err := ParseUserConfig([]byte(tpl))
	if err != nil {
		t.Fatalf("默认模板必须可解析: %v", err)
	}
	if parsed.Refresh.DashboardInterval != 30 || parsed.Refresh.WatchInterval != 30 {
		t.Errorf("模板 refresh 应为 30/30,实际 %+v", parsed.Refresh)
	}
}
