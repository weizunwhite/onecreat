package builtin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// C3 回归:对 GB18030 文件 grep 且命中 200 上限(提前 return io.EOF)时,解码用的
// io.Pipe 写端 goroutine 必须退出,不能因 pw.Write 永久阻塞而泄漏。重复多次后 goroutine
// 数应回落到基线附近。
func TestGrepGB18030NoGoroutineLeak(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 1000; i++ { // 远超 200 上限,且足够大让写端仍在 io.Copy 时命中 cap
		sb.WriteString("匹配行 你好世界\n")
	}
	gb, err := simplifiedchinese.GB18030.NewEncoder().String(sb.String())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "gb.txt")
	if err := os.WriteFile(f, []byte(gb), 0o644); err != nil {
		t.Fatal(err)
	}

	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		runTool(t, grepTool{}, map[string]any{"path": f, "pattern": "你好"})
	}
	// 给写端 goroutine 拿到 ErrClosedPipe 后退出的调度时间。
	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= before+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutine leak: before=%d after=%d (20 greps over a GB18030 file hitting the 200-match cap)", before, after)
	}
}
