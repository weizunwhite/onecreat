#!/usr/bin/env bash
# 把 dsh sidecar 运行时装配进一个发行目录:
#
#   <dest>/runtime/node/bin/node        锁版本的 Node 运行时(Windows 是 runtime/node/node.exe)
#   <dest>/runtime/dsh/                 OneCreat 的 dsh 组合包(profile + 插件 + 生产依赖闭包)
#
# Go 侧的路径解析对应 internal/engine/dsh/runtime.go:
#   组合包 = 主程序同目录的 runtime/dsh;node = 组合包的 ../node/bin/node。
#
# 用法:scripts/dsh-bundle.sh <dest-dir> <os> <arch>
#   os   = darwin | linux | windows
#   arch = arm64 | amd64(会转成 Node 的 x64)
#
# 本脚本只往 <dest>/runtime 里写东西,不碰 <dest> 下的其它内容 —— 所以 CI 可以拿一个
# 空目录当 dest,单独产出一份 runtime/ 打成 artifact,再喂给别的机器上的 web-build.sh
# (见 .github/workflows/release-web.yml 的 sidecar job 与 web-build.sh 的 DSH_RUNTIME_DIR)。
#
# 环境变量:
#   NODE_VERSION      默认 v22.23.2(dsh 要求 ^22.19.0 || >=24;这里取 22 LTS 线)
#   BUILD_CACHE       Node 压缩包缓存目录,默认 <repo>/build-cache
#
# ⚠️ 跨平台限制:dsh 的依赖闭包里有原生模块(node-pty / koffi /
#    dsh-subprocess-local 的 spawn helper)。在 macOS 上装出来的 node_modules
#    只含 macOS 的原生产物,拷到 Windows/Linux 不能用。所以本脚本只在
#    "目标平台 == 本机平台" 时装配 node_modules;跨平台包由各自平台的 CI 装配。
set -euo pipefail

DEST="${1:?usage: dsh-bundle.sh <dest-dir> <os> <arch>}"
OS="${2:?usage: dsh-bundle.sh <dest-dir> <os> <arch>}"
ARCH="${3:?usage: dsh-bundle.sh <dest-dir> <os> <arch>}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NODE_VERSION="${NODE_VERSION:-v22.23.2}"
CACHE="${BUILD_CACHE:-$ROOT/build-cache}"
mkdir -p "$CACHE"

# Node 的 arch 命名与 Go 不同。
case "$ARCH" in
amd64) NODE_ARCH=x64 ;;
arm64) NODE_ARCH=arm64 ;;
*)
	echo "dsh-bundle: 不支持的 arch $ARCH" >&2
	exit 1
	;;
esac

case "$OS" in
darwin | linux)
	NODE_PKG="node-${NODE_VERSION}-${OS}-${NODE_ARCH}.tar.gz"
	;;
windows)
	NODE_PKG="node-${NODE_VERSION}-win-${NODE_ARCH}.zip"
	;;
*)
	echo "dsh-bundle: 不支持的 os $OS" >&2
	exit 1
	;;
esac

RUNTIME="$DEST/runtime"
mkdir -p "$RUNTIME"

# ---- 1. Node 运行时(下载 + 缓存 + 只留必要文件)----
if [ ! -f "$CACHE/$NODE_PKG" ]; then
	echo "==> 下载 Node ${NODE_VERSION} (${OS}/${NODE_ARCH})"
	curl -fsSL -o "$CACHE/$NODE_PKG.part" "https://nodejs.org/dist/${NODE_VERSION}/${NODE_PKG}"
	mv "$CACHE/$NODE_PKG.part" "$CACHE/$NODE_PKG"
else
	echo "==> Node ${NODE_VERSION} (${OS}/${NODE_ARCH}) 用缓存"
fi

rm -rf "$RUNTIME/node"
tmp="$(mktemp -d)"
if [ "$OS" = windows ]; then
	# Windows runner 的 Git Bash 未必带 unzip,7z 是 GitHub 镜像里一定有的兜底。
	if command -v unzip >/dev/null 2>&1; then
		unzip -q "$CACHE/$NODE_PKG" -d "$tmp"
	elif command -v 7z >/dev/null 2>&1; then
		7z x -o"$tmp" "$CACHE/$NODE_PKG" >/dev/null
	else
		echo "dsh-bundle: 解 Node 压缩包需要 unzip 或 7z,两个都没有" >&2
		exit 1
	fi
	src="$(find "$tmp" -maxdepth 1 -type d -name 'node-*' | head -1)"
	mkdir -p "$RUNTIME/node"
	cp "$src/node.exe" "$RUNTIME/node/node.exe"
