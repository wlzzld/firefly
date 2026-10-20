package hub

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/lei/firefly/internal/discovery"
	"github.com/lei/firefly/internal/identity"
	"github.com/lei/firefly/internal/transport"
)

// peerConn 跟踪与单个 peer 的连接状态。
type peerConn struct {
	id     identity.PeerID
	dialer bool // 按方向规则本机是否应主动拨号

	mu      sync.Mutex
	addrs   []netip.AddrPort // 发现层提供的候选地址
	online  bool
	session transport.Session
	dialing bool // supervise 进行中（防止并发拨号竞争导致会话抖动）
}

func (pc *peerConn) get() (addrs []netip.AddrPort, online bool, session transport.Session) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.addrs, pc.online, pc.session
}

// peerManager 维护全部 peer 连接：方向规则、去重、断线重连。
// 注意：会话的 Recv() 由 hub 的路由协程唯一消费；
// 会话结束后 hub 调用 notifySessionDead 触发清理与重连。
type peerManager struct {
	selfID identity.PeerID
	dialer *transport.Dialer
	log    *slog.Logger

	mu    sync.Mutex
	peers map[identity.PeerID]*peerConn

	// 新建立的会话（hub 消费：信任检查 + 启动路由协程）
	established chan transport.Session
}

func newPeerManager(selfID identity.PeerID, dialer *transport.Dialer, log *slog.Logger) *peerManager {
	return &peerManager{
		selfID:      selfID,
		dialer:      dialer,
		log:         log,
		peers:       make(map[identity.PeerID]*peerConn),
		established: make(chan transport.Session, 32),
	}
}

// onPeerEvent 处理发现层事件。
func (pm *peerManager) onPeerEvent(ev discovery.PeerEvent) {
	switch ev.Kind {
	case discovery.PeerUp, discovery.PeerUpdated:
		pm.mu.Lock()
		pc, ok := pm.peers[ev.Peer.ID]
		if !ok {
			pc = &peerConn{id: ev.Peer.ID, dialer: transport.ShouldDial(pm.selfID, ev.Peer.ID)}
			pm.peers[ev.Peer.ID] = pc
		}
		pc.mu.Lock()
		pc.addrs = ev.Peer.Addrs
		pc.online = true
		pc.mu.Unlock()
		pm.mu.Unlock()

		if pc.dialer {
			pm.startSupervise(pc)
		}
		// 非拨号方等待对端连入（listener → attachIncoming）
	case discovery.PeerDown:
		pm.mu.Lock()
		pc, ok := pm.peers[ev.Peer.ID]
		pm.mu.Unlock()
		if !ok {
			return
		}
		pc.mu.Lock()
		pc.online = false
		s := pc.session
		pc.mu.Unlock()
		if s != nil {
			s.Close() //nolint:errcheck
		}
	}
}

// attachIncoming 处理监听到的入站会话：去重 + 方向规则。
// 返回 true 表示会话被接受（调用方随后必须消费其 Recv 或关闭）。
func (pm *peerManager) attachIncoming(s transport.Session) bool {
	id := s.Peer()
	pm.mu.Lock()
	pc, ok := pm.peers[id]
	if !ok {
		pc = &peerConn{id: id, dialer: transport.ShouldDial(pm.selfID, id)}
		pm.peers[id] = pc
	}
	pm.mu.Unlock()

	_, _, existing := pc.get()
	if existing != nil {
		if pc.dialer {
			// 本机应主动拨号且已有会话：入站为双拨冲突连接，关闭
			s.Close() //nolint:errcheck
			return false
		}
		// 本机应接受入站：关闭旧会话，保留新入站
		existing.Close() //nolint:errcheck
	}

	pc.mu.Lock()
	pc.session = s
	pc.mu.Unlock()
	return true
}

// startSupervise 去重启动拨号监督：同一 peerConn 同时至多一个 supervise
// 在运行（发现层多地址心跳会连续触发 PeerUpdated，若不设防会并发拨号，
// 入站方「关旧留新」导致会话反复抖动）。
func (pm *peerManager) startSupervise(pc *peerConn) {
	pc.mu.Lock()
	if pc.dialing || pc.session != nil {
		pc.mu.Unlock()
		return
	}
	pc.dialing = true
	pc.mu.Unlock()

	go func() {
		defer func() {
			pc.mu.Lock()
			pc.dialing = false
			pc.mu.Unlock()
		}()
		pm.supervise(pc)
	}()
}

