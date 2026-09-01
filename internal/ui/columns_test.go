package ui

import (
	"reflect"
	"strings"
	"testing"
)

// 默认输出列:七个指标、当前固定顺序、不含 cache_create;返回独立副本。
func TestDefaultOutputColumns(t *testing.T) {
	want := []string{"requests", "input", "output", "cache_read", "reasoning", "total", "cache_hit"}
	cols := DefaultOutputColumns()
	if !reflect.DeepEqual(cols, want) {
		t.Errorf("DefaultOutputColumns() = %v, want %v", cols, want)
	}
	cols[0] = "mutated"
	if again := DefaultOutputColumns(); again[0] != "requests" {
		t.Errorf("默认列应为独立副本,再次获取被污染: %v", again)
	}
}

// 候选指标顺序:默认七列在前,cache_create 殿后,共 8 项且无重复。
func TestOutputMetricIDsCandidateOrder(t *testing.T) {
	want := []string{"requests", "input", "output", "cache_read", "reasoning", "total", "cache_hit", "cache_create"}
	ids := OutputMetricIDs()
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("OutputMetricIDs() = %v, want %v", ids, want)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("候选指标 %q 重复", id)
		}
		seen[id] = true
	}
	ids[0] = "mutated"
	if again := OutputMetricIDs(); again[0] != "requests" {
		t.Errorf("候选列表应为独立副本,再次获取被污染: %v", again)
	}
}

// 每个候选 ID 都有双语两行表头(en\nzh 形态);未知 ID 不被接受;cache_create 表头就位。
func TestOutputMetricHeader(t *testing.T) {
	for _, id := range OutputMetricIDs() {
		header, ok := OutputMetricHeader(id)
		if !ok {
			t.Errorf("OutputMetricHeader(%q) 不应缺失", id)
			continue
		}
		lines := strings.Split(header, "\n")
		if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
			t.Errorf("OutputMetricHeader(%q) = %q, 应为两行双语表头", id, header)
		}
	}
	if _, ok := OutputMetricHeader("nope"); ok {
		t.Error("未知 ID 不应有表头")
	}
	if h, _ := OutputMetricHeader("cache_create"); h != HeaderLines("Cache Create", "缓存创建") {
		t.Errorf("cache_create 表头 = %q, want %q", h, HeaderLines("Cache Create", "缓存创建"))
	}
	// 表头常量与单行列名常量同源同译文。
	if h, _ := OutputMetricHeader("requests"); h != HRequests {
		t.Errorf("requests 表头 = %q, want %q", h, HRequests)
	}
}

// ID 列表文本用于 querydef 错误信息的允许集合:与候选顺序一致、逗号分隔。
func TestOutputColumnIDList(t *testing.T) {
	got := OutputColumnIDList()
	want := "requests, input, output, cache_read, reasoning, total, cache_hit, cache_create"
	if got != want {
		t.Errorf("OutputColumnIDList() = %q, want %q", got, want)
	}
}
