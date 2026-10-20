package testkit_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lei/firefly/internal/filetransfer"
	"github.com/lei/firefly/internal/hub"
	"github.com/lei/firefly/internal/store"
	"github.com/lei/firefly/internal/testkit"
)

// TestTwoNodeFullStack 在同进程内启动两个完整节点，全栈走通：
// 发现 → Noise XX 握手 → 一对一私聊 → 回执 → 群组广播 → 文件传输，
// 并断言 wire 上录制的 TCP 字节流为密文（无任何明文泄露）。
func TestTwoNodeFullStack(t *testing.T) {
	if runtime.GOOS == "windows" {
		// 双节点同进程共享 UDP 广播端口依赖 SO_REUSEPORT；
		// Windows 无此语义（SO_REUSEADDR 有端口劫持风险），跳过。
		t.Skip("Windows 无 SO_REUSEPORT，双节点集成测试仅覆盖 darwin/linux")
	}
	rec := &testkit.Recorder{}
	testkit.CaptureConns(t, rec) // 必须先于 NewNode

	udpPort := testkit.FreeUDPPort(t)
	a := testkit.NewNode(t, "节点A", udpPort)
	b := testkit.NewNode(t, "节点B", udpPort)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// ---- 1. 发现 + 握手：双向消息会话建立 ----
	testkit.WaitSession(t, a, b.ID.PeerID)
	testkit.WaitSession(t, b, a.ID.PeerID)

	aEvents, aUnsub := a.Hub.Subscribe()
	defer aUnsub()
	bEvents, bUnsub := b.Hub.Subscribe()
	defer bUnsub()

	// ---- 2. 一对一私聊 ----
	dmConv := hub.DMConversation(b.ID.PeerID)
	dmText := "集成测试私密消息-" + uuid.NewString()
	msgID, err := a.Hub.SendChat(ctx, dmConv, dmText, "")
	require.NoError(t, err)

	ev := testkit.WaitEvent(t, bEvents, 15*time.Second, "B 收到私聊消息",
		func(ev hub.Event) bool {
			return ev.Kind == hub.EventMessageReceived &&
				ev.Message != nil && ev.Message.Text == dmText
		})
	// B 侧会话按发送者归类（不信任对端声称的 conversation_id）
	bConv := hub.DMConversation(a.ID.PeerID)
	assert.Equal(t, bConv, ev.Conversation)
	assert.Equal(t, msgID, ev.Message.ID)
	assert.Equal(t, store.DirectionInbound, ev.Message.Direction)

	// A 收到 DELIVERED 回执
	testkit.WaitEvent(t, aEvents, 15*time.Second, "A 收到 DELIVERED 回执",
		func(ev hub.Event) bool {
			return ev.Kind == hub.EventReceiptUpdated &&
				ev.ReceiptID == msgID && ev.Receipt == store.StatusDelivered
		})

	// B 侧历史已持久化
	testkit.WaitFor(t, 5*time.Second, "B 侧消息落库", func() bool {
		msgs, err := b.Store.ListMessages(bConv, 0, 50)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.ID == msgID && m.Text == dmText {
				return true
			}
		}
		return false
	})

	// ---- 3. 群组广播 ----
	groupText := "集成测试群播-" + uuid.NewString()
	groupMsgID, err := a.Hub.SendChat(ctx, hub.GroupLAN, groupText, "")
	require.NoError(t, err)

	testkit.WaitEvent(t, bEvents, 15*time.Second, "B 收到群播",
		func(ev hub.Event) bool {
			return ev.Kind == hub.EventMessageReceived &&
				ev.Message != nil && ev.Message.Text == groupText &&
				ev.Conversation == hub.GroupLAN
		})
	// 唯一在线目标回执后 → 全部送达
	testkit.WaitEvent(t, aEvents, 15*time.Second, "A 群播全部送达",
		func(ev hub.Event) bool {
			return ev.Kind == hub.EventReceiptUpdated &&
				ev.ReceiptID == groupMsgID && ev.Receipt == store.StatusDelivered
		})

	// ---- 4. 文件传输（256KiB，含明文标记）----
	marker := "FIREFLY-PLAINTEXT-MARKER-" + uuid.NewString()
	content := make([]byte, 256*1024)
	_, err = rand.Read(content)
	require.NoError(t, err)
	copy(content, marker)
	srcPath := filepath.Join(a.Dir, "send.bin")
	require.NoError(t, os.WriteFile(srcPath, content, 0o600))

	transferID, err := a.Hub.Files().SendOffer(ctx, b.ID.PeerID, "节点B", dmConv, srcPath)
	require.NoError(t, err)

	// B 收到入站 offer → 接受
	offerEv := testkit.WaitEvent(t, bEvents, 15*time.Second, "B 收到文件 offer",
		func(ev hub.Event) bool {
			return ev.Kind == hub.EventTransferState && ev.Transfer != nil &&
				ev.Transfer.TransferID == transferID &&
				ev.Transfer.Phase == filetransfer.PhaseOffered &&
				ev.Transfer.Direction == filetransfer.Inbound
		})
	assert.Equal(t, int64(len(content)), offerEv.Transfer.Size)
	require.NoError(t, b.Hub.Files().Accept(ctx, transferID))

	// 双方等到 Done
	testkit.WaitEvent(t, bEvents, 30*time.Second, "B 接收完成",
		func(ev hub.Event) bool {
			return ev.Kind == hub.EventTransferState && ev.Transfer != nil &&
				ev.Transfer.TransferID == transferID &&
				ev.Transfer.Phase == filetransfer.PhaseDone
		})
	testkit.WaitEvent(t, aEvents, 30*time.Second, "A 发送完成",
		func(ev hub.Event) bool {
			return ev.Kind == hub.EventTransferState && ev.Transfer != nil &&
				ev.Transfer.TransferID == transferID &&
				ev.Transfer.Phase == filetransfer.PhaseDone
		})

	// B 下载目录内容逐字节一致
	gotPath := filepath.Join(b.Downloads, "send.bin")
	testkit.WaitFor(t, 5*time.Second, "B 下载文件就位", func() bool {
		_, err := os.Stat(gotPath)
		return err == nil
	})
	got, err := os.ReadFile(gotPath)
	require.NoError(t, err)
	require.Equal(t, len(content), len(got))
	assert.Equal(t, sha256.Sum256(content), sha256.Sum256(got), "文件 SHA-256 必须一致")

	// ---- 5. TOFU：双方 peers 表互有记录 ----
	pa, err := a.Store.GetPeer(b.ID.PeerID.String())
	require.NoError(t, err)
	require.NotNil(t, pa)
	assert.Equal(t, store.TrustUnverified, pa.Trust)
	assert.Equal(t, "节点B", pa.Name)

	pb, err := b.Store.GetPeer(a.ID.PeerID.String())
	require.NoError(t, err)
	require.NotNil(t, pb)
	assert.Equal(t, "节点A", pb.Name)

	// ---- 6. 密文断言：wire 录制中不得出现任何明文 ----
	wire := rec.Bytes()
	require.NotEmpty(t, wire, "应录到 TCP 字节流")
	t.Logf("wire 录制共 %d 字节", len(wire))

	for _, secret := range []string{dmText, groupText, marker, "firefly/1"} {
		assert.False(t, bytes.Contains(wire, []byte(secret)),
			"wire 字节流泄露明文: %.40s", secret)
	}
	// 文件名也会出现在 offer 控制消息里（已加密），同样不得明文出现
	assert.False(t, bytes.Contains(wire, []byte("send.bin")), "文件名明文泄露")
}
