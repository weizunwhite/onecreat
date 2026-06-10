package boot

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// 隔离用户全局配置:Build() 内部 config.Load() 会读真实的
	// ~/Library/Application Support/reasonix(或 ~/.config/reasonix),开发机上
	// 设过的 planner_model / [[plugins]] 会污染测试(真实发生过:全局
	// planner_model 指向测试夹具里不存在的 provider,3 个 Build 测试齐挂)。
	// 统一指到一次性临时目录,测试只看见自己写的配置。
	if tmp, err := os.MkdirTemp("", "reasonix-test-config-*"); err == nil {
		os.Setenv("REASONIX_CONFIG_DIR", tmp)
	}
	os.Exit(m.Run())
}
