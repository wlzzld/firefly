# Firefly — 去中心化局域网聊天与文件传输客户端
APP      := firefly
VERSION  ?= 0.1.0
LDFLAGS  := -X main.version=$(VERSION)

# 双实例本地冒烟的数据目录与共享 UDP 广播端口
DEV_DIR  ?= /tmp/firefly-dev-a
DEV2_DIR ?= /tmp/firefly-dev-b
UDP_PORT ?= 51771

.PHONY: all build dev dev2 test race lint proto tools icon \
        package-darwin cross-windows cross-linux clean

all: build

## build: 编译当前平台二进制到 bin/
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP) ./cmd/firefly

## dev: 以调试模式运行实例 A（数据目录 /tmp/firefly-dev-a）
dev:
	FIREFLY_DEBUG=1 FIREFLY_DATA_DIR=$(DEV_DIR) go run -ldflags "$(LDFLAGS)" ./cmd/firefly

## dev2: 以调试模式运行实例 B（与 A 共享 UDP 广播端口以便互发现）
dev2:
	@mkdir -p $(DEV2_DIR)
	@printf '{"display_name":"firefly-b","tcp_port":0,"udp_port":%s,"download_dir":"%s/downloads","theme":"auto","max_transfers":3}\n' \
		"$(UDP_PORT)" "$(DEV2_DIR)" > $(DEV2_DIR)/config.json
	FIREFLY_DEBUG=1 FIREFLY_DATA_DIR=$(DEV2_DIR) go run -ldflags "$(LDFLAGS)" ./cmd/firefly

## test: 全量单元测试 + 双节点集成测试
test:
	go test ./... -count=1

## race: 带竞态检测的全量测试
race:
	go test ./... -count=1 -race

## lint: go vet + golangci-lint（如已安装）
lint:
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || echo "（golangci-lint 未安装，已跳过）"

## proto: 用 buf 重新生成 protobuf Go 代码
proto: tools
	./tools/buf generate

## tools: 安装 buf 与 protoc-gen-go 到 tools/（已 gitignore）
tools:
	GOBIN=$(CURDIR)/tools go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	GOBIN=$(CURDIR)/tools go install github.com/bufbuild/buf/cmd/buf@latest

## icon: 重新生成应用图标 build/icon.png
icon:
	go run ./scripts/genicon -out build/icon.png

## package-darwin: 打包 macOS .app（注入本地网络权限键 + ad-hoc 签名）
package-darwin:
	./scripts/package-darwin.sh $(VERSION)

## cross-windows / cross-linux: fyne-cross 交叉打包（需 Docker）
cross-windows:
	./scripts/cross.sh windows

cross-linux:
	./scripts/cross.sh linux

clean:
	rm -rf bin/ dist/ fyne-cross/ Firefly.app
