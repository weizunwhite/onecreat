#!/usr/bin/env bash
# 从 hardware-common skill 刷新硬件 MCP 内嵌的传感器目录快照。
# 仓库里的 data/sensor-catalog.json 是 go:embed 进二进制的「真相源」,
# 此脚本让它能方便地从外部 skill 更新。改完需重新 make build。
#
# 用法: scripts/sync-hardware-catalog.sh [源 sensor-catalog.json 路径]
set -euo pipefail

SRC="${1:-$HOME/Library/Mobile Documents/com~apple~CloudDocs/M4bot/skills/hardware-common/sensor-catalog.json}"
DST="$(cd "$(dirname "$0")/.." && pwd)/cmd/reasonix-hardware-mcp/data/sensor-catalog.json"

if [ ! -f "$SRC" ]; then
  echo "源文件不存在: $SRC" >&2
  exit 1
fi
if ! python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$SRC" 2>/dev/null; then
  echo "源不是合法 JSON: $SRC" >&2
  exit 1
fi

cp "$SRC" "$DST"
echo "已同步: $SRC"
echo "     -> $DST"
echo "下一步: make build (让快照进二进制) + go test ./cmd/reasonix-hardware-mcp/"
