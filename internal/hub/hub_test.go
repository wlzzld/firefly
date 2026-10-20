package hub

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lei/firefly/internal/identity"
)

func TestConversationHelpers(t *testing.T) {
	var pid identity.PeerID
	for i := range pid {
		pid[i] = byte(i)
	}

	conv := DMConversation(pid)
	assert.Equal(t, "dm:"+pid.String(), conv)

	got, ok := PeerFromConversation(conv)
	require.True(t, ok)
	assert.Equal(t, pid, got)

	// 非法输入
	_, ok = PeerFromConversation(GroupLAN)
	assert.False(t, ok)
	_, ok = PeerFromConversation("dm:tooshort")
	assert.False(t, ok)
	_, ok = PeerFromConversation("xx:" + pid.String())
	assert.False(t, ok)
	_, ok = PeerFromConversation("dm:" + string(make([]byte, 64)))
	assert.False(t, ok, "非 hex 字符必须拒绝")
}

func TestReceiptTrackerDM(t *testing.T) {
	rt := newReceiptTracker()
	rt.register("m1", []string{"peerA"})

	tracked, allDone := rt.ack("m1", "peerA")
	assert.True(t, tracked)
	assert.True(t, allDone, "DM 单目标回执即完成")

	// 完成后重复回执：不再追踪
	tracked, allDone = rt.ack("m1", "peerA")
	assert.False(t, tracked)
	assert.False(t, allDone)

	// 未注册的消息
	tracked, _ = rt.ack("unknown", "peerA")
	assert.False(t, tracked)
}

func TestReceiptTrackerGroupFanout(t *testing.T) {
	rt := newReceiptTracker()
	rt.register("m2", []string{"p1", "p2", "p3"})

	tracked, allDone := rt.ack("m2", "p1")
	assert.True(t, tracked)
	assert.False(t, allDone, "1/3 送达不应标记完成")

	tracked, allDone = rt.ack("m2", "p2")
	assert.True(t, tracked)
	assert.False(t, allDone)

	tracked, allDone = rt.ack("m2", "p3")
	assert.True(t, tracked)
	assert.True(t, allDone, "3/3 全部送达")
}

func TestReceiptTrackerForgetPeer(t *testing.T) {
	rt := newReceiptTracker()
	rt.register("m3", []string{"p1", "p2"})

	// p1 发送失败被移除，仍期待 p2
	empty := rt.forgetPeer("m3", "p1")
	assert.False(t, empty)

	tracked, allDone := rt.ack("m3", "p2")
	assert.True(t, tracked)
	assert.True(t, allDone)

	// 未注册消息的 forgetPeer 直接报告已空
	assert.True(t, rt.forgetPeer("nope", "p1"))
}

func TestReceiptTrackerRegisterEmpty(t *testing.T) {
	rt := newReceiptTracker()
	rt.register("m4", nil)
	tracked, _ := rt.ack("m4", "p1")
	assert.False(t, tracked, "空目标集合不应建立追踪")
}

func TestBusSubscribePublishUnsubscribe(t *testing.T) {
	b := newBus()

	ch, unsub := b.subscribe()
	b.publish(Event{Kind: EventNotice, Notice: "hello"})
	select {
	case ev := <-ch:
		assert.Equal(t, "hello", ev.Notice)
	case <-time.After(time.Second):
		t.Fatal("订阅者未收到事件")
	}

	unsub()
	b.publish(Event{Kind: EventNotice, Notice: "after"})
	// 退订即关闭通道：publish 不会再投递，读取立即返回 ok=false
	ev, ok := <-ch
	assert.False(t, ok, "退订后通道应已关闭")
	assert.Empty(t, ev.Notice)
}

// TestBusConcurrentPublishSubscribe 用 -race 验证订阅/发布并发安全。
func TestBusConcurrentPublishSubscribe(t *testing.T) {
	b := newBus()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				b.publish(Event{Kind: EventNotice})
			}
		}()
		go func() {
			defer wg.Done()
			_, unsub := b.subscribe()
			time.Sleep(time.Millisecond)
			unsub()
		}()
	}
	wg.Wait()
}

func TestHumanSize(t *testing.T) {
	assert.Equal(t, "512 B", humanSize(512))
	assert.Equal(t, "1.0 KB", humanSize(1024))
	assert.Equal(t, "1.5 KB", humanSize(1536))
	assert.Equal(t, "2.0 MB", humanSize(2*1024*1024))
	assert.Equal(t, "3.0 GB", humanSize(3*1024*1024*1024))
}
