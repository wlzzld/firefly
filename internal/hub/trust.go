package hub

import (
	"time"

	"github.com/lei/firefly/internal/identity"
	"github.com/lei/firefly/internal/store"
	"github.com/lei/firefly/internal/transport"
)

// checkTrust 在会话建立时执行 TOFU 信任检查：
//   - 首次见到该 PeerID → 入库 unverified，放行（UI 弹配对框由 PeerUp 事件触发）
//   - trusted / unverified → 放行
//   - blocked / key_changed → 拒绝会话
//
// 另外做"同名异钥"启发式检测：若库中存在同名且 trusted 的其他设备，
// 发布警告 Notice（不阻断，交给用户判断）。
func (h *Hub) checkTrust(s transport.Session) bool {
	pid := s.Peer()
	rec, err := h.db.GetPeer(pid.String())
	if err != nil {
		h.log.Error("读取 peer 信任状态失败", "err", err)
		return false // fail-closed
	}
	if rec == nil {
		// TOFU 首见
		now := time.Now().UnixMilli()
		rec = &store.Peer{
			ID:          pid.String(),
			Name:        s.RemoteName(),
			Fingerprint: identity.FingerprintOf(s.RemotePub()),
			Trust:       store.TrustUnverified,
			FirstSeen:   now,
			LastSeen:    now,
		}
		if err := h.db.UpsertPeer(rec); err != nil {
			h.log.Error("写入新 peer 失败", "err", err)
			return false
		}
		h.warnSameNameDifferentKey(rec)
		return true
	}

	switch rec.Trust {
	case store.TrustBlocked, store.TrustKeyChanged:
		h.log.Warn("拒绝会话：信任状态为 "+string(rec.Trust), "peer", rec.Name)
		return false
	default:
		return true
	}
}

// warnSameNameDifferentKey 检测"同名但不同公钥"的可疑设备（可能是仿冒，
// 也可能只是重名），发布 Notice 供 UI 提示，不阻断通信。
func (h *Hub) warnSameNameDifferentKey(rec *store.Peer) {
	if rec.Name == "" {
		return
	}
	peers, err := h.db.ListPeers()
	if err != nil {
		return
	}
	for _, p := range peers {
		if p.ID != rec.ID && p.Name == rec.Name && p.Trust == store.TrustTrusted {
			h.bus.publish(Event{
				Kind: EventNotice,
				Notice: "警告：发现与你已信任设备同名的新设备「" + rec.Name +
					"」（指纹 " + rec.Fingerprint + "），请仔细核对后再信任。",
			})
			return
		}
	}
}

// SetTrust 更新 peer 信任状态（用户在配对对话框中操作）。
// blocked 时立即关闭活跃会话；重新信任不自动重连（等发现层下一心跳）。
func (h *Hub) SetTrust(peer identity.PeerID, trust store.TrustState) error {
	if err := h.db.SetTrust(peer.String(), trust); err != nil {
		return err
	}
	if trust == store.TrustBlocked {
		h.pm.closeSession(peer)
	}
	rec, _ := h.db.GetPeer(peer.String())
	if rec != nil {
		h.bus.publish(Event{
			Kind: EventTrustChanged,
			Peer: &PeerInfo{
				ID: peer, Name: rec.Name, Platform: rec.Platform,
				Fingerprint: rec.Fingerprint, Trust: trust,
				Online: h.pm.isOnline(peer), LastSeen: time.UnixMilli(rec.LastSeen),
			},
		})
	}
	return nil
}

// TrustOf 返回某 peer 的当前信任状态。
func (h *Hub) TrustOf(peer identity.PeerID) store.TrustState {
	rec, err := h.db.GetPeer(peer.String())
	if err != nil || rec == nil {
		return store.TrustUnverified
	}
	return rec.Trust
}
