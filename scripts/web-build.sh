#!/usr/bin/env bash
# Web 模式(主分发形态)打包脚本:一台机器交叉编译出全部平台的发行包。
#
# 与 desktop-build.sh(Wails,CGO/WebKit,需逐平台原生 runner)不同,Web 模式是纯 Go,
# 所以这里不需要 wails / NSIS / hdiutil,也不需要公证——一条命令出 mac/win/linux 全套。
#
# 发行包布局(解压即用,硬件 MCP 必须和主程序同目录,见 desktop/app.go resolveHardwareMCP):
#   onecreat-web-<os>-<arch>/
#     onecreat-web(.exe)            主程序:起本地 HTTP 服务 + 自动开浏览器
#     onecreat-hardware-mcp(.exe)   硬件助手(Arduino/ESP-IDF/PlatformIO MCP)
#     README.txt                    怎么启动 / macOS 首次放行 / 端口参数
#
# 产物落在 <repo>/dist/:
#   onecreat-web-<os>-<arch>.tar.gz   (darwin / linux)
#   onecreat-web-<os>-<arch>.zip      (windows)
#   SHA256SUMS                        (target=all 时生成)
#
# 用法:
#   scripts/web-build.sh all v1.2.0            # 全平台
#   scripts/web-build.sh darwin/arm64 v1.2.0   # 单平台(本机点测用)
# 环境变量:
#   SKIP_FRONTEND=1   跳过 pnpm install/build(前端 dist 已是最新时省时间)
#   TARGETS="..."     覆盖 all 的平台列表
set -euo pipefail

PLATFORM="${1:?usage: web-build.sh <os/arch|all> <version>}"
VERSION="${2:?usage: web-build.sh <os/arch|all> <version>}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NAME="onecreat-web"
ALL_TARGETS="${TARGETS:-darwin/arm64 darwin/amd64 windows/amd64 linux/amd64 linux/arm64}"

# defaultAccountMode=platform:正式打包版默认平台模式(强制登录 + 走平台网关),与桌面版打包一致;
# dev/裸 go build 不注入 → 本地免登录。见 desktop/accounts_app.go platformAccountEnabled。
LDFLAGS="-s -w -X main.version=${VERSION} -X main.defaultAccountMode=platform"
MCP_LDFLAGS="-s -w -X main.version=${VERSION}"

if [ "$PLATFORM" = all ]; then
	targets="$ALL_TARGETS"
else
	targets="$PLATFORM"
fi

# 前端 bundle 内嵌进二进制(desktop/main.go go:embed frontend/dist),全平台共用,只构建一次。
if [ "${SKIP_FRONTEND:-0}" != 1 ]; then
	echo "==> frontend: pnpm install + build"
	(cd "$ROOT/desktop/frontend" && pnpm install --frozen-lockfile && pnpm build)
else
	echo "==> frontend: SKIP_FRONTEND=1,沿用现有 desktop/frontend/dist"
fi
[ -f "$ROOT/desktop/frontend/dist/index.html" ] || {
	echo "desktop/frontend/dist/index.html 不存在,前端没构建成功" >&2
	exit 1
}

STAGE="$ROOT/stage-web"
rm -rf "$STAGE"
mkdir -p "$STAGE" "$ROOT/dist"

write_readme() {
	local dir="$1" os="$2"
	local exe="onecreat-web"
	[ "$os" = windows ] && exe="onecreat-web.exe"
	{
		echo "OneCreat ${VERSION}(Web 模式)"
		echo
		echo "启动:双击 ${exe}(或在终端运行 ./${exe}),它会在本机起一个服务并自动打开浏览器。"
		echo "关掉终端窗口 / Ctrl-C 即退出。AI agent 完整跑在你这台电脑上,串口、烧录照常可用。"
		echo
		echo "常用参数:"
		echo "  --port 3700        改端口(默认 3700;若被别的程序占用会自动向上换端口)"
		echo "  --no-open          不自动开浏览器(终端会打印带 token 的链接)"
		echo "  --workspace <目录>  启动时切到指定项目目录"
		echo
		echo "再次双击会怎样:如果 OneCreat 已经在运行(比如你只是关掉了浏览器标签),再次双击"
		echo "会直接帮你把浏览器打开到已经在跑的那个实例,而不是再起一个、也不会报「端口被占」。"
		echo
		echo "如何退出:点右上角的电源图标「退出 OneCreat」,或直接关掉这个终端窗口 / 按 Ctrl-C。"
		echo "退出后本地服务会停止,后台的代码索引进程也会一并清理干净。"
		echo
		echo "如何更新:启动后若有新版本,页面顶部会出现更新提示,点它去下载页拿新版覆盖即可。"
		echo "(Web 版只提示、不自动替换自身;检查失败时静默,不打扰。)"
		echo
		case "$os" in
		darwin)
			echo "macOS 首次运行若提示「无法打开,因为无法验证开发者」:"
			echo "  方法一:在访达里右键 ${exe} → 打开 → 打开;"
			echo "  方法二:终端运行  xattr -dr com.apple.quarantine \"\$(pwd)\"  (在解压出的目录里执行)。"
			echo
			;;
		windows)
			echo "Windows 若 SmartScreen 拦截:点「更多信息」→「仍要运行」。"
			echo "双击会先出现一个黑色终端窗口,这是正常的,浏览器随后自动打开;不要关这个窗口。"
			echo
			;;
		esac
		echo "onecreat-hardware-mcp 是硬件助手,必须和主程序放在同一目录,不要单独移动。"
		echo "不要把带 token 的链接发给别人:它只对本次启动有效,而且谁拿到谁就能操作你的电脑。"
	} >"$dir/README.txt"
}

