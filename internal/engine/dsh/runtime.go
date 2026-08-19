package dsh

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"reasonix/internal/config"
)

// launchSpec 是一次 sidecar 启动的完整解析结果。
type launchSpec struct {
	// Bin 是要执行的程序(通常是 node,打包形态下是发行包内置的 node)。
	Bin string
	// Args 是启动参数(bin.js + profile 绝对路径)。
	Args []string
	// RuntimeDir 是 dsh 组合包目录(node_modules/profiles 所在处)。
	RuntimeDir string
}

// dshBinRelPath 是组合包内 JSON-RPC bin 的相对路径(由 dsh-sdk-jsonrpc-demo 提供)。
const dshBinRelPath = "node_modules/@deepseek-ai/dsh-sdk-jsonrpc-demo/lib/bin.js"

// defaultProfile 是组合包内默认 profile 的相对路径。
const defaultProfile = "profiles/onecreat.cordis.yml"

// resolveLaunch 解析出"用什么、在哪、带什么参数"拉起 dsh sidecar。
//
// 解析顺序(先命中先用):
//  1. [dsh].runtime_dir 显式配置;
//  2. 主程序同目录的 runtime/dsh(发行包布局,见 scripts/web-build.sh);
//  3. 从当前工作目录逐级向上找 dsh/(开发仓库布局)。
//
// node 解释器:[dsh].bin_path > 组合包内置 runtime bin(打包形态)> PATH 里的 node。
func resolveLaunch(cfg config.DSHConfig) (launchSpec, error) {
	dir, err := resolveRuntimeDir(cfg.RuntimeDir)
	if err != nil {
		return launchSpec{}, err
	}
	profile := strings.TrimSpace(cfg.Profile)
	if profile == "" {
		profile = defaultProfile
	}
	profilePath := profile
	if !filepath.IsAbs(profilePath) {
		profilePath = filepath.Join(dir, profile)
	}
	if _, err := os.Stat(profilePath); err != nil {
		return launchSpec{}, fmt.Errorf("dsh 引擎:找不到 profile %s", profilePath)
	}
	binJS := filepath.Join(dir, dshBinRelPath)
	if _, err := os.Stat(binJS); err != nil {
		return launchSpec{}, fmt.Errorf("dsh 引擎:组合包依赖未安装(缺 %s),在 %s 里跑一次 `pnpm install`", dshBinRelPath, dir)
	}
	node, err := resolveNode(cfg.BinPath, dir)
	if err != nil {
		return launchSpec{}, err
	}
	args := cfg.Args
	if len(args) == 0 {
		args = []string{binJS, profilePath}
	}
	return launchSpec{Bin: node, Args: args, RuntimeDir: dir}, nil
}

// resolveRuntimeDir 找组合包目录。
func resolveRuntimeDir(configured string) (string, error) {
	if d := strings.TrimSpace(configured); d != "" {
		if _, err := os.Stat(d); err != nil {
			return "", fmt.Errorf("dsh 引擎:[dsh].runtime_dir %q 不存在", d)
		}
		return d, nil
	}
	// 发行包形态:主程序旁边的 runtime/dsh。
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		cand := filepath.Join(filepath.Dir(exe), "runtime", "dsh")
		if isRuntimeDir(cand) {
			return cand, nil
		}
	}
	// 开发形态:从 cwd 逐级向上找 dsh/。
	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; ; {
			cand := filepath.Join(dir, "dsh")
			if isRuntimeDir(cand) {
				return cand, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", errors.New("dsh 引擎:找不到 dsh 组合包目录(设 [dsh].runtime_dir,或在仓库根跑 `pnpm -C dsh install`)")
}

// isRuntimeDir 判断一个目录像不像 dsh 组合包(有 profile 就算)。
func isRuntimeDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, defaultProfile))
	return err == nil
}

// resolveNode 找 node 解释器。
func resolveNode(configured, runtimeDir string) (string, error) {
	if p := strings.TrimSpace(configured); p != "" {
		if filepath.IsAbs(p) {
			if _, err := os.Stat(p); err != nil {
				return "", fmt.Errorf("dsh 引擎:[dsh].bin_path %q 不存在", p)
			}
			return p, nil
		}
		return exec.LookPath(p)
	}
	// 发行包内置的 node(与组合包同级的 node/bin/node)。
	for _, rel := range []string{
		filepath.Join("..", "node", "bin", "node"),
		filepath.Join("..", "node", "node.exe"),
	} {
		cand := filepath.Join(runtimeDir, rel)
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return "", errors.New("dsh 引擎:PATH 里没有 node,且发行包未内置 node 运行时;装 Node 20+ 或设 [dsh].bin_path")
	}
	return node, nil
}
