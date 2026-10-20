package transport

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lei/firefly/internal/identity"
	fireflyv1 "github.com/lei/firefly/internal/protocol/pb/firefly/v1"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.LoadOrCreate(t.TempDir())
	require.NoError(t, err)
	return id
}

func chatEnv(text string) *fireflyv1.Envelope {
	return &fireflyv1.Envelope{
		MessageId:    "m-" + text,
		SentAtUnixMs: time.Now().UnixMilli(),
		Payload:      &fireflyv1.Envelope_Chat{Chat: &fireflyv1.Chat{ConversationId: "group:lan", Text: text}},
	}
}

// TestHandshakeRoundtrip 验证 net.Pipe 上完整 XX 握手：
// 双向身份认证正确、消息双向可达。
func TestHandshakeRoundtrip(t *testing.T) {
	idA := testIdentity(t)
	idB := testIdentity(t)

	client, server := net.Pipe()
	type result struct {
		s   *session
		err error
	}
	respCh := make(chan result, 1)
	go func() {
		s, err := handshakeResponder(server, idB, testLog())
		respCh <- result{s, err}
	}()

	initS, initErr := handshakeInitiator(client, idA, testLog())
	require.NoError(t, initErr)
	res := <-respCh
	require.NoError(t, res.err)
	respS := res.s
	defer initS.Close()
	defer respS.Close()

	// 双向身份：各自握得对端经签名验证的 PeerID
	assert.Equal(t, idB.PeerID, initS.Peer())
	assert.Equal(t, idA.PeerID, respS.Peer())
	assert.Equal(t, idB.Pub, initS.RemotePub())
	assert.Equal(t, idA.Pub, respS.RemotePub())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// initiator → responder
	require.NoError(t, initS.Send(ctx, chatEnv("hello")))
	select {
	case got := <-respS.Recv():
		require.NotNil(t, got)
		assert.Equal(t, "hello", got.GetChat().GetText())
		assert.Equal(t, uint32(1), got.ProtoVersion)
	case <-time.After(5 * time.Second):
		t.Fatal("responder 接收超时")
	}

	// responder → initiator（验证 c1/c2 方向分配正确）
	require.NoError(t, respS.Send(ctx, chatEnv("world")))
	select {
	case got := <-initS.Recv():
		require.NotNil(t, got)
		assert.Equal(t, "world", got.GetChat().GetText())
	case <-time.After(5 * time.Second):
		t.Fatal("initiator 接收超时")
	}
}

// TestVerifyPayload 验证签名绑定：正确转录通过，错误转录/静态密钥被拒绝。
func TestVerifyPayload(t *testing.T) {
	id := testIdentity(t)
	transcriptHash := []byte("transcript-checkpoint-32bytes!!!")

	data, err := buildPayload(id, transcriptHash, "测试设备")
	require.NoError(t, err)

	pid, pub, name, err := verifyPayload(data, transcriptHash, id.Static.PublicKey().Bytes())
	require.NoError(t, err)
	assert.Equal(t, id.PeerID, pid)
	assert.Equal(t, id.Pub, pub)
	assert.Equal(t, "测试设备", name)

	// 转录被篡改（中间人改变了握手上下文）→ 签名验证失败
	_, _, _, err = verifyPayload(data, []byte("tampered-transcript-32bytes!!!!!"), id.Static.PublicKey().Bytes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "签名验证失败")

	// 静态密钥不匹配（冒名顶替者提供别的 X25519 公钥）→ 失败
	other := testIdentity(t)
	_, _, _, err = verifyPayload(data, transcriptHash, other.Static.PublicKey().Bytes())
	require.Error(t, err)
}

// TestVerifyPayloadGarbage 验证非法 payload 不 panic。
func TestVerifyPayloadGarbage(t *testing.T) {
	_, _, _, err := verifyPayload([]byte{0xff, 0xfe, 0xfd}, make([]byte, 32), make([]byte, 32))
	require.Error(t, err)
}

// TestShouldDial 验证方向规则：任意两个不同 ID 恰有一方拨号；自己对自己不拨号。
func TestShouldDial(t *testing.T) {
	a := testIdentity(t).PeerID
	b := testIdentity(t).PeerID
	require.NotEqual(t, a, b)

	assert.NotEqual(t, ShouldDial(a, b), ShouldDial(b, a), "方向规则必须互补")
	assert.False(t, ShouldDial(a, a), "相同 ID 不拨号")
}
