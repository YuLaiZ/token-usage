package collector

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// F34：缓存命中时仍返回撤销候选（模拟「首轮事务失败、次轮重试」）。
func TestFixtureF34CacheReplaysObsolete(t *testing.T) {
	home := newDSHHome(t)
	content := dshHeaderJSONCwd(4, "session-f34", "/tmp/f34") +
		`{"type":"assistant/message","seq":1,"time":1700000000000,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `,"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n" +
		`{"type":"assistant/message","seq":2,"time":1700000001000,"data":{"turn":1,"step":1,"usage":` + dshUsageZero + `,"message":{"id":"m","source":{"provider":"p","model":"m"}}}}` + "\n"
	writeDSHSession(t, home, "session-f34", "/tmp/f34", content, dshFixtureOpts{})

	collector := newDSHTestCollector(t, home)
	first, err := collector.Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || len(first.DSHObsolete) != 1 {
		t.Fatalf("first: obsolete=%d err=%v", len(first.DSHObsolete), err)
	}
	second, err := collector.Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || len(second.DSHObsolete) != 1 {
		t.Fatalf("second (cache hit): obsolete=%d err=%v", len(second.DSHObsolete), err)
	}
	if second.DSHObsolete[0].ID != first.DSHObsolete[0].ID {
		t.Fatalf("obsolete id drifted: %q vs %q", second.DSHObsolete[0].ID, first.DSHObsolete[0].ID)
	}
}

// F38：不同 TZ 的子进程对同一 TS 得到不同日期，证明 Date 由进程时区现算。
func TestFixtureF38TimezoneAcrossProcess(t *testing.T) {
	const ts = int64(1700000000000)
	if os.Getenv("DSH_TZ_CHILD") == "1" {
		fmt.Printf("DSH_DATE=%s\n", tsMsToDate(ts))
		return
	}

	shanghai := runDSHTZChild(t, "Asia/Shanghai")
	losAngeles := runDSHTZChild(t, "America/Los_Angeles")
	if shanghai == "" || losAngeles == "" {
		t.Fatalf("child did not report date: %q / %q", shanghai, losAngeles)
	}
	if shanghai == losAngeles {
		t.Fatalf("TZ did not change date: both %q", shanghai)
	}
}

func runDSHTZChild(t *testing.T, tz string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestFixtureF38TimezoneAcrossProcess")
	cmd.Env = append(os.Environ(), "DSH_TZ_CHILD=1", "TZ="+tz)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child %s: %v\n%s", tz, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "DSH_DATE=") {
			return strings.TrimPrefix(line, "DSH_DATE=")
		}
	}
	return ""
}