else
	tar -xzf "$CACHE/$NODE_PKG" -C "$tmp"
	src="$(find "$tmp" -maxdepth 1 -type d -name 'node-*' | head -1)"
	mkdir -p "$RUNTIME/node/bin"
	cp "$src/bin/node" "$RUNTIME/node/bin/node"
	chmod +x "$RUNTIME/node/bin/node"
fi
rm -rf "$tmp"

# ---- 2. dsh 组合包(源码 + 生产依赖)----
rm -rf "$RUNTIME/dsh"
mkdir -p "$RUNTIME/dsh"
cp "$ROOT/dsh/package.json" "$ROOT/dsh/pnpm-lock.yaml" "$RUNTIME/dsh/"
cp -R "$ROOT/dsh/profiles" "$ROOT/dsh/plugins" "$RUNTIME/dsh/"

# ⚠️ Windows runner 的 Git Bash 报的是 MINGW64_NT-10.0-…,不识别就会被当成 linux,
# 于是"在 Windows 上装 Windows 包"反而走进跨平台分支被跳过。
case "$(uname -s | tr '[:upper:]' '[:lower:]')" in
darwin*) HOST_OS=darwin ;;
mingw* | msys* | cygwin* | windows*) HOST_OS=windows ;;
*) HOST_OS=linux ;;
esac
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
arm64 | aarch64) HOST_ARCH=arm64 ;;
x86_64) HOST_ARCH=amd64 ;;
esac

if [ "$OS" = "$HOST_OS" ] && [ "$ARCH" = "$HOST_ARCH" ]; then
	echo "==> 装配 dsh 生产依赖闭包(hoisted,便于原样打包)"
	# --node-linker=hoisted:装成普通 node_modules 目录树。pnpm 默认的符号链接
	# 布局一打包/解压就断链,发行包必须是实体文件。
	(cd "$RUNTIME/dsh" && pnpm install --prod --frozen-lockfile --node-linker=hoisted --ignore-scripts >/dev/null)
	# 原生模块的 postinstall(node-pty / koffi / spawn helper)得跑,否则运行期缺 .node。
	(cd "$RUNTIME/dsh" && pnpm rebuild >/dev/null 2>&1 || true)
	# 闭包版本一致性闸门。dsh 是 developer preview,官方明说 rc 之间会破坏兼容,混着装
	# 必炸。而升级时只改 package.json 再跑 `pnpm install` 是**不够**的:直接依赖会升,
	# 传递依赖(尤其是被当 peer 解析的那批)会被 pnpm 判定"已满足"而留在旧 rc 上,
	# 装出来是 rc.8 直接依赖 + rc.7 传递依赖的混合体,还能编译通过。2026-08-20 的
	# rc.7→rc.8 升级就这么中过一次(正确做法:删掉 pnpm-lock.yaml 与 node_modules 重装)。
	versions="$(find "$RUNTIME/dsh/node_modules/@deepseek-ai" -maxdepth 2 -name package.json -path '*/dsh-*/package.json' -exec sed -n 's/.*"version": "\(0\.1\.0-rc\.[0-9]*\)".*/\1/p' {} \; | sort -u)"
	if [ "$(echo "$versions" | wc -l | tr -d ' ')" != 1 ]; then
		echo "dsh-bundle: 依赖闭包里混了多个 dsh 版本,拒绝出包:" >&2
		echo "$versions" | sed 's/^/    /' >&2
		echo "  修:rm -rf dsh/node_modules dsh/pnpm-lock.yaml && pnpm -C dsh install,再重跑本脚本。" >&2
		exit 1
	fi
	echo "==> dsh 闭包版本一致:$versions"
else
	echo "⚠️  跨平台包 (${OS}/${ARCH}) 跳过 node_modules 装配:原生模块只能在目标平台上装。" >&2
	echo "    该平台的发行包不含 dsh sidecar,engine=\"dsh\" 在其上不可用(engine=native 照常)。" >&2
	rm -rf "$RUNTIME"
	return 0 2>/dev/null || exit 0
fi

echo "==> dsh sidecar 就绪:$(du -sh "$RUNTIME" | cut -f1)"
