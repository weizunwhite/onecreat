package control

import (
	"os"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// 隔离用户全局配置:config.Load() 会读 ~/Library/Application Support/reasonix
	// (或 ~/.config/reasonix),开发机上装过的 [[plugins]] / default_model 会污染
	// 测试(真实发生过:全局 hardware 插件让 RemoveMCPServer 测试误判)。
	// 统一指到一次性临时目录,测试只看见自己写的配置。
	// (VerifyTestMain 内部 os.Exit,defer 不会执行;临时目录交给系统回收。)
	if tmp, err := os.MkdirTemp("", "reasonix-test-config-*"); err == nil {
		os.Setenv("REASONIX_CONFIG_DIR", tmp)
	}
	goleak.VerifyTestMain(m)
}
