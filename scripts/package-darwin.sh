#!/usr/bin/env bash
# package-darwin.sh — 打包 macOS .app 并注入本地网络权限键。
#
# 背景：fyne 工具的 Info.plist 是内置模板，不支持自定义键；
# macOS 14+ 的本地网络隐私（TCC）要求 mDNS 应用声明
# NSLocalNetworkUsageDescription + NSBonjourServices，
# 否则系统静默拦截组播/广播（发现层退化为不可用）。
# 故在 fyne package 之后用 PlistBuddy 注入并重签名。
set -euo pipefail

VERSION="${1:-0.1.0}"
FYNE="${FYNE:-fyne}"

# go install 的产物在 GOPATH/bin，make 派生 shell 常常不在 PATH 里，主动补上
export PATH="$(go env GOPATH)/bin:$PATH"

if ! command -v "$FYNE" >/dev/null 2>&1; then
	echo "未找到 fyne CLI。安装：go install fyne.io/tools/cmd/fyne@latest" >&2
	exit 1
fi
if [ "$(uname)" != "Darwin" ]; then
	echo "macOS 打包必须在 macOS 上原生执行（CI 使用 macos-14 runner）" >&2
	exit 1
fi

# 与 fyne-cross 相同策略：先自行编译（注入版本号），再让 fyne package 只负责
# 组装 .app。这样 srcDir 保持为仓库根目录，FyneApp.toml（图标/AppID 元数据）
# 才能被正确读取——若用 --src cmd/firefly，工具会去该目录找 FyneApp.toml 而落空。
# fyne package 结束时会按自己的模式回写 FyneApp.toml：丢掉 Summary、[Linux]
# 等它不认识的键，并自增 Build 计数。打包前备份，脚本退出时恢复。
cp FyneApp.toml FyneApp.toml.bak
trap 'mv -f FyneApp.toml.bak FyneApp.toml' EXIT

echo "==> 预编译二进制 version=${VERSION}"
BIN_TMP="bin/firefly-darwin-$(uname -m)"
mkdir -p bin
go build -trimpath -ldflags "-X main.version=${VERSION}" -o "$BIN_TMP" ./cmd/firefly

echo "==> fyne package -os darwin"
"$FYNE" package -os darwin -name Firefly -app-version "$VERSION" --executable "$BIN_TMP"

PLIST="Firefly.app/Contents/Info.plist"
if [ ! -f "$PLIST" ]; then
	echo "未找到 $PLIST（fyne 版本输出结构变化？）" >&2
	exit 1
fi

echo "==> 注入本地网络权限键"
add_if_missing() {
	if ! /usr/libexec/PlistBuddy -c "Print :$1" "$PLIST" >/dev/null 2>&1; then
		/usr/libexec/PlistBuddy -c "Add :$1 $2 ${3:-}" "$PLIST"
	fi
}
add_if_missing NSLocalNetworkUsageDescription string \
	"Firefly 需要在本地网络中发现同网段的其他 Firefly 设备（mDNS/UDP 广播），用于端到端加密聊天与文件传输。"
add_if_missing NSBonjourServices array
if ! /usr/libexec/PlistBuddy -c "Print :NSBonjourServices:0" "$PLIST" >/dev/null 2>&1; then
	/usr/libexec/PlistBuddy -c "Add :NSBonjourServices:0 string _firefly._tcp" "$PLIST"
fi

echo "==> 重签名（ad-hoc；注入 plist 会使 fyne 的原签名失效）"
codesign --force --deep --sign - Firefly.app

echo "==> 完成：Firefly.app（版本 ${VERSION}）"
echo "    提示：正式发布请使用开发者证书签名并公证（notarytool），"
echo "    未签名/未公证的分发会让 macOS 对本地网络权限提示更严格。"