// supervise 拨号方连接监督：指数退避 1s→30s + jitter，peer 离线或已有会话时停止。
func (pm *peerManager) supervise(pc *peerConn) {
	backoff := time.Second
	for {
		_, online, session := pc.get()
		if session != nil || !online {
			return
		}
		addrs := pc.addrsSnapshot()

		peer := discovery.Peer{ID: pc.id, Addrs: addrs}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		s, err := pm.dialer.Dial(ctx, peer)
		cancel()
		if err != nil {
			pm.log.Debug("拨号失败，退避重试", "peer", pc.id.String()[:8], "err", err)
			time.Sleep(backoff + time.Duration(rand.IntN(500))*time.Millisecond)
			backoff = min(backoff*2, 30*time.Second)
			// 退避期间 peer 可能已离线或已有会话
			if _, online2, sess2 := pc.get(); !online2 || sess2 != nil {
				return
			}
			continue
		}

		pc.mu.Lock()
		if pc.session != nil {
			pc.mu.Unlock()
			s.Close() //nolint:errcheck // 竞态：期间对端已连入
			return
		}
		pc.session = s
		pc.mu.Unlock()

		select {
		case pm.established <- s:
		default:
			s.Close() //nolint:errcheck
		}
		return
	}
}

func (pc *peerConn) addrsSnapshot() []netip.AddrPort {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	out := make([]netip.AddrPort, len(pc.addrs))
	copy(out, pc.addrs)
	return out
}

// notifySessionDead 由 hub 路由协程在会话 Recv 关闭后调用：清理并按需重连。
func (pm *peerManager) notifySessionDead(s transport.Session) {
	id := s.Peer()
	pm.mu.Lock()
	pc, ok := pm.peers[id]
	pm.mu.Unlock()
	if !ok {
		return
	}
	pc.mu.Lock()
	if pc.session == s {
		pc.session = nil
	}
	online, dialer := pc.online, pc.dialer
	pc.mu.Unlock()

	if online && dialer {
		go func() {
			time.Sleep(time.Second + time.Duration(rand.IntN(500))*time.Millisecond)
			pm.startSupervise(pc)
		}()
	}
}

// session 返回 peer 的活跃会话。
func (pm *peerManager) session(id identity.PeerID) transport.Session {
	pm.mu.Lock()
	pc, ok := pm.peers[id]
	pm.mu.Unlock()
	if !ok {
		return nil
	}
	_, _, s := pc.get()
	return s
}

// addr 返回 peer 的可达地址（优先活跃会话远端地址，其次发现层候选）。
func (pm *peerManager) addr(id identity.PeerID) (netip.AddrPort, bool) {
	pm.mu.Lock()
	pc, ok := pm.peers[id]
	pm.mu.Unlock()
	if !ok {
		return netip.AddrPort{}, false
	}
	addrs, _, s := pc.get()
	// 仅本机拨出的会话，其 RemoteAddr 才是对端监听地址；
	// 入站会话的远端地址是对端的临时源端口（不可回连），必须走发现层候选。
	if s != nil && pc.dialer {
		if ta, ok := s.RemoteAddr().(*net.TCPAddr); ok {
			if a, ok := netip.AddrFromSlice(ta.IP); ok {
				return netip.AddrPortFrom(a.Unmap(), uint16(ta.Port)), true
			}
		}
	}
	if len(addrs) > 0 {
		return addrs[0], true
	}
	return netip.AddrPort{}, false
}

// closeSession 关闭与指定 peer 的会话（拉黑时调用）。
func (pm *peerManager) closeSession(id identity.PeerID) {
	if s := pm.session(id); s != nil {
		s.Close() //nolint:errcheck
	}
}

// onlineSessions 返回全部活跃会话（群播扇出用）。
func (pm *peerManager) onlineSessions() []transport.Session {
	pm.mu.Lock()
	pcs := make([]*peerConn, 0, len(pm.peers))
	for _, pc := range pm.peers {
		pcs = append(pcs, pc)
	}
	pm.mu.Unlock()

	var out []transport.Session
	for _, pc := range pcs {
		if _, _, s := pc.get(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// isOnline 报告 peer 是否在发现层在线。
func (pm *peerManager) isOnline(id identity.PeerID) bool {
	pm.mu.Lock()
	pc, ok := pm.peers[id]
	pm.mu.Unlock()
	if !ok {
		return false
	}
	_, online, _ := pc.get()
	return online
}
