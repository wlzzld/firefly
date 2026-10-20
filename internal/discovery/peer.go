// Package discovery 实现局域网设备自动发现：
// mDNS(_firefly._tcp) 主通道 + UDP 广播兜底，双源按 PeerID 合并去重。
// 发现层元数据只是"线索"，设备真实性由 transport 层 Noise 握手签名证明。
package discovery

import (
	"context"
	"net/netip"
	"time"

	"github.com/lei/firefly/internal/identity"
)

// Source 标记 peer 信息由哪个通道发现。
type Source int

const (
	SourceBroadcast Source = 1 << iota
	SourceMDNS
)

func (s Source) String() string {
	switch s {
	case SourceBroadcast:
		return "broadcast"
	case SourceMDNS:
		return "mdns"
	case SourceBroadcast | SourceMDNS:
		return "broadcast+mdns"
	default:
		return "unknown"
	}
}

// Peer 是一个被发现（或本机）的局域网设备。
type Peer struct {
	ID          identity.PeerID
	Name        string
	Platform    string
	Fingerprint string
	Addrs       []netip.AddrPort // 候选 TCP 地址（多网卡可能有多个）
	Source      Source
	LastSeen    time.Time
}

// EventKind 是 peer 状态变化类型。
type EventKind int

const (
	PeerUp      EventKind = iota // 新设备上线
	PeerDown                     // 设备离线（TTL 超时 / goodbye / 连接断开）
	PeerUpdated                  // 信息刷新（名称、地址等）
)

// PeerEvent 推送给订阅者（hub）。
type PeerEvent struct {
	Kind EventKind
	Peer Peer
}

// Discoverer 是发现层统一接口。
type Discoverer interface {
	// Start 启动发现（注册自身 + 浏览/监听他人），阻塞直到 ctx 取消或出错。
	Start(ctx context.Context) error
	// Events 返回 peer 状态事件流。
	Events() <-chan PeerEvent
	// Self 返回本机对外广告的 peer 信息。
	Self() Peer
	// NotifyDown 由 transport 调用：连接断开时立即标记 peer 离线（不等 TTL）。
	NotifyDown(id identity.PeerID)
	// Probe 立即发送一次探测，请求所有在线节点尽快 announce。
	Probe()
}
