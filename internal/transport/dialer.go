package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/lei/firefly/internal/discovery"
	"github.com/lei/firefly/internal/identity"
)

// dialTimeout 是单个候选地址的 TCP 连接超时。
const dialTimeout = 5 * time.Second

// Dialer 发起出站连接并完成 XX 握手。
type Dialer struct {
	id  *identity.Identity
	log *slog.Logger
}

func NewDialer(id *identity.Identity, log *slog.Logger) *Dialer {
	return &Dialer{id: id, log: log}
}

// Dial 依次尝试 peer 的候选地址，任一成功即返回会话。
func (d *Dialer) Dial(ctx context.Context, peer discovery.Peer) (Session, error) {
	if len(peer.Addrs) == 0 {
		return nil, fmt.Errorf("transport: peer %s 无可用地址", peer.Name)
	}
	var lastErr error
	for _, addr := range peer.Addrs {
		s, err := d.DialAddr(ctx, addr)
		if err == nil {
			// 校验拨到的设备身份与发现层线索一致（防止地址错乱/伪造广播）
			if s.Peer() != peer.ID {
				s.Close() //nolint:errcheck
				lastErr = fmt.Errorf("transport: %s 身份不匹配（发现层 %s，握手 %s）",
					addr, peer.ID.String()[:8], s.Peer().String()[:8])
				continue
			}
			return s, nil
		}
		lastErr = errors.Join(lastErr, err)
	}
	return nil, fmt.Errorf("transport: 拨号 %s 失败: %w", peer.Name, lastErr)
}

// DialAddr 连接指定地址并完成握手（文件传输连接也复用此入口）。
func (d *Dialer) DialAddr(ctx context.Context, addr netip.AddrPort) (Session, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(dctx, "tcp", addr.String())
	if err != nil {
		return nil, err
	}
	tconn, ok := conn.(*net.TCPConn)
	if ok {
		_ = tconn.SetNoDelay(true)
		_ = tconn.SetKeepAlive(true)
		_ = tconn.SetKeepAlivePeriod(30 * time.Second)
	}
	conn = wrapConn(conn)

	s, err := handshakeInitiator(conn, d.id, d.log)
	if err != nil {
		conn.Close() //nolint:errcheck
		return nil, err
	}
	return s, nil
}

// ShouldDial 实现连接方向规则：PeerID 字典序大者为拨号方。
// 双方同时拨号的冲突由 hub 的 PeerManager 去重（保留拨号方会话）。
func ShouldDial(selfID, peerID identity.PeerID) bool {
	for i := 0; i < 32; i++ {
		if selfID[i] != peerID[i] {
			return selfID[i] > peerID[i]
		}
	}
	return false // 相同 ID（自己），不拨号
}
