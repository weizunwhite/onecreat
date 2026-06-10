#!/usr/bin/env bash
# Build and inspect the Windows desktop installer payload.
#
# This validates the packaging layer on non-Windows runners. It does not replace
# a native Windows launch test, but it proves the installer is built and contains
# the bundled hardware MCP executable.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ARCH="${REASONIX_WINDOWS_VERIFY_ARCH:-amd64}"
VERSION="${REASONIX_WINDOWS_VERIFY_VERSION:-windows-package-verify}"
RUN_DIR="${REASONIX_WINDOWS_VERIFY_DIR:-$ROOT/dist/windows-package-verify-$(date +%Y%m%d-%H%M%S)}"
INSTALLER="$ROOT/dist/onecreat-windows-${ARCH}-installer.exe"
EXTRACT_DIR="$RUN_DIR/extract"
SUMMARY="$RUN_DIR/summary.json"

need() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "missing required command: $1" >&2
		exit 1
	fi
}

need go
need file
need shasum
need makensis
need 7zz

mkdir -p "$RUN_DIR"

echo "==> building Windows installer -> $INSTALLER"
PATH="$PATH:/Users/zunwei/go/bin" "$ROOT/scripts/desktop-build.sh" "windows/$ARCH" "$VERSION"

test -s "$INSTALLER" || { echo "installer was not created: $INSTALLER" >&2; exit 1; }

echo "==> extracting installer -> $EXTRACT_DIR"
rm -rf "$EXTRACT_DIR"
mkdir -p "$EXTRACT_DIR"
7zz x -y "-o$EXTRACT_DIR" "$INSTALLER" >"$RUN_DIR/7zz-extract.log"
7zz l "$INSTALLER" >"$RUN_DIR/7zz-list.log"

DESKTOP_EXE="$EXTRACT_DIR/reasonix-desktop.exe"
HARDWARE_EXE="$EXTRACT_DIR/reasonix-hardware-mcp.exe"
SOURCE_HARDWARE_EXE="$ROOT/desktop/build/windows/installer/reasonix-hardware-mcp.exe"

for path in "$DESKTOP_EXE" "$HARDWARE_EXE" "$SOURCE_HARDWARE_EXE"; do
	test -s "$path" || { echo "missing expected Windows payload: $path" >&2; exit 1; }
done

file "$INSTALLER" "$DESKTOP_EXE" "$HARDWARE_EXE" "$SOURCE_HARDWARE_EXE" >"$RUN_DIR/file.txt"
go version -m "$HARDWARE_EXE" >"$RUN_DIR/hardware-go-version.txt"
go version -m "$DESKTOP_EXE" >"$RUN_DIR/desktop-go-version.txt"

grep -q "Nullsoft Installer" "$RUN_DIR/file.txt" || { echo "installer is not recognized as NSIS" >&2; exit 1; }
grep -q "reasonix/cmd/reasonix-hardware-mcp" "$RUN_DIR/hardware-go-version.txt" || {
	echo "hardware MCP payload has unexpected Go module metadata" >&2
	exit 1
}
grep -q "GOOS=windows" "$RUN_DIR/hardware-go-version.txt" || {
	echo "hardware MCP payload is not a Windows binary" >&2
	exit 1
}
grep -q "GOARCH=$ARCH" "$RUN_DIR/hardware-go-version.txt" || {
	echo "hardware MCP payload arch mismatch" >&2
	exit 1
}
grep -q 'File "reasonix-hardware-mcp.exe"' "$ROOT/desktop/build/windows/installer/project.nsi" || {
	echo "NSIS script does not install reasonix-hardware-mcp.exe" >&2
	exit 1
}
grep -q 'InstallDir "$LOCALAPPDATA\\Programs\\${INFO_PRODUCTNAME}"' "$ROOT/desktop/build/windows/installer/project.nsi" || {
	echo "NSIS script is not configured for per-user install dir" >&2
	exit 1
}
grep -q 'WriteRegStr HKCU' "$ROOT/desktop/build/windows/installer/project.nsi" || {
	echo "NSIS script does not use HKCU uninstaller registry" >&2
	exit 1
}

src_sha=$(shasum -a 256 "$SOURCE_HARDWARE_EXE" | awk '{print $1}')
extracted_sha=$(shasum -a 256 "$HARDWARE_EXE" | awk '{print $1}')
if [ "$src_sha" != "$extracted_sha" ]; then
	echo "extracted hardware MCP checksum does not match installer payload" >&2
	exit 1
fi

python3 - "$ROOT" "$RUN_DIR" "$INSTALLER" "$DESKTOP_EXE" "$HARDWARE_EXE" "$src_sha" "$ARCH" "$VERSION" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
run_dir = pathlib.Path(sys.argv[2])
installer = pathlib.Path(sys.argv[3])
desktop_exe = pathlib.Path(sys.argv[4])
hardware_exe = pathlib.Path(sys.argv[5])
hardware_sha = sys.argv[6]
arch = sys.argv[7]
version = sys.argv[8]


def rel(path):
    path = pathlib.Path(path)
    try:
        return str(path.relative_to(root))
    except ValueError:
        return str(path)


summary = {
    "runDir": str(run_dir),
    "arch": arch,
    "version": version,
    "installer": rel(installer),
    "installerBytes": installer.stat().st_size,
    "extracted": {
        "desktop": rel(desktop_exe),
        "hardwareMCP": rel(hardware_exe),
        "hardwareMCPBytes": hardware_exe.stat().st_size,
        "hardwareMCPSHA256": hardware_sha,
    },
    "checks": [
        "desktop-build windows installer succeeded",
        "installer recognized as NSIS self-extracting archive",
        "installer extracts with 7zz",
        "reasonix-desktop.exe is present",
        "reasonix-hardware-mcp.exe is present",
        "extracted hardware MCP checksum matches NSIS payload",
        "hardware MCP Go metadata is reasonix/cmd/reasonix-hardware-mcp",
        "hardware MCP Go metadata is GOOS=windows and matching GOARCH",
        "NSIS installs reasonix-hardware-mcp.exe next to the desktop executable",
        "NSIS uses LOCALAPPDATA per-user install directory and HKCU uninstall registry",
    ],
    "notVerifiedHere": [
        "native Windows process launch; run scripts/windows-native-smoke.ps1 on a Windows runner or VM",
        "silent install/uninstall on a Windows VM or runner",
        "Windows WebView2 runtime behavior",
    ],
}
(run_dir / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8")
print(f"summary: {run_dir / 'summary.json'}")
PY

echo "==> Windows package verification passed"
