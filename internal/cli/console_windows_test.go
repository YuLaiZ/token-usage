//go:build windows

package cli

import (
	"os"
	"testing"
)

// 标准三流全部替换为管道时不得切换代码页（输出重定向/管道场景字节恒为 UTF-8）。
func TestInitConsoleAllRedirectedReturnsNil(t *testing.T) {
	origIn, origOut, origErr := os.Stdin, os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin, os.Stdout, os.Stderr = r, r, r
	defer func() {
		os.Stdin, os.Stdout, os.Stderr = origIn, origOut, origErr
		r.Close()
		w.Close()
	}()

	if restore := InitConsole(); restore != nil {
		restore()
		t.Fatal("全重定向时 InitConsole 应返回 nil，不得切换代码页")
	}
}