for t in $targets; do
	os="${t%/*}"
	arch="${t#*/}"
	ext=""
	[ "$os" = windows ] && ext=".exe"
	pkg="${NAME}-${os}-${arch}"
	dir="$STAGE/$pkg"
	mkdir -p "$dir"

	echo "==> build ${pkg}"
	(cd "$ROOT/desktop" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
		go build -tags web -ldflags "$LDFLAGS" -o "$dir/onecreat-web${ext}" .)
	(cd "$ROOT" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
		go build -ldflags "$MCP_LDFLAGS" -o "$dir/onecreat-hardware-mcp${ext}" ./cmd/reasonix-hardware-mcp)
	write_readme "$dir" "$os"

	echo "==> package ${pkg}"
	if [ "$os" = windows ]; then
		rm -f "$ROOT/dist/${pkg}.zip"
		(cd "$STAGE" && zip -qr "$ROOT/dist/${pkg}.zip" "$pkg")
	else
		# COPYFILE_DISABLE:macOS 上打 tar 不带 ._* 资源叉文件。
		COPYFILE_DISABLE=1 tar -czf "$ROOT/dist/${pkg}.tar.gz" -C "$STAGE" "$pkg"
	fi
done

rm -rf "$STAGE"

if [ "$PLATFORM" = all ]; then
	echo "==> checksums"
	(cd "$ROOT/dist" && if command -v sha256sum >/dev/null 2>&1; then
		sha256sum ${NAME}-*.tar.gz ${NAME}-*.zip >SHA256SUMS
	else
		shasum -a 256 ${NAME}-*.tar.gz ${NAME}-*.zip >SHA256SUMS
	fi)

	# 更新清单 latest.json:客户端启动时会 GET 它比对版本(见 desktop/updater_web.go)。
	# assets 的 key 是 "<os>-<arch>"(与 update.PlatformKey 一致),value 是发行包直链。
	# 基址默认阿里云 nginx 的 /onecreat/(UPDATE_CHANNEL),可用 RELEASE_BASE_URL 覆盖;
	# downloadPage(人可读的下载落地页)可用 RELEASE_DOWNLOAD_PAGE 覆盖。
	echo "==> latest.json"
	BASE_URL="${RELEASE_BASE_URL:-http://47.95.176.214/onecreat}"
	BASE_URL="${BASE_URL%/}" # 去掉可能的结尾斜杠
	DOWNLOAD_PAGE="${RELEASE_DOWNLOAD_PAGE:-${BASE_URL}/}"
	{
		printf '{\n'
		printf '  "version": "%s",\n' "$VERSION"
		printf '  "downloadPage": "%s",\n' "$DOWNLOAD_PAGE"
		printf '  "assets": {\n'
		first=1
		for t in $targets; do
			os="${t%/*}"
			arch="${t#*/}"
			pkg="${NAME}-${os}-${arch}"
			file="${pkg}.tar.gz"
			[ "$os" = windows ] && file="${pkg}.zip"
			[ "$first" = 1 ] || printf ',\n'
			first=0
			printf '    "%s-%s": "%s/%s"' "$os" "$arch" "$BASE_URL" "$file"
		done
		printf '\n  }\n}\n'
	} >"$ROOT/dist/latest.json"
fi

echo "==> packaged into dist/:"
ls -la "$ROOT/dist" | grep -E "${NAME}|SHA256SUMS|latest.json" || true
