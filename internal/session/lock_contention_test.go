package session

// Windows 上 `O_CREATE|O_EXCL` 撞到「已经有人持锁」时,报的**不是** EEXIST。
//
// 释放方 os.Remove 之后文件会短暂进入 delete-pending 状态,这期间 CreateFile 返回
// ERROR_ACCESS_DENIED。原来的判断只认 os.IsExist,于是这条路径直接放弃取锁、
// fail-open 无锁执行 —— **锁在 Windows 上等于不存在**,两个 owner 互相覆盖写回。
//
// 真机证据(CI run 32441069610 / attempt 2,windows-latest):
//
//	WARN session: 未能取得索引锁,本次更新在无锁下进行
//	  err="open ...\.sessions.lock: Access is denied."
//	--- FAIL: TestTwoOwnersDoNotLoseEachOthersRecords
//	    应有 80 条记录,拿到 79 —— 两个 owner 互相覆盖了写回
//
// ⚠️ 文件名不能叫 lock_windows_test.go —— `_windows` 是 Go 的构建约束后缀,那样这个
// 文件只在 Windows 上编译,恰好与它存在的理由相反。第一版就是这么写的,`go test` 报
// 「no tests to run」才发现。
//
// 这条用例测的是**分类函数**而不是真去开文件:Windows 那条分支必须在每个平台上都跑得到,
// 否则它只有 Windows CI 才验证得了 —— 而这个 bug 正是因为本地只做交叉编译、没跑过
// Windows 运行期才漏掉的。同一个教训在 config.envKey 那里已经吃过一次。

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

func TestLockContendedClassifiesWindowsAccessDenied(t *testing.T) {
	// Unix 的形态:EEXIST。两个平台都必须认。
	if !contendedOn("windows", fs.ErrExist) {
		t.Error("EEXIST 在 windows 上应视为「有人持锁」")
	}
	if !contendedOn("linux", fs.ErrExist) {
		t.Error("EEXIST 在 unix 上应视为「有人持锁」")
	}

	// Windows 的形态:ERROR_ACCESS_DENIED(delete-pending 窗口)。
	denied := &os.PathError{Op: "open", Path: "x", Err: syscall.EACCES}
	if !contendedOn("windows", denied) {
		t.Error("Access denied 在 windows 上必须重试 —— " +
			"否则释放锁的那一瞬间,另一个 owner 会直接 fail-open,锁形同虚设")
	}
	// 但在 unix 上,权限错误是真错误:目录只读之类,重试 2 秒没有意义。
	if contendedOn("linux", denied) {
		t.Error("unix 上的权限错误不该被当成锁竞争 —— 那会把一个真实故障拖成静默 fail-open")
	}

	// 其它错误哪个平台都不该重试。
	if contendedOn("windows", errors.New("disk on fire")) {
		t.Error("无关错误不该被当成锁竞争")
	}
}
