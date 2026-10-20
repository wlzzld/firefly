package discovery

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lei/firefly/internal/identity"
)

func testPeerID(t *testing.T, seed byte) identity.PeerID {
	t.Helper()
	var id identity.PeerID
	for i := range id {
		id[i] = seed
	}
	return id
}

func sighting(id identity.PeerID, addr string) rawPeerSighting {
	return rawPeerSighting{
		id:          id,
		name:        "peer-" + string(rune('a'+id[0])),
		platform:    "darwin",
		fingerprint: "AAAA-BBBB-CCCC-DDDD",
		tcpPort:     51770,
		addr:        netip.MustParseAddrPort(addr),
		source:      SourceBroadcast,
	}
}

func expectEvent(t *testing.T, idx *peerIndex, kind EventKind) Peer {
	t.Helper()
	select {
	case ev := <-idx.events:
		require.Equal(t, kind, ev.Kind)
		return ev.Peer
	case <-time.After(time.Second):
		t.Fatalf("等待事件 kind=%d 超时", kind)
		return Peer{}
	}
}

func expectNoEvent(t *testing.T, idx *peerIndex) {
	t.Helper()
	select {
	case ev := <-idx.events:
		t.Fatalf("意外收到事件 kind=%d peer=%s", ev.Kind, ev.Peer.ID.String()[:8])
	case <-time.After(50 * time.Millisecond):
	}
}

func TestObserveSelfIsFiltered(t *testing.T) {
	self := testPeerID(t, 0x01)
	idx := newPeerIndex(self)

	idx.observe(sighting(self, "192.168.1.10:51770"))
	expectNoEvent(t, idx)
	assert.Empty(t, idx.snapshot())
}

func TestObservePeerUpAndDedup(t *testing.T) {
	self := testPeerID(t, 0x01)
	other := testPeerID(t, 0x02)
	idx := newPeerIndex(self)

	idx.observe(sighting(other, "192.168.1.20:51770"))
	p := expectEvent(t, idx, PeerUp)
	assert.Equal(t, other, p.ID)
	assert.Equal(t, "darwin", p.Platform)
	assert.Len(t, p.Addrs, 1)

	// 相同内容重复观测 → 不产生事件（心跳静默）
	idx.observe(sighting(other, "192.168.1.20:51770"))
	expectNoEvent(t, idx)

	// 新地址 → PeerUpdated，双通道 source 合并
	s2 := sighting(other, "10.0.0.5:51770")
	s2.source = SourceMDNS
	idx.observe(s2)
	p = expectEvent(t, idx, PeerUpdated)
	assert.Len(t, p.Addrs, 2)
	assert.Equal(t, SourceBroadcast|SourceMDNS, p.Source)
}

func TestObserveAddrCap(t *testing.T) {
	self := testPeerID(t, 0x01)
	other := testPeerID(t, 0x02)
	idx := newPeerIndex(self)

	idx.observe(sighting(other, "192.168.1.20:51770"))
	expectEvent(t, idx, PeerUp)
	for i := 0; i < maxAddrsPerPeer+2; i++ {
		s := sighting(other, netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 0, 0, byte(i + 1)}), 51770).String())
		idx.observe(s)
		<-idx.events // 消费 PeerUpdated
	}
	idx.mu.Lock()
	n := len(idx.peers[other].peer.Addrs)
	idx.mu.Unlock()
	assert.LessOrEqual(t, n, maxAddrsPerPeer, "候选地址数量必须有上限")
}

func TestGoodbyeMarksOffline(t *testing.T) {
	self := testPeerID(t, 0x01)
	other := testPeerID(t, 0x02)
	idx := newPeerIndex(self)

	idx.observe(sighting(other, "192.168.1.20:51770"))
	expectEvent(t, idx, PeerUp)

	gb := rawPeerSighting{id: other, goodbye: true, source: SourceBroadcast}
	idx.observe(gb)
	expectEvent(t, idx, PeerDown)
	assert.Empty(t, idx.snapshot())

	// 未知 peer 的 goodbye 不产生事件
	idx.observe(rawPeerSighting{id: testPeerID(t, 0x99), goodbye: true})
	expectNoEvent(t, idx)
}

func TestSweepExpiresStalePeer(t *testing.T) {
	self := testPeerID(t, 0x01)
	other := testPeerID(t, 0x02)
	idx := newPeerIndex(self)

	idx.observe(sighting(other, "192.168.1.20:51770"))
	expectEvent(t, idx, PeerUp)

	// 直接把 lastSeen 拨回 TTL 之前（避免真实等待 9s）
	idx.mu.Lock()
	idx.peers[other].lastSeen = time.Now().Add(-peerTTL - time.Second)
	idx.mu.Unlock()

	idx.sweep()
	expectEvent(t, idx, PeerDown)
	assert.Empty(t, idx.snapshot())

	// 重新观测 → 再次 PeerUp
	idx.observe(sighting(other, "192.168.1.20:51770"))
	expectEvent(t, idx, PeerUp)
}

func TestNotifyDown(t *testing.T) {
	self := testPeerID(t, 0x01)
	other := testPeerID(t, 0x02)
	idx := newPeerIndex(self)

	idx.observe(sighting(other, "192.168.1.20:51770"))
	expectEvent(t, idx, PeerUp)

	idx.notifyDown(other)
	expectEvent(t, idx, PeerDown)

	// 已离线的 peer 再次 notifyDown 不重复发事件
	idx.notifyDown(other)
	expectNoEvent(t, idx)
}
