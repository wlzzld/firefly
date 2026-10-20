package hub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/lei/firefly/internal/filetransfer"
	"github.com/lei/firefly/internal/identity"
	fireflyv1 "github.com/lei/firefly/internal/protocol/pb/firefly/v1"
	"github.com/lei/firefly/internal/store"
	"github.com/lei/firefly/internal/transport"
)

// maxTextLen 单条文本消息上限（8KiB）。
const maxTextLen = 8 * 1024

// route 分发单个入站 Envelope。
func (h *Hub) route(ctx context.Context, s transport.Session, env *fireflyv1.Envelope) {
	sender := s.Peer()

	// 被拉黑/密钥变更的 peer 的一切消息直接丢弃
	switch h.TrustOf(sender) {
	case store.TrustBlocked, store.TrustKeyChanged:
		return
	}

	switch p := env.Payload.(type) {
	case *fireflyv1.Envelope_Chat:
		h.handleChat(ctx, s, sender, env, p.Chat)
	case *fireflyv1.Envelope_Receipt:
		h.handleReceipt(sender, p.Receipt)
	case *fireflyv1.Envelope_FileOffer:
		h.handleFileOffer(sender, p.FileOffer)
	case *fireflyv1.Envelope_FileResponse:
		h.files.HandleResponse(sender, p.FileResponse)
	case *fireflyv1.Envelope_FileCancel:
		h.files.HandleCancel(sender, p.FileCancel)
	case *fireflyv1.Envelope_Presence:
		h.handlePresence(sender, p.Presence)
	case *fireflyv1.Envelope_Typing:
		// v1 暂不展示输入状态
	case *fireflyv1.Envelope_StreamHeader:
		// 消息连接上不应出现 StreamHeader，忽略
	default:
		h.log.Debug("未知 envelope payload", "peer", sender.String()[:8])
	}
}

