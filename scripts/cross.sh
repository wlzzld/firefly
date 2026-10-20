#!/usr/bin/env bash
# cross.sh — 用 fyne-cross（Docker 固定工具链）交叉打包 Windows / Linux。
# 产物在 fyne-cross/dist/<os>_<arch>/ 下。macOS 不走此脚本（必须原生）。
set -euo pipefail

if ! command -v fyne-cross >/dev/null 2>&1; then
	echo "未找到 fyne-cross。安装：go install github.com/fyne-io/fyne-cross@latest" >&2
	echo "（需要 Docker）" >&2
	exit 1
fi

TARGET="${1:-all}"

if [ "$TARGET" = "windows" ] || [ "$TARGET" = "all" ]; then
	echo "==> fyne-cross windows (amd64)"
	fyne-cross windows -arch amd64 -name Firefly
fi

if [ "$TARGET" = "linux" ] || [ "$TARGET" = "all" ]; then
	echo "==> fyne-cross linux (amd64)"
	# Linux 产物为 .tar.gz；AppImage/deb 需要额外工具链，CI release 阶段处理
	fyne-cross linux -arch amd64 -name Firefly
fi

echo "==> 完成，产物见 fyne-cross/dist/"
