package transport

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"

	"github.com/lei/firefly/internal/identity"
)

// Listener 监听入站连接，为每条连接完成 responder 握手后投递到 Incoming。
type Listener struct {
	ln       *net.TCPListener
	id       *identity.Identity
	log      *slog.Logger
	incoming chan Session

	closeOnce sync.Once
	done      chan struct{}
}

// Listen 绑定 TCP 端口；preferredPort 被占用时降级为系统分配端口（返回实际地址）。
func Listen(ctx context.Context, id *identity.Identity, preferredPort int, log *slog.Logger) (*Listener, error) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{Port: preferredPort})
	if err != nil {
		log.Warn("TCP 端口被占用，降级为随机端口", "port", preferredPort, "err", err)
		ln, err = net.ListenTCP("tcp", &net.TCPAddr{Port: 0})
		if err != nil {
			return nil, err
		}
	}
	l := &Listener{
		ln:       ln,
		id:       id,
		log:      log,
		incoming: make(chan Session, 32),
		done:     make(chan struct{}),
	}
	go l.acceptLoop()
	return l, nil
}

// Addr 返回实际监听地址（含降级后的真实端口，供发现层广告）。
func (l *Listener) Addr() netip.AddrPort {
	addr := l.ln.Addr().(*net.TCPAddr)
	ip, _ := netip.AddrFromSlice(addr.IP)
	return netip.AddrPortFrom(ip.Unmap(), uint16(addr.Port))
}

// Port 返回实际监听端口。
func (l *Listener) Port() int {
	return l.ln.Addr().(*net.TCPAddr).Port
}

func (l *Listener) Incoming() <-chan Session { return l.incoming }

func (l *Listener) acceptLoop() {
	for {
		conn, err := l.ln.AcceptTCP()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			l.log.Debug("accept 失败", "err", err)
			return
		}
		_ = conn.SetNoDelay(true)
		_ = conn.SetKeepAlive(true)
		go l.handleConn(conn)
	}
}

func (l *Listener) handleConn(conn net.Conn) {
	conn = wrapConn(conn)
	s, err := handshakeResponder(conn, l.id, l.log)
	if err != nil {
		conn.Close() //nolint:errcheck
		if !errors.Is(err, context.Canceled) {
			l.log.Debug("入站握手失败", "remote", conn.RemoteAddr(), "err", err)
		}
		return
	}
	select {
	case l.incoming <- s:
	case <-l.done:
		s.Close() //nolint:errcheck
	}
}

func (l *Listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.done)
		err = l.ln.Close()
	})
	return err
}
