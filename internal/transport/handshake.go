package transport

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/flynn/noise"
	"google.golang.org/protobuf/proto"

	"github.com/lei/firefly/internal/identity"
	fireflyv1 "github.com/lei/firefly/internal/protocol/pb/firefly/v1"
)

// sigPrefix 是签名域分隔符：sig = Ed25519.Sign(priv, prefix || handshake_hash || x25519_static_pub)
// 参考 libp2p noise spec 的签名绑定模式。
var sigPrefix = []byte("firefly/1")

// prologue 绑定协议上下文，防止跨协议重放。
var prologue = []byte("firefly/1")

// handshakeTimeout 是整个 XX 三消息握手的超时。
const handshakeTimeout = 10 * time.Second

func cipherSuite() noise.CipherSuite {
	return noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)
}

func staticDHKey(id *identity.Identity) noise.DHKey {
	return noise.DHKey{
		Private: id.Static.Bytes(),
		Public:  id.Static.PublicKey().Bytes(),
	}
}

// transcript 返回当前握手转录哈希（channel binding）的副本。
// 用于在混入签名 payload 之前固定检查点，两端据此对静态密钥做签名绑定。
// 必须复制：noise 内部会复用哈希缓冲，后续步骤会覆盖。
func transcript(hs *noise.HandshakeState) []byte {
	h := hs.ChannelBinding()
	out := make([]byte, len(h))
	copy(out, h)
	return out
}

// 握手消息同样使用 [u32be len][payload] 分帧（TCP 流式传输必须处理半包）。

func writeHSMsg(conn net.Conn, msg []byte) error {
	buf := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(msg)))
	copy(buf[4:], msg)
	_, err := conn.Write(buf)
	return err
}

func readHSMsg(conn net.Conn) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxCiphertextLen {
		return nil, fmt.Errorf("transport: 非法握手消息长度 %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// buildPayload 生成并签名握手 payload。transcriptHash 必须是"混入该 payload 之前"的转录检查点。
func buildPayload(id *identity.Identity, transcriptHash []byte, name string) ([]byte, error) {
	sigMsg := make([]byte, 0, len(sigPrefix)+len(transcriptHash)+32)
	sigMsg = append(sigMsg, sigPrefix...)
	sigMsg = append(sigMsg, transcriptHash...)
	sigMsg = append(sigMsg, id.Static.PublicKey().Bytes()...)

	payload := &fireflyv1.HandshakePayload{
		Ed25519Pub: id.Pub,
		Sig:        id.Sign(sigMsg),
		Name:       name,
	}
	return proto.Marshal(payload)
}

// verifyPayload 校验对端握手 payload 与签名。peerStatic 是对端 Noise X25519 静态公钥。
func verifyPayload(data, transcriptHash, peerStatic []byte) (identity.PeerID, ed25519.PublicKey, string, error) {
	var payload fireflyv1.HandshakePayload
	if err := proto.Unmarshal(data, &payload); err != nil {
		return identity.PeerID{}, nil, "", fmt.Errorf("transport: 握手 payload 解析失败: %w", err)
	}
	if len(payload.Ed25519Pub) != ed25519.PublicKeySize {
		return identity.PeerID{}, nil, "", errors.New("transport: 握手 payload 公钥长度非法")
	}
	sigMsg := make([]byte, 0, len(sigPrefix)+len(transcriptHash)+len(peerStatic))
	sigMsg = append(sigMsg, sigPrefix...)
	sigMsg = append(sigMsg, transcriptHash...)
	sigMsg = append(sigMsg, peerStatic...)
	if !ed25519.Verify(payload.Ed25519Pub, sigMsg, payload.Sig) {
		return identity.PeerID{}, nil, "", errors.New("transport: 握手签名验证失败（可能的中间人）")
	}
	pid, err := identity.PeerIDFromBytes(payload.Ed25519Pub)
	if err != nil {
		return identity.PeerID{}, nil, "", err
	}
	return pid, payload.Ed25519Pub, payload.Name, nil
}

// handshakeInitiator 执行 XX 握手（拨号方）。
// 消息流: → e | ← e,ee,s,es(payload2) | → s,se(payload3)
// flynn/noise 完成握手的调用返回 (c1, c2)：c1 加密 initiator→responder，c2 加密反向。
// 故 initiator：send=c1，recv=c2。
func handshakeInitiator(conn net.Conn, id *identity.Identity, log *slog.Logger) (*session, error) {
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   cipherSuite(),
		Pattern:       noise.HandshakeXX,
		Initiator:     true,
		StaticKeypair: staticDHKey(id),
		Prologue:      prologue,
	})
	if err != nil {
		return nil, err
	}

	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetDeadline(time.Time{}) //nolint:errcheck

	// msg1: → e（无 payload）
	msg1, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	if err := writeHSMsg(conn, msg1); err != nil {
		return nil, err
	}

	// msg2: ← e,ee,s,es + 对端签名 payload
	// 验证哈希 h1 = 读 msg2 前的检查点（两端一致：均为 msg1 混入后的转录哈希）
	h1 := transcript(hs)
	msg2, err := readHSMsg(conn)
	if err != nil {
		return nil, err
	}
	payload2, _, _, err := hs.ReadMessage(nil, msg2)
	if err != nil {
		return nil, err
	}
	peerID, peerPub, peerName, err := verifyPayload(payload2, h1, hs.PeerStatic())
	if err != nil {
		return nil, err
	}

	// msg3: → s,se + 本端签名 payload（哈希 h2 = msg2 混入后）
	h2 := transcript(hs)
	payload3, err := buildPayload(id, h2, currentName(id))
	if err != nil {
		return nil, err
	}
	msg3, c1, c2, err := hs.WriteMessage(nil, payload3)
	if err != nil {
		return nil, err
	}
	if c1 == nil || c2 == nil {
		return nil, errors.New("transport: 握手未完成（内部错误）")
	}
	if err := writeHSMsg(conn, msg3); err != nil {
		return nil, err
	}

	return newSession(conn, c1, c2, peerID, peerPub, peerName, log), nil
}

