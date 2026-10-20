package collector

import (
	"context"
	"fmt"
	"testing"
)

func setupDSHBenchHome(b *testing.B) string {
	b.Helper()
	home := newDSHHome(b)
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("session-%03d", i)
		cwd := fmt.Sprintf("/tmp/project-%03d", i)
		content := dshHeaderJSONCwd(4, id, cwd) +
			`{"type":"user/message","seq":0,"time":1700000000000,"data":{}}` + "\n" +
			`{"type":"assistant/message","seq":1,"time":1700000001000,"data":{"turn":1,"step":1,"usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150},"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n"
		writeDSHSession(b, home, id, cwd, content, dshFixtureOpts{Compression: "zstd", Frames: 2})
	}
	return home
}

// 冷路径：每轮新建 collector，强制重新发现、解码与归约。
func BenchmarkDSHCollect100SessionsCold(b *testing.B) {
	home := setupDSHBenchHome(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		collector := newDSHTestCollector(b, home)
		if _, err := collector.Collect(context.Background(), CollectRequest{}, nil); err != nil {
			b.Fatalf("Collect: %v", err)
		}
	}
}

// 热路径：同一 collector 复用解析缓存（daemon 周期的常态）。
func BenchmarkDSHCollect100SessionsHot(b *testing.B) {
	home := setupDSHBenchHome(b)
	collector := newDSHTestCollector(b, home)
	if _, err := collector.Collect(context.Background(), CollectRequest{}, nil); err != nil {
		b.Fatalf("warmup: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := collector.Collect(context.Background(), CollectRequest{}, nil); err != nil {
			b.Fatalf("Collect: %v", err)
		}
	}
}
