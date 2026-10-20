#!/usr/bin/env bash
# cross.sh — 用 fyne-cross（Docker 固定工具链）交叉打包 Windows / Linux。
# 产物在 fyne-cross/dist/<os>_<arch>/ 下。macOS 不走此脚本（必须原生）。
set -euo pipefail

# go install 的产物在 GOPATH/bin，交互式 shell 之外常常不在 PATH 里，主动补上
export PATH="$(go env GOPATH)/bin:$PATH"

if ! command -v fyne-cross >/dev/null 2>&1; then
	echo "未找到 fyne-cross。安装：go install github.com/fyne-io/fyne-cross@latest" >&2
	echo "（需要 Docker）" >&2
	exit 1
fi

if ! docker info >/dev/null 2>&1; then
	echo "Docker 守护进程不可用。请先启动 Docker Desktop（或 dockerd）再重试。" >&2
	exit 1
fi

TARGET="${1:-all}"

# main 包在 ./cmd/firefly，必须显式传给 fyne-cross（默认构建模块根目录）。
# -cache 持久化模块与编译缓存：容器内下载中断（unexpected EOF）后重试可续用。
CACHE_DIR="${HOME}/.cache/fyne-cross"

# fyne package（容器内执行）会按自己的模式回写 FyneApp.toml：丢掉 Summary、
# [Linux] 等未识别键并自增 Build 计数。打包前备份，脚本退出时恢复。
cp FyneApp.toml FyneApp.toml.bak
trap 'mv -f FyneApp.toml.bak FyneApp.toml' EXIT

if [ "$TARGET" = "windows" ] || [ "$TARGET" = "all" ]; then
	echo "==> fyne-cross windows (amd64)"
	fyne-cross windows -arch amd64 -cache "$CACHE_DIR" -name Firefly ./cmd/firefly
fi

if [ "$TARGET" = "linux" ] || [ "$TARGET" = "all" ]; then
	echo "==> fyne-cross linux (amd64)"
	# Linux 产物为 .tar.gz；AppImage/deb 需要额外工具链，CI release 阶段处理
	fyne-cross linux -arch amd64 -cache "$CACHE_DIR" -name Firefly ./cmd/firefly
fi

echo "==> 完成，产物见 fyne-cross/dist/"
