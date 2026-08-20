package dsh

import (
	"testing"

	"go.uber.org/goleak"
)

// 适配器会为每一轮起 goroutine(native 跑内核循环,dsh 等 sidecar 的通知)。
// 忘了收尾的话,泄漏的是「每说一句话漏一个 goroutine」这种慢性病 —— 用 goleak
// 在包级别兜住,而不是指望每条用例自己记得断言。
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
