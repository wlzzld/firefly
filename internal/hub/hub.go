package hub

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/lei/firefly/internal/discovery"
	"github.com/lei/firefly/internal/filetransfer"
	"github.com/lei/firefly/internal/identity"
	fireflyv1 "github.com/lei/firefly/internal/protocol/pb/firefly/v1"
	"github.com/lei/firefly/internal/store"
	"github.com/lei/firefly/internal/transport"
)

// presenceInterval 应用层心跳周期（保活连接、刷新对端显示名）。
const presenceInterval = 30 * time.Second

// Hub 是核心编排层，同时实现 filetransfer.SessionProvider。
type Hub struct {
	selfID identity.PeerID
	nameFn func() string
	disc   discovery.Discoverer
	lst    *transport.Listener
	pm     *peerManager
	db     *store.Store
	files  *filetransfer.Manager
	log    *slog.Logger

	bus  *bus
	stop chan struct{}

	// 群播送达计数：messageID → 剩余未回执的在线 peer 数
	receipts *receiptTracker
}

// New 创建 Hub。id 必须是完整身份（含 Static/Priv）：
// filetransfer 的数据面拨号要用它做 Noise 握手。
func New(id *identity.Identity, nameFn func() string, disc discovery.Discoverer, lst *transport.Listener, dialer *transport.Dialer, db *store.Store, dataDir string, dlDir func() string, maxTransfers int, log *slog.Logger) *Hub {
	selfID := id.PeerID
	h := &Hub{
		selfID:   selfID,
		nameFn:   nameFn,
		disc:     disc,
		lst:      lst,
		pm:       newPeerManager(selfID, dialer, log),
		db:       db,
		log:      log,
		bus:      newBus(),
		stop:     make(chan struct{}),
		receipts: newReceiptTracker(),
	}
	h.files = filetransfer.NewManager(id, h, db, dataDir, dlDir, maxTransfers, log)
	return h
}

// Subscribe 订阅事件流，返回退订函数。
func (h *Hub) Subscribe() (<-chan Event, func()) { return h.bus.subscribe() }

// Files 返回文件传输管理器。
func (h *Hub) Files() *filetransfer.Manager { return h.files }

// Store 返回持久化层。
func (h *Hub) Store() *store.Store { return h.db }

// SelfID 返回本机 PeerID。
func (h *Hub) SelfID() identity.PeerID { return h.selfID }

// Start 启动 hub 全部循环，阻塞直到 ctx 取消。
func (h *Hub) Start(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error { h.discoveryLoop(gctx); return nil })
	g.Go(func() error { h.incomingLoop(gctx); return nil })
	g.Go(func() error { h.establishedLoop(gctx); return nil })
	g.Go(func() error { h.transferLoop(gctx); return nil })
	g.Go(func() error { h.presenceLoop(gctx); return nil })

	return g.Wait()
}

// discoveryLoop 消费发现事件：持久化 peer、通知 PeerManager、发布 UI 事件。
func (h *Hub) discoveryLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-h.disc.Events():
			h.pm.onPeerEvent(ev)
			h.persistAndPublishPeer(ev)
		}
	}
}

func (h *Hub) persistAndPublishPeer(ev discovery.PeerEvent) {
	now := time.Now().UnixMilli()
	rec := &store.Peer{
		ID:          ev.Peer.ID.String(),
		Name:        ev.Peer.Name,
		Platform:    ev.Peer.Platform,
		Fingerprint: ev.Peer.Fingerprint,
		Trust:       store.TrustUnverified,
		FirstSeen:   now,
		LastSeen:    now,
	}
	// 已有记录则保留原 trust / first_seen
	if existing, err := h.db.GetPeer(rec.ID); err == nil && existing != nil {
		rec.Trust = existing.Trust
		rec.FirstSeen = existing.FirstSeen
	}
	if err := h.db.UpsertPeer(rec); err != nil {
		h.log.Error("持久化 peer 失败", "err", err)
	}

	info := &PeerInfo{
		ID: ev.Peer.ID, Name: rec.Name, Platform: rec.Platform,
		Fingerprint: rec.Fingerprint, Trust: rec.Trust, LastSeen: time.UnixMilli(now),
	}
	switch ev.Kind {
	case discovery.PeerUp:
		info.Online = true
		h.bus.publish(Event{Kind: EventPeerUp, Peer: info})
		// TOFU：未验证的 peer 上线时提示 UI 弹配对框
		if rec.Trust == store.TrustUnverified {
			h.bus.publish(Event{Kind: EventTrustChanged, Peer: info})
		}
	case discovery.PeerDown:
		info.Online = false
		h.bus.publish(Event{Kind: EventPeerDown, Peer: info})
	case discovery.PeerUpdated:
		info.Online = true
		h.bus.publish(Event{Kind: EventPeerUpdated, Peer: info})
	}
}

// incomingLoop 消费 listener 的入站会话，按首帧分类：
// StreamHeader → 文件数据连接（交给 filetransfer）；其他 → 消息连接（PeerManager）。
func (h *Hub) incomingLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-h.lst.Incoming():
			go h.classifyIncoming(ctx, s)
		}
	}
}

