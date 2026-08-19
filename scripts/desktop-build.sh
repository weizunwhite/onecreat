#!/usr/bin/env bash
# Build and package the Wails desktop app for one platform. Wails cannot
# cross-compile a CGO+webview binary, so this runs on a native runner per target
# (see .github/workflows/release-desktop.yml) and is invoked once per matrix entry.
#
# Output lands in <repo>/dist/ with stable, platform-keyed names that
# desktop/cmd/sign's `manifest` subcommand maps back to update.PlatformKey:
#   macOS:   onecreat-darwin-<arch>.zip                  (ditto archive of the .app,自动更新用)
#            onecreat-darwin-<arch>-<version>.dmg        (拖拽安装的分发盘,给用户下载)
#   Windows: onecreat-windows-<arch>-installer.exe       (NSIS per-user installer)
#   Linux:   onecreat-linux-<arch>.tar.gz                (bare binary)
#
# Usage: scripts/desktop-build.sh <os/arch> <version>
#   e.g. scripts/desktop-build.sh darwin/arm64 v1.1.0
set -euo pipefail

PLATFORM="${1:?usage: desktop-build.sh <os/arch> <version>}"
VERSION="${2:?usage: desktop-build.sh <os/arch> <version>}"

# makensis 在 LANG/LC_ALL 为空的环境下宽字符转换会崩(bad_alloc,macOS 实测
# 3.11/3.12 均复现);无 locale 的脚本化/CI 环境兜底为 UTF-8,已设置的不覆盖。
if [ -z "${LC_ALL:-}" ] && [ -z "${LANG:-}" ]; then
	export LC_ALL="en_US.UTF-8"
fi

os="${PLATFORM%/*}"
arch="${PLATFORM#*/}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APPNAME="onecreat"            # wails.json productName -> onecreat.app
BINNAME="onecreat-desktop"    # wails.json outputfilename -> linux binary name

cd "$ROOT/desktop"

if command -v wails >/dev/null 2>&1; then
	WAILS="$(command -v wails)"
else
	gopath="$(go env GOPATH 2>/dev/null || true)"
	if [ -n "$gopath" ] && [ -x "$gopath/bin/wails" ]; then
		WAILS="$gopath/bin/wails"
	else
		echo "wails CLI not found. Install it with: go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0" >&2
		exit 127
	fi
fi

hardware_ext=""
[ "$os" = windows ] && hardware_ext=".exe"

# The Windows NSIS installer is produced during `wails build -nsis`, so its extra
# payload must exist before Wails invokes makensis. macOS/Linux package the MCP
# after Wails finishes.
if [ "$os" = windows ]; then
	hardware_installer_payload="build/windows/installer/onecreat-hardware-mcp.exe"
	echo "==> building bundled hardware MCP for NSIS -> $hardware_installer_payload"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -ldflags "-s -w -X main.version=$VERSION" \
		-o "$hardware_installer_payload" ../cmd/reasonix-hardware-mcp
fi

# NSIS installer is Windows-only (Wails requires a single windows target for -nsis).
# defaultAccountMode=platform 让正式打包版默认进平台模式(强制登录 + 走平台网关);dev/裸 go build
# 不注入 → 本地免登录。见 desktop/accounts_app.go platformAccountEnabled。
build_args=(-clean -platform "$PLATFORM" -ldflags "-X main.version=$VERSION -X main.defaultAccountMode=platform")
[ "$os" = windows ] && build_args+=(-nsis)

echo "==> $WAILS build ${build_args[*]}"
"$WAILS" build "${build_args[@]}"

hardware_bin="build/bin/onecreat-hardware-mcp${hardware_ext}"
if [ "$os" != windows ]; then
	echo "==> building bundled hardware MCP -> $hardware_bin"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -ldflags "-s -w -X main.version=$VERSION" \
		-o "$hardware_bin" ../cmd/reasonix-hardware-mcp
fi

mkdir -p "$ROOT/dist"

case "$os" in
darwin)
	# Wails names the bundle after outputfilename (reasonix-desktop.app); repackage
	# it as onecreat.app for a clean user-facing name. Ad-hoc sign the copy (still
	# not notarized — the real fix is a Developer ID cert); this cuts down the
	# Gatekeeper "is damaged / can't be opened" error on a downloaded build, though
	# users may still need to clear the quarantine attribute (see desktop/README.md).
	staging=$(mktemp -d)
	app="$staging/${APPNAME}.app"
	cp -R "build/bin/onecreat-desktop.app" "$app"
	cp "$hardware_bin" "$app/Contents/MacOS/onecreat-hardware-mcp"
	find "$app" -name '._*' -delete
	codesign --force --deep -s - "$app"
	COPYFILE_DISABLE=1 ditto -c -k --norsrc --noextattr --keepParent "$app" "$ROOT/dist/${APPNAME}-darwin-${arch}.zip"
	# 同时出 DMG(拖拽安装盘):zip 供自动更新链路,DMG 给用户手动下载安装。
	dmg_stage=$(mktemp -d)
	cp -R "$app" "$dmg_stage/${APPNAME}.app"
	ln -s /Applications "$dmg_stage/Applications"
	hdiutil create -volname "OneCreat ${VERSION}" -srcfolder "$dmg_stage" -ov -format UDZO \
		"$ROOT/dist/${APPNAME}-darwin-${arch}-${VERSION}.dmg"
	rm -rf "$dmg_stage" "$staging"
	;;
windows)
	# `wails build -nsis` writes the installer under build/bin; its exact name
	# varies, so glob for it and copy to a stable, platform-keyed name.
	installer=$(ls build/bin/*installer*.exe 2>/dev/null | head -n1 || true)
	[ -n "$installer" ] || { echo "no NSIS installer found in build/bin" >&2; exit 1; }
	cp "$installer" "$ROOT/dist/${APPNAME}-windows-${arch}-installer.exe"
	;;
linux)
	tar -czf "$ROOT/dist/${APPNAME}-linux-${arch}.tar.gz" -C build/bin "$BINNAME" "onecreat-hardware-mcp"
	;;
*)
	echo "unsupported os: $os" >&2
	exit 1
	;;
esac

echo "==> packaged into dist/:"
ls -la "$ROOT/dist"