// handshakeResponder 执行 XX 握手（监听方）。
// responder：send=c2，recv=c1。
func handshakeResponder(conn net.Conn, id *identity.Identity, log *slog.Logger) (*session, error) {
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   cipherSuite(),
		Pattern:       noise.HandshakeXX,
		Initiator:     false,
		StaticKeypair: staticDHKey(id),
		Prologue:      prologue,
	})
	if err != nil {
		return nil, err
	}

	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetDeadline(time.Time{}) //nolint:errcheck

	// msg1: ← e
	msg1, err := readHSMsg(conn)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := hs.ReadMessage(nil, msg1); err != nil {
		return nil, err
	}

	// msg2: → e,ee,s,es + 本端签名 payload（哈希 h1 = msg1 混入后）
	h1 := transcript(hs)
	payload2, err := buildPayload(id, h1, currentName(id))
	if err != nil {
		return nil, err
	}
	msg2, _, _, err := hs.WriteMessage(nil, payload2)
	if err != nil {
		return nil, err
	}
	if err := writeHSMsg(conn, msg2); err != nil {
		return nil, err
	}

	// msg3: ← s,se + 对端签名 payload（哈希 h2 = msg2 混入后）
	h2 := transcript(hs)
	msg3, err := readHSMsg(conn)
	if err != nil {
		return nil, err
	}
	payload3, c1, c2, err := hs.ReadMessage(nil, msg3)
	if err != nil {
		return nil, err
	}
	if c1 == nil || c2 == nil {
		return nil, errors.New("transport: 握手未完成（内部错误）")
	}
	peerID, peerPub, peerName, err := verifyPayload(payload3, h2, hs.PeerStatic())
	if err != nil {
		return nil, err
	}

	return newSession(conn, c2, c1, peerID, peerPub, peerName, log), nil
}

// nameFn 由 app 注入，握手时携带当前显示名。
var nameFn = func(id *identity.Identity) string { return "" }

// SetNameFunc 注入显示名提供者（config.DisplayName 可变）。
func SetNameFunc(fn func(id *identity.Identity) string) { nameFn = fn }

func currentName(id *identity.Identity) string { return nameFn(id) }