// firstFrameTimeout 等待入站连接首帧的超时（防端口扫描挂起）。
const firstFrameTimeout = 10 * time.Second

func (h *Hub) classifyIncoming(ctx context.Context, s transport.Session) {
	var first *fireflyv1.Envelope
	timer := time.NewTimer(firstFrameTimeout)
	defer timer.Stop()
	select {
	case env, ok := <-s.Recv():
		if !ok {
			s.Close() //nolint:errcheck
			return
		}
		first = env
	case <-timer.C:
		s.Close() //nolint:errcheck
		return
	case <-ctx.Done():
		s.Close() //nolint:errcheck
		return
	}

	// 信任检查对所有入站连接生效
	if !h.checkTrust(s) {
		s.Close() //nolint:errcheck
		return
	}

	if hdr := first.GetStreamHeader(); hdr != nil {
		// 文件数据连接：不入 PeerManager，直接交给传输管理器
		go h.files.HandleStreamHeader(s, hdr)
		return
	}

	// 消息连接
	if !h.pm.attachIncoming(s) {
		return // 冲突被拒，已关闭
	}
	go h.routeSession(ctx, s, first)
}

// establishedLoop 对拨号建立的会话做信任检查并启动路由协程。
func (h *Hub) establishedLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-h.pm.established:
			if !h.checkTrust(s) {
				s.Close() //nolint:errcheck
				continue
			}
			// 立即发送 presence 作为首帧：入站方的 classifyIncoming 要等到
			// 首帧才会注册会话，否则 10s 超时断开 → 重拨 → 抖动脉冲。
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := s.Send(c, h.presenceEnvelope()); err != nil {
				h.log.Debug("首帧 presence 发送失败", "peer", s.Peer().String()[:8], "err", err)
			}
			cancel()
			go h.routeSession(ctx, s, nil)
		}
	}
}

// presenceEnvelope 构造应用层心跳（拨号建立后的首帧兼保活消息）。
func (h *Hub) presenceEnvelope() *fireflyv1.Envelope {
	return &fireflyv1.Envelope{
		ProtoVersion: 1,
		MessageId:    uuid.Must(uuid.NewV7()).String(),
		SentAtUnixMs: time.Now().UnixMilli(),
		Payload: &fireflyv1.Envelope_Presence{Presence: &fireflyv1.Presence{
			Name: h.nameFn(),
		}},
	}
}

// routeSession 唯一消费会话 Recv()；通道关闭即会话结束。
// first 为分类阶段已消费的首帧（入站连接），出站会话为 nil。
func (h *Hub) routeSession(ctx context.Context, s transport.Session, first *fireflyv1.Envelope) {
	defer func() {
		h.pm.notifySessionDead(s)
		h.disc.NotifyDown(s.Peer())
	}()
	if first != nil {
		h.route(ctx, s, first)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-s.Recv():
			if !ok {
				if err := s.Err(); err != nil && !errors.Is(err, context.Canceled) {
					h.log.Debug("会话结束", "peer", s.Peer().String()[:8], "err", err)
				}
				return
			}
			h.route(ctx, s, env)
		}
	}
}

// transferLoop 把文件管理器的状态/进度转发到事件总线。
func (h *Hub) transferLoop(ctx context.Context) {
	states := h.files.States()
	progress := h.files.Progresses()
	for {
		select {
		case <-ctx.Done():
			return
		case st, ok := <-states:
			if !ok {
				states = nil
				continue
			}
			stCopy := st
			h.bus.publish(Event{Kind: EventTransferState, Transfer: &stCopy})
			// 传输完成时在对应会话里补一条文件消息（聊天记录可见）
			if st.Phase == filetransfer.PhaseDone {
				h.recordFileMessage(&st)
			}
		case p, ok := <-progress:
			if !ok {
				progress = nil
				continue
			}
			pCopy := p
			h.bus.publish(Event{Kind: EventTransferProgress, Progress: &pCopy})
		}
	}
}

// presenceLoop 周期性向所有活跃会话发送 presence 心跳（兼作连接保活）。
func (h *Hub) presenceLoop(ctx context.Context) {
	t := time.NewTicker(presenceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			env := h.presenceEnvelope()
			for _, s := range h.pm.onlineSessions() {
				c, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := s.Send(c, env); err != nil {
					h.log.Debug("presence 发送失败", "peer", s.Peer().String()[:8], "err", err)
				}
				cancel()
			}
		}
	}
}

// ---- filetransfer.SessionProvider 实现 ----

// Session 返回与 peer 的活跃消息会话。
func (h *Hub) Session(peer identity.PeerID) transport.Session { return h.pm.session(peer) }

// Addr 返回 peer 当前可达的 TCP 地址。
func (h *Hub) Addr(peer identity.PeerID) (netip.AddrPort, bool) { return h.pm.addr(peer) }

// Send 通过消息会话发送控制 Envelope。
func (h *Hub) Send(ctx context.Context, peer identity.PeerID, env *fireflyv1.Envelope) error {
	s := h.pm.session(peer)
	if s == nil {
		return errors.New("hub: 对端不在线")
	}
	return s.Send(ctx, env)
}
