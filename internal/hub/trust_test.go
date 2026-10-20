package hub

import (
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lei/firefly/internal/identity"
	fireflyv1 "github.com/lei/firefly/internal/protocol/pb/firefly/v1"
	"github.com/lei/firefly/internal/store"
)

// fakeSession 是 transport.Session 的测试替身。
type fakeSession struct {
	peer identity.PeerID
	pub  ed25519.PublicKey
	name string
	recv chan *fireflyv1.Envelope
}

func newFakeSession(t *testing.T, name string) *fakeSession {
	t.Helper()
	dir := t.TempDir()
	id, err := identity.LoadOrCreate(dir)
	require.NoError(t, err)
	return &fakeSession{peer: id.PeerID, pub: id.Pub, name: name,
		recv: make(chan *fireflyv1.Envelope, 8)}
}

func (f *fakeSession) Peer() identity.PeerID        { return f.peer }
func (f *fakeSession) RemotePub() ed25519.PublicKey { return f.pub }
func (f *fakeSession) RemoteName() string           { return f.name }
func (f *fakeSession) Send(context.Context, *fireflyv1.Envelope) error {
	return nil
}
func (f *fakeSession) Recv() <-chan *fireflyv1.Envelope { return f.recv }
func (f *fakeSession) Err() error                       { return nil }
func (f *fakeSession) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}
func (f *fakeSession) Close() error { return nil }

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	db, err := store.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &Hub{
		nameFn:   func() string { return "self" },
		db:       db,
		log:      log,
		bus:      newBus(),
		stop:     make(chan struct{}),
		receipts: newReceiptTracker(),
	}
	h.pm = newPeerManager(h.selfID, nil, log)
	return h
}

func TestCheckTrustTOFUFirstSeen(t *testing.T) {
	h := newTestHub(t)
	s := newFakeSession(t, "新设备")

	require.True(t, h.checkTrust(s), "首见设备应放行（TOFU）")

	rec, err := h.db.GetPeer(s.peer.String())
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, store.TrustUnverified, rec.Trust)
	assert.Equal(t, "新设备", rec.Name)
	assert.Equal(t, identity.FingerprintOf(s.pub), rec.Fingerprint)
}

func TestCheckTrustStates(t *testing.T) {
	h := newTestHub(t)
	s := newFakeSession(t, "设备X")
	require.True(t, h.checkTrust(s)) // 首次入库

	cases := []struct {
		trust store.TrustState
		allow bool
	}{
		{store.TrustUnverified, true},
		{store.TrustTrusted, true},
		{store.TrustBlocked, false},
		{store.TrustKeyChanged, false},
	}
	for _, c := range cases {
		require.NoError(t, h.db.SetTrust(s.peer.String(), c.trust))
		assert.Equal(t, c.allow, h.checkTrust(s), "trust=%s", c.trust)
	}
}

func TestWarnSameNameDifferentKey(t *testing.T) {
	h := newTestHub(t)
	events, _ := h.Subscribe()

	// 已信任设备「办公室Mac」
	trusted := newFakeSession(t, "办公室Mac")
	require.True(t, h.checkTrust(trusted))
	require.NoError(t, h.SetTrust(trusted.peer, store.TrustTrusted))

	// 同名新设备（不同公钥）首见 → 应发布警告 Notice
	impersonator := newFakeSession(t, "办公室Mac")
	require.True(t, h.checkTrust(impersonator), "同名异钥不阻断，交给用户判断")

	var warned bool
	for i := 0; i < 4; i++ {
		select {
		case ev := <-events:
			if ev.Kind == EventNotice && strings.Contains(ev.Notice, "同名") {
				warned = true
			}
		default:
		}
		if warned {
			break
		}
	}
	assert.True(t, warned, "应发布同名异钥警告")
}

func TestSetTrustPublishesEvent(t *testing.T) {
	h := newTestHub(t)
	events, _ := h.Subscribe()
	s := newFakeSession(t, "设备Y")
	require.True(t, h.checkTrust(s))

	require.NoError(t, h.SetTrust(s.peer, store.TrustTrusted))
	assert.Equal(t, store.TrustTrusted, h.TrustOf(s.peer))

	select {
	case ev := <-events:
		assert.Equal(t, EventTrustChanged, ev.Kind)
		require.NotNil(t, ev.Peer)
		assert.Equal(t, store.TrustTrusted, ev.Peer.Trust)
	default:
		t.Fatal("SetTrust 应发布 EventTrustChanged")
	}
}

func TestTrustOfUnknownPeer(t *testing.T) {
	h := newTestHub(t)
	var unknown identity.PeerID
	unknown[0] = 0xff
	assert.Equal(t, store.TrustUnverified, h.TrustOf(unknown))
}
