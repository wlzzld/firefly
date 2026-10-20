package transport

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/flynn/noise"
	"google.golang.org/protobuf/proto"

	"github.com/lei/firefly/internal/identity"
	fireflyv1 "github.com/lei/firefly/internal/protocol/pb/firefly/v1"
)

// rekeyEvery 每 N 条消息对收发 CipherState 各做一次对称 rekey
// （双方按相同消息计数触发，保持同步）。
const rekeyEvery = 10000

// readIdleTimeout：读空闲超时上限。应用层有周期性 presence 心跳，
// 超时说明对端异常，触发重连（hub 负责）。
const readIdleTimeout = 90 * time.Second

// Session 是一条已认证、加密的 P2P 会话。
type Session interface {
	// Peer 返回对端 PeerID（经握手签名验证的 Ed25519 公钥）。
	Peer() identity.PeerID
	// RemotePub 返回对端身份公钥原始字节。
	RemotePub() ed25519.PublicKey
	// RemoteName 返回握手时对端自报的显示名。
	RemoteName() string
	// Send 发送一个 Envelope（线程安全）。
	Send(ctx context.Context, env *fireflyv1.Envelope) error
	// Recv 返回入站 Envelope 流；通道关闭表示会话结束。
	Recv() <-chan *fireflyv1.Envelope
	// Err 返回导致会话结束的错误（未结束为 nil）。
	Err() error
	// RemoteAddr 返回底层 TCP 对端地址（文件连接复用同地址拨号）。
	RemoteAddr() net.Addr
	// Close 关闭会话。
	Close() error
}

type session struct {
	conn       net.Conn
	sendCS     *noise.CipherState
	recvCS     *noise.CipherState
	sendMu     sync.Mutex // 保护 sendCS 与 conn 写路径（单写者）
	sendN      uint64
	recvN      uint64 // 仅 readLoop 单 goroutine 访问
	peer       identity.PeerID
	remotePub  ed25519.PublicKey
	remoteName string
	incoming   chan *fireflyv1.Envelope
	log        *slog.Logger

	closeOnce sync.Once
	done      chan struct{}
	errMu     sync.Mutex
	err       error
}

func newSession(conn net.Conn, sendCS, recvCS *noise.CipherState, peer identity.PeerID, pub ed25519.PublicKey, name string, log *slog.Logger) *session {
	s := &session{
		conn:       conn,
		sendCS:     sendCS,
		recvCS:     recvCS,
		peer:       peer,
		remotePub:  pub,
		remoteName: name,
		incoming:   make(chan *fireflyv1.Envelope, 256),
		log:        log,
		done:       make(chan struct{}),
	}
	go s.readLoop()
	return s
}

// readLoop 是唯一读者：recvN/recvCS 无并发访问，不需加锁。
func (s *session) readLoop() {
	defer close(s.incoming)
	for {
		select {
		case <-s.done:
			return
		default:
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(readIdleTimeout))
		plaintext, err := readFrame(s.conn, s.recvCS)
		if err != nil {
			if s.isClosing() {
				return
			}
			s.fail(err)
			return
		}
		s.recvN++
		if s.recvN%rekeyEvery == 0 {
			s.recvCS.Rekey()
		}
		var env fireflyv1.Envelope
		if err := proto.Unmarshal(plaintext, &env); err != nil {
			s.fail(err)
			return
		}
		if env.ProtoVersion != 1 {
			s.fail(errors.New("transport: 不支持的协议版本"))
			return
		}
		select {
		case s.incoming <- &env:
		case <-s.done:
			return
		}
	}
}

func (s *session) Send(ctx context.Context, env *fireflyv1.Envelope) error {
	select {
	case <-s.done:
		if err := s.Err(); err != nil {
			return err
		}
		return ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if env.ProtoVersion == 0 {
		env.ProtoVersion = 1
	}
	plaintext, err := proto.Marshal(env)
	if err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	// 第 10001、20001… 条消息起使用 rekey 后的新密钥（与对端读侧计数对称）
	s.sendN++
	if s.sendN > 1 && (s.sendN-1)%rekeyEvery == 0 {
		s.sendCS.Rekey()
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = s.conn.SetWriteDeadline(deadline)
		defer s.conn.SetWriteDeadline(time.Time{}) //nolint:errcheck
	}
	return encryptFrame(s.conn, s.sendCS, plaintext)
}

func (s *session) Peer() identity.PeerID            { return s.peer }
func (s *session) RemotePub() ed25519.PublicKey     { return s.remotePub }
func (s *session) RemoteName() string               { return s.remoteName }
func (s *session) Recv() <-chan *fireflyv1.Envelope { return s.incoming }
func (s *session) RemoteAddr() net.Addr             { return s.conn.RemoteAddr() }

func (s *session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *session) fail(err error) {
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
	s.Close() //nolint:errcheck
}

func (s *session) isClosing() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		err = s.conn.Close()
	})
	return err
}
