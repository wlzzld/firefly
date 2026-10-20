package discovery

import (
	"net/netip"
	"sync"
	"time"

	"github.com/lei/firefly/internal/identity"
)

const (
	// heartbeatInterval 与广播周期一致（3s）。
	heartbeatInterval = 3 * time.Second
	// peerTTL = 3 个心跳周期未刷新即判定离线。
	peerTTL = 9 * time.Second
	// maxAddrsPerPeer 限制每个 peer 的候选 TCP 地址数量。
	maxAddrsPerPeer = 4
)

// peerIndex 合并双通道观测，维护在线表并产出 PeerEvent。
type peerIndex struct {
	selfID identity.PeerID

	mu    sync.Mutex
	peers map[identity.PeerID]*indexedPeer

	events chan PeerEvent
}

type indexedPeer struct {
	peer     Peer
	online   bool
	lastSeen time.Time
}

func newPeerIndex(selfID identity.PeerID) *peerIndex {
	return &peerIndex{
		selfID: selfID,
		peers:  make(map[identity.PeerID]*indexedPeer),
		events: make(chan PeerEvent, 64),
	}
}

// observe 处理一条原始观测，必要时产出事件。
func (idx *peerIndex) observe(s rawPeerSighting) {
	id, err := identity.PeerIDFromBytes(s.id[:])
	if err != nil || id == idx.selfID {
		return
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	ep, exists := idx.peers[id]
	if s.goodbye {
		if exists && ep.online {
			ep.online = false
			idx.emitLocked(PeerDown, ep.peer)
		}
		return
	}

	now := time.Now()
	if !exists {
		ep = &indexedPeer{}
		idx.peers[id] = ep
	}
	p := &ep.peer
	p.ID = id
	p.LastSeen = now
	p.Source |= s.source
	ep.lastSeen = now

	changed := false
	if s.name != "" && s.name != p.Name {
		p.Name = s.name
		changed = true
	}
	if s.platform != "" && s.platform != p.Platform {
		p.Platform = s.platform
		changed = true
	}
	if s.fingerprint != "" && s.fingerprint != p.Fingerprint {
		p.Fingerprint = s.fingerprint
		changed = true
	}
	if s.addr.IsValid() && s.tcpPort > 0 && !containsAddr(p.Addrs, s.addr) {
		p.Addrs = append(p.Addrs, s.addr)
		if len(p.Addrs) > maxAddrsPerPeer {
			p.Addrs = p.Addrs[len(p.Addrs)-maxAddrsPerPeer:]
		}
		changed = true
	}

	if !ep.online {
		ep.online = true
		idx.emitLocked(PeerUp, *p)
	} else if changed {
		idx.emitLocked(PeerUpdated, *p)
	}
}

// sweep 由定时器驱动，将 TTL 超时的 peer 标记为离线。
func (idx *peerIndex) sweep() {
	now := time.Now()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for id, ep := range idx.peers {
		if ep.online && now.Sub(ep.lastSeen) > peerTTL {
			ep.online = false
			idx.emitLocked(PeerDown, ep.peer)
			// 离线 peer 保留在表中（地址可能过期），但清空候选地址防止误连
			ep.peer.Addrs = nil
			delete(idx.peers, id) // 重新上线时会作为 PeerUp 再次出现
		}
	}
}

// notifyDown 由 transport 调用：连接断开时立即下线（不等 TTL）。
func (idx *peerIndex) notifyDown(id identity.PeerID) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if ep, ok := idx.peers[id]; ok && ep.online {
		ep.online = false
		idx.emitLocked(PeerDown, ep.peer)
		delete(idx.peers, id)
	}
}

// snapshot 返回当前在线 peer 列表（副本）。
func (idx *peerIndex) snapshot() []Peer {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	out := make([]Peer, 0, len(idx.peers))
	for _, ep := range idx.peers {
		if ep.online {
			out = append(out, ep.peer)
		}
	}
	return out
}

func (idx *peerIndex) emitLocked(kind EventKind, p Peer) {
	ev := PeerEvent{Kind: kind, Peer: p}
	select {
	case idx.events <- ev:
	default:
		// 事件通道满时丢弃（订阅者应尽快消费；Down/Up 后续 sweep 会补偿）
	}
}

func containsAddr(addrs []netip.AddrPort, a netip.AddrPort) bool {
	for _, x := range addrs {
		if x == a {
			return true
		}
	}
	return false
}