// handleChat 处理入站聊天：持久化（message_id 幂等去重）→ 回执 → UI 事件。
func (h *Hub) handleChat(ctx context.Context, s transport.Session, sender identity.PeerID, env *fireflyv1.Envelope, chat *fireflyv1.Chat) {
	if len(chat.Text) > maxTextLen || env.MessageId == "" {
		return
	}
	// 会话归属以发送者身份为准（不信任对端声称的 conversation_id，DM 一律归到发送者）
	conv := chat.ConversationId
	if conv != GroupLAN {
		conv = DMConversation(sender)
	}

	now := time.Now().UnixMilli()
	sentAt := env.SentAtUnixMs
	if sentAt <= 0 || sentAt > now+5*60*1000 {
		sentAt = now // 时钟异常兜底
	}
	msg := &store.Message{
		ID:           env.MessageId,
		Conversation: conv,
		PeerID:       sender.String(),
		Direction:    store.DirectionInbound,
		Kind:         store.KindText,
		Text:         chat.Text,
		SentAt:       sentAt,
		ReceivedAt:   &now,
		Status:       store.StatusDelivered,
	}
	if chat.ReplyTo != "" {
		reply := chat.ReplyTo
		msg.ReplyTo = &reply
	}

	inserted, err := h.db.InsertMessage(msg)
	if err != nil {
		h.log.Error("消息持久化失败", "err", err)
		return
	}

	// 幂等：重复投递仍回执，但不重复推 UI
	if inserted {
		h.bus.publish(Event{Kind: EventMessageReceived, Conversation: conv, Message: msg})
	}

	// DELIVERED 回执
	ack := &fireflyv1.Envelope{
		ProtoVersion: 1,
		MessageId:    uuid.Must(uuid.NewV7()).String(),
		SentAtUnixMs: time.Now().UnixMilli(),
		Payload: &fireflyv1.Envelope_Receipt{Receipt: &fireflyv1.Receipt{
			MessageId: env.MessageId,
			Kind:      fireflyv1.ReceiptKind_RECEIPT_KIND_DELIVERED,
		}},
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.Send(c, ack); err != nil {
		h.log.Debug("回执发送失败", "peer", sender.String()[:8], "err", err)
	}
}

// handleReceipt 处理入站回执。
func (h *Hub) handleReceipt(sender identity.PeerID, r *fireflyv1.Receipt) {
	if r.MessageId == "" {
		return
	}
	switch r.Kind {
	case fireflyv1.ReceiptKind_RECEIPT_KIND_DELIVERED:
		tracked, allDone := h.receipts.ack(r.MessageId, sender.String())
		if tracked && !allDone {
			return // 群播尚未全部送达
		}
		if err := h.db.UpdateMessageStatus(r.MessageId, store.StatusDelivered); err != nil {
			h.log.Debug("更新回执状态失败", "err", err)
			return
		}
		h.bus.publish(Event{Kind: EventReceiptUpdated, ReceiptID: r.MessageId, Receipt: store.StatusDelivered})
	case fireflyv1.ReceiptKind_RECEIPT_KIND_READ:
		if err := h.db.UpdateMessageStatus(r.MessageId, store.StatusRead); err != nil {
			return
		}
		h.bus.publish(Event{Kind: EventReceiptUpdated, ReceiptID: r.MessageId, Receipt: store.StatusRead})
	}
}

func (h *Hub) handleFileOffer(sender identity.PeerID, offer *fireflyv1.FileOffer) {
	if offer.Size <= 0 || len(offer.Sha256) != 32 || offer.FileId == "" {
		return
	}
	name := ""
	if rec, err := h.db.GetPeer(sender.String()); err == nil && rec != nil {
		name = rec.Name
	}
	h.files.HandleOffer(sender, name, offer)
}

func (h *Hub) handlePresence(sender identity.PeerID, p *fireflyv1.Presence) {
	if p.Name == "" {
		return
	}
	rec, err := h.db.GetPeer(sender.String())
	if err != nil || rec == nil || rec.Name == p.Name {
		return
	}
	rec.Name = p.Name
	rec.LastSeen = time.Now().UnixMilli()
	if err := h.db.UpsertPeer(rec); err != nil {
		return
	}
	h.bus.publish(Event{Kind: EventPeerUpdated, Peer: &PeerInfo{
		ID: sender, Name: rec.Name, Platform: rec.Platform,
		Fingerprint: rec.Fingerprint, Trust: rec.Trust,
		Online: true, LastSeen: time.UnixMilli(rec.LastSeen),
	}})
}

// ---------------------------------------------------------------------------
// 发送
// ---------------------------------------------------------------------------

// SendChat 发送聊天消息到 DM 或群组广播会话。返回 message_id。
func (h *Hub) SendChat(ctx context.Context, conv ConversationID, text, replyTo string) (string, error) {
	if len(text) == 0 {
		return "", errors.New("hub: 空消息")
	}
	if len(text) > maxTextLen {
		return "", fmt.Errorf("hub: 消息超过 %d 字节上限", maxTextLen)
	}

	msgID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UnixMilli()

	peerID := h.selfID.String()
	if conv != GroupLAN {
		pid, ok := PeerFromConversation(conv)
		if !ok {
			return "", errors.New("hub: 非法会话 ID")
		}
		peerID = pid.String()
	}

	msg := &store.Message{
		ID:           msgID,
		Conversation: conv,
		PeerID:       peerID,
		Direction:    store.DirectionOutbound,
		Kind:         store.KindText,
		Text:         text,
		SentAt:       now,
		Status:       store.StatusSending,
	}
	if replyTo != "" {
		msg.ReplyTo = &replyTo
	}
	if _, err := h.db.InsertMessage(msg); err != nil {
		return "", err
	}
	h.bus.publish(Event{Kind: EventMessageSent, Conversation: conv, Message: msg})

	env := &fireflyv1.Envelope{
		ProtoVersion: 1,
		MessageId:    msgID,
		SentAtUnixMs: now,
		Payload: &fireflyv1.Envelope_Chat{Chat: &fireflyv1.Chat{
			ConversationId: conv,
			Text:           text,
			ReplyTo:        replyTo,
		}},
	}

	if conv == GroupLAN {
		h.fanout(ctx, env, msgID)
	} else {
		pid, _ := PeerFromConversation(conv)
		h.receipts.register(msgID, []string{pid.String()})
		if err := h.Send(ctx, pid, env); err != nil {
			h.receipts.forget(msgID)
			_ = h.db.UpdateMessageStatus(msgID, store.StatusFailed)
			h.bus.publish(Event{Kind: EventReceiptUpdated, ReceiptID: msgID, Receipt: store.StatusFailed, Err: err})
			return msgID, err
		}
		_ = h.db.UpdateMessageStatus(msgID, store.StatusSent)
		h.bus.publish(Event{Kind: EventReceiptUpdated, ReceiptID: msgID, Receipt: store.StatusSent})
	}
	return msgID, nil
}

// fanout 群播扇出：同一 Envelope 逐一加密发送给所有在线且未拉黑的 peer。
func (h *Hub) fanout(ctx context.Context, env *fireflyv1.Envelope, msgID string) {
	sessions := h.pm.onlineSessions()
	targets := make([]transport.Session, 0, len(sessions))
	peerIDs := make([]string, 0, len(sessions))
	for _, s := range sessions {
		switch h.TrustOf(s.Peer()) {
		case store.TrustBlocked, store.TrustKeyChanged:
			continue
		}
		targets = append(targets, s)
		peerIDs = append(peerIDs, s.Peer().String())
	}
	if len(targets) == 0 {
		_ = h.db.UpdateMessageStatus(msgID, store.StatusSent) // 无人在线也算已发（本地留档）
		h.bus.publish(Event{Kind: EventReceiptUpdated, ReceiptID: msgID, Receipt: store.StatusSent})
		return
	}
	h.receipts.register(msgID, peerIDs)

	sent := 0
	for _, s := range targets {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := s.Send(c, env); err != nil {
			h.log.Debug("群播发送失败", "peer", s.Peer().String()[:8], "err", err)
			h.receipts.forgetPeer(msgID, s.Peer().String()) // 不再期待其回执
		} else {
			sent++
		}
		cancel()
	}
	if sent > 0 {
		_ = h.db.UpdateMessageStatus(msgID, store.StatusSent)
		h.bus.publish(Event{Kind: EventReceiptUpdated, ReceiptID: msgID, Receipt: store.StatusSent})
	} else {
		h.receipts.forget(msgID)
		_ = h.db.UpdateMessageStatus(msgID, store.StatusFailed)
		h.bus.publish(Event{Kind: EventReceiptUpdated, ReceiptID: msgID, Receipt: store.StatusFailed, Err: errors.New("全部发送失败")})
	}
}

// MarkRead 将当前会话标记为已读，并向对端发送 READ 回执。
func (h *Hub) MarkRead(ctx context.Context, conv ConversationID) {
	ids, err := h.db.MarkConversationRead(conv)
	if err != nil || len(ids) == 0 {
		return
	}
	var targets []transport.Session
	if conv == GroupLAN {
		targets = h.pm.onlineSessions()
	} else if pid, ok := PeerFromConversation(conv); ok {
		if s := h.pm.session(pid); s != nil {
			targets = []transport.Session{s}
		}
	}
	for _, id := range ids {
		env := &fireflyv1.Envelope{
			ProtoVersion: 1,
			MessageId:    uuid.Must(uuid.NewV7()).String(),
			SentAtUnixMs: time.Now().UnixMilli(),
			Payload: &fireflyv1.Envelope_Receipt{Receipt: &fireflyv1.Receipt{
				MessageId: id,
				Kind:      fireflyv1.ReceiptKind_RECEIPT_KIND_READ,
			}},
		}
		for _, s := range targets {
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = s.Send(c, env)
			cancel()
		}
	}
}

// Peers 返回全部已知设备（在线状态合并持久化信任），供 UI 侧栏。
func (h *Hub) Peers() []*PeerInfo {
	recs, err := h.db.ListPeers()
	if err != nil {
		h.log.Error("读取 peers 失败", "err", err)
		return nil
	}
	out := make([]*PeerInfo, 0, len(recs))
	for _, r := range recs {
		pid, err := identity.PeerIDFromHex(r.ID)
		if err != nil {
			continue
		}
		out = append(out, &PeerInfo{
			ID: pid, Name: r.Name, Platform: r.Platform,
			Fingerprint: r.Fingerprint, Trust: r.Trust,
			Online:   h.pm.isOnline(pid),
			LastSeen: time.UnixMilli(r.LastSeen),
		})
	}
	return out
}

// recordFileMessage 传输完成后在会话中补记一条文件消息。
func (h *Hub) recordFileMessage(st *filetransfer.State) {
	conv := DMConversation(st.Peer)
	now := time.Now().UnixMilli()
	fileID := st.FileID
	msg := &store.Message{
		ID:           uuid.Must(uuid.NewV7()).String(),
		Conversation: conv,
		PeerID:       st.Peer.String(),
		Direction:    st.Direction,
		Kind:         store.KindFile,
		Text:         fmt.Sprintf("📎 %s (%s)", st.Name, humanSize(st.Size)),
		FileID:       &fileID,
		SentAt:       now,
		Status:       store.StatusDelivered,
	}
	if st.Direction == filetransfer.Outbound {
		msg.Status = store.StatusSent
	}
	inserted, err := h.db.InsertMessage(msg)
	if err != nil || !inserted {
		return
	}
	kind := EventMessageReceived
	if st.Direction == filetransfer.Outbound {
		kind = EventMessageSent
	}
	h.bus.publish(Event{Kind: kind, Conversation: conv, Message: msg})
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ---------------------------------------------------------------------------
// 送达回执计数（DM 与群播共用）
// ---------------------------------------------------------------------------

type receiptTracker struct {
	mu      sync.Mutex
	pending map[string]map[string]bool // messageID → 未回执 peer 集合
}

func newReceiptTracker() *receiptTracker {
	return &receiptTracker{pending: make(map[string]map[string]bool)}
}

func (rt *receiptTracker) register(msgID string, peers []string) {
	if len(peers) == 0 {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	set := make(map[string]bool, len(peers))
	for _, p := range peers {
		set[p] = true
	}
	rt.pending[msgID] = set
}

// ack 记录一个回执。返回 (是否在追踪中, 是否全部完成)。
func (rt *receiptTracker) ack(msgID, peer string) (tracked bool, allDone bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	set, ok := rt.pending[msgID]
	if !ok {
		return false, false
	}
	delete(set, peer)
	if len(set) == 0 {
		delete(rt.pending, msgID)
		return true, true
	}
	return true, false
}

// forgetPeer 移除一个发送失败的目标；返回 true 表示集合已空（已全部失败/完成）。
func (rt *receiptTracker) forgetPeer(msgID, peer string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	set, ok := rt.pending[msgID]
	if !ok {
		return true
	}
	delete(set, peer)
	if len(set) == 0 {
		delete(rt.pending, msgID)
		return true
	}
	return false
}

func (rt *receiptTracker) forget(msgID string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.pending, msgID)
}
