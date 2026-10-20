package collector

import (
	"context"
	"sync"
	"testing"
)

func TestFilterDSHMessagesDoesNotMutateInput(t *testing.T) {
	day1 := tsMsToDate(1700000000000)
	messages := []parsedMessage{
		{SessionID: "a", FirstSeenTS: 1700000000000},
		{SessionID: "b", FirstSeenTS: 1600000000000},
	}
	filtered := filterDSHMessages(messages, []string{day1})
	if len(filtered) != 1 || filtered[0].SessionID != "a" {
		t.Fatalf("filtered = %+v", filtered)
	}
	if len(messages) != 2 {
		t.Fatalf("input mutated: %+v", messages)
	}
	if len(filterDSHMessages(messages, nil)) != 2 {
		t.Fatalf("empty dates must mean full set")
	}
}

func TestFilterDSHObsoleteDoesNotMutateInput(t *testing.T) {
	day1 := tsMsToDate(1700000000000)
	items := []parsedObsolete{
		{ID: "a", TS: 1700000000000},
		{ID: "b", TS: 1600000000000},
	}
	filtered := filterDSHObsolete(items, []string{day1})
	if len(filtered) != 1 || filtered[0].ID != "a" {
		t.Fatalf("filtered = %+v", filtered)
	}
	if len(items) != 2 {
		t.Fatalf("input mutated: %+v", items)
	}
}

func TestDSHCacheHitAndInvalidate(t *testing.T) {
	home := newDSHHome(t)
	first := dshHeaderJSONCwd(4, "session-cache", "/tmp/c") +
		`{"type":"assistant/message","seq":1,"time":1700000000000,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `,"message":{"id":"m1","source":{"provider":"p","model":"m"}}}}` + "\n"
	writeDSHSession(t, home, "session-cache", "/tmp/c", first, dshFixtureOpts{})

	collector := newDSHTestCollector(t, home)
	r1, err := collector.Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || len(r1.Messages) != 1 {
		t.Fatalf("first collect: messages=%d err=%v", len(r1.Messages), err)
	}

	second := first +
		`{"type":"assistant/message","seq":2,"time":1700000001000,"data":{"turn":1,"step":2,"usage":` + dshUsage250 + `,"message":{"id":"m2","source":{"provider":"p","model":"m"}}}}` + "\n"
	writeDSHSession(t, home, "session-cache", "/tmp/c", second, dshFixtureOpts{})

	r2, err := collector.Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || len(r2.Messages) != 2 {
		t.Fatalf("after append: messages=%d err=%v", len(r2.Messages), err)
	}

	r3, err := collector.Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || len(r3.Messages) != 2 {
		t.Fatalf("cache hit: messages=%d err=%v", len(r3.Messages), err)
	}
}

func TestDSHCollectDateFilter(t *testing.T) {
	home := newDSHHome(t)
	day1 := int64(1700000000000)
	day2 := day1 + 24*3600*1000
	content := dshHeaderJSONCwd(4, "session-date", "/tmp/d") +
		`{"type":"assistant/message","seq":1,"time":` + itoa64(day1) + `,"data":{"turn":1,"step":1,"usage":` + dshUsage100 + `,"message":{"id":"m1","source":{"provider":"p","model":"m"}}}}` + "\n" +
		`{"type":"assistant/message","seq":2,"time":` + itoa64(day2) + `,"data":{"turn":1,"step":2,"usage":` + dshUsage250 + `,"message":{"id":"m2","source":{"provider":"p","model":"m"}}}}` + "\n"
	writeDSHSession(t, home, "session-date", "/tmp/d", content, dshFixtureOpts{})

	collector := newDSHTestCollector(t, home)
	all, err := collector.Collect(context.Background(), CollectRequest{}, nil)
	if err != nil || len(all.Messages) != 2 || len(all.Sessions) != 1 {
		t.Fatalf("all: messages=%d sessions=%d err=%v", len(all.Messages), len(all.Sessions), err)
	}

	filtered, err := collector.Collect(context.Background(), CollectRequest{Dates: []string{tsMsToDate(day1)}}, nil)
	if err != nil {
		t.Fatalf("filtered collect: %v", err)
	}
	if len(filtered.Messages) != 1 || filtered.Messages[0].TS != day1 {
		t.Fatalf("filtered messages = %+v", filtered.Messages)
	}
	if len(filtered.Sessions) != 1 {
		t.Fatalf("sessions must not be date-filtered: %+v", filtered.Sessions)
	}
}

func TestDSHCollectConcurrent(t *testing.T) {
	home := newDSHHome(t)
	writeDSHSession(t, home, "session-a", "/tmp/a", dshSessionWithMessage("session-a", "/tmp/a"), dshFixtureOpts{})
	collector := newDSHTestCollector(t, home)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := collector.Collect(context.Background(), CollectRequest{}, nil); err != nil {
				t.Errorf("concurrent collect: %v", err)
			}
		}()
	}
	wg.Wait()
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
