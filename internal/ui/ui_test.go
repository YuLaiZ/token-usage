package ui

import "testing"

func TestBiFormat(t *testing.T) {
	if got := Bi("Start the daemon", "后台启动守护进程"); got != "Start the daemon / 后台启动守护进程" {
		t.Fatalf("Bi = %q", got)
	}
	if got := Bi("", ""); got != " / " {
		t.Fatalf("empty Bi = %q", got)
	}
}
