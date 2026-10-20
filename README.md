# Firefly 🔥

去中心化的局域网聊天与文件传输客户端。没有服务器、没有账号、没有云——
同一局域网内的设备互相发现、直接点对点通信，全程端到端加密。

- **平台**：macOS / Windows / Ubuntu（单一 Go 二进制 + Fyne GUI）
- **聊天**：一对一私聊、局域网群组广播、消息历史与全文搜索
- **文件**：点对点直传，SHA-256 内容寻址校验，断点续传
- **安全**：Noise XX 端到端加密，TOFU 指纹配对，抓包不可读

## 快速开始

```bash
make build          # 编译到 bin/firefly
./bin/firefly       # 启动（首次运行自动创建身份与配置）
```

同一局域网内启动两台设备的 Firefly 即可互相发现（侧栏设备列表）。
首次见到新设备会弹出 **指纹配对对话框**：双方核对屏幕上 16 位指纹一致后
点「信任」，即完成 TOFU（Trust On First Use）配对。

### 本地开发（单台机器双实例冒烟）

```bash
make dev    # 实例 A：数据目录 /tmp/firefly-dev-a
make dev2   # 实例 B：数据目录 /tmp/firefly-dev-b（共享 UDP 广播端口 51771）
```

两个窗口互发消息、传文件即可全链路冒烟。`FIREFLY_DEBUG=1` 时日志同时
输出 stderr（debug 级），完整日志始终写在数据目录 `firefly.log`。

## 安全模型

| 层 | 机制 |
|---|---|
| 身份 | Ed25519 密钥对（`identity.key`，0600 权限，原子写）；PeerID = 公钥 |
| 传输加密 | Noise XX（25519/ChaChaPoly/SHA256），每条连接独立会话 |
| 身份绑定 | 握手 payload 携带 Ed25519 签名绑定 X25519 静态密钥（libp2p noise 模式），防中间人 |
| 密钥派生 | X25519 传输密钥由 Ed25519 种子经 HKDF 派生，签名/加密密钥分离 |
| 前向保密 | 每次重连重新 XX 握手；会话内每 10000 条消息自动 rekey |
| 信任 | TOFU：首见设备弹指纹框；公钥变化 → `key_changed` 拒收并红色警告 |
| 群播 | pairwise 扇出：群组消息逐一对每台在线设备独立加密发送，非共享密钥 |

**本地存储说明**：消息历史与文件记录保存在本地 SQLite
（`<配置目录>/firefly/firefly.db`，文件权限 0600），**明文**存储以便搜索与
历史浏览。端到端加密的目标是**网络不可读**（抓包、路由器、同网段嗅探均
只能看到密文）；本机磁盘安全由操作系统账户与文件权限保证。若设备可能被
他人物理接触，请使用全盘加密（FileVault / BitLocker / LUKS）。

## 打包发布

```bash
make package-darwin   # macOS .app（必须 macOS 原生；注入本地网络权限键 + ad-hoc 签名）
make cross-windows    # Windows（fyne-cross，需 Docker）
make cross-linux      # Linux（fyne-cross，需 Docker）
```

正式发布由 GitHub Actions 完成（推送 `v*` tag 触发 `.github/workflows/release.yml`）：
Windows/Linux 走 fyne-cross，macOS 在 macos-14 runner 原生打包并注入
Info.plist 权限键。

## 排障

### macOS：设备列表始终为空

macOS 14+ 的**本地网络隐私（TCC）**会静默拦截未声明用途应用的组播/广播。
`make package-darwin` 已向 `Info.plist` 注入：

- `NSLocalNetworkUsageDescription`（权限弹窗文案）
- `NSBonjourServices = ["_firefly._tcp"]`

若直接 `go run` / 未签名二进制运行，系统可能不弹窗直接拒绝（开发环境
已知限制）——此时发现层自动降级到 UDP 广播兜底通道；若仍不可用，请打包
后运行 `.app`，并在「系统设置 → 隐私与安全性 → 本地网络」确认已勾选。

### Windows：对方看不到我

首次启动请在防火墙弹窗中勾选**专用网络**允许。被误拒后手动放行：

```powershell
netsh advfirewall firewall add rule name="Firefly" dir=in action=allow program="C:\path\to\Firefly.exe" enable=yes
```

### 企业网 / 校园网发现不了设备

- 网管禁用组播：UDP 广播兜底通道通常仍可用（默认端口 51771，可在设置中改）。
- **AP 客户端隔离**（无线隔离）：路由器层面禁止设备互访，任何局域网方案都
  无解——需要管理员关闭隔离，或改用有线/同一 VLAN。

### 端口占用

TCP 51770 被占用时自动降级为系统随机端口并在发现层广告真实端口，无需处理。
UDP 51771 被占用意味着已有 Firefly 实例在运行（单实例锁也会阻止重复启动）。

## 项目结构

```
cmd/firefly/            入口：装配 + 日志
internal/
  app/                  生命周期编排（ctx 树、单实例锁、优雅关停）
  config/               JSON 配置（显示名/端口/下载目录/主题）
  identity/             Ed25519 身份、X25519 派生、指纹
  discovery/            mDNS + UDP 广播双通道发现、PeerIndex 在线判定
  transport/            Noise XX 握手（签名绑定）、分帧、会话、拨号/监听
  protocol/             protobuf 定义（proto/）与生成代码（pb/，提交入库）
  hub/                  路由、事件总线、PeerManager、TOFU 信任状态机、群播扇出
  filetransfer/         文件传输：控制面消息 + 数据面独立加密连接、续传、校验
  store/                SQLite（modernc 纯 Go）：peers/messages/files/settings + FTS5
  ui/                   Fyne 界面（唯一允许 import fyne 的包）
  testkit/              双节点集成测试基建（含 wire 字节密文断言）
scripts/                打包与图标脚本
build/                  图标、平台资源
```

## 开发

```bash
make test      # 全量测试（含双节点集成测试：发现→握手→私聊→群播→文件→密文断言）
make race      # -race 竞态检测
make lint      # go vet + golangci-lint
make proto     # buf 重新生成 protobuf 代码（首次需 make tools）
```

集成测试在同进程内起两个完整节点走真实 TCP/UDP 全链路，并录制 wire 字节
断言聊天文本、文件名、文件内容均不以明文出现在网络上。

端口默认：TCP 51770（消息/文件控制面 + 文件数据面监听），UDP 51771（发现广播）。
