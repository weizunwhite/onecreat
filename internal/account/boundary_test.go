package account

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gatewayVars are the three variables that used to be the account state bus.
var gatewayVars = []string{"ONECREAT_GATEWAY_URL", "ONECREAT_GATEWAY_TOKEN", "ONECREAT_TIER"}

// TestNothingElseReadsTheAccountEnvironment is Plan 09's acceptance as a test:
// no package outside this one may read the gateway variables from the process
// environment. The account is an object now — reading it back out of the
// environment is how the state bus grew in the first place.
//
// Writing them is equally banned: the only sanctioned direction is Gateway.Env,
// which projects a session into a subprocess's environment at launch.
//
// A test file may still name them (a test may set up a process that imports
// them), so only non-test sources are scanned.
func TestNothingElseReadsTheAccountEnvironment(t *testing.T) {
	root := filepath.Join("..", "..")
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner of the tree is not this test's business
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "bin", "vendor":
				return filepath.SkipDir
			}
			abs, aerr := filepath.Abs(path)
			if aerr == nil && abs == self {
				return filepath.SkipDir // this package is where they legitimately live
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		text := string(src)
		for _, v := range gatewayVars {
			for _, call := range []string{`os.Getenv("` + v + `")`, `os.Setenv("` + v + `"`, `os.Unsetenv("` + v + `")`} {
				if strings.Contains(text, call) {
					offenders = append(offenders, path+": "+call)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("账号状态又开始走进程环境变量了:\n  %s\n"+
			"改用显式的 *account.Gateway:持有它、更新它、把它传给需要的会话。"+
			"env 只保留两个合法用途 —— account.FromEnv(启动时导入一次)与 Gateway.Env(投影给子进程)。",
			strings.Join(offenders, "\n  "))
	}
}
