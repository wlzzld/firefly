// Package hub 是核心编排层：路由消息、管理 peer 连接、TOFU 信任状态机、
// 群播扇出，并向 UI 发布事件。除 internal/ui 外的包不依赖 Fyne。
package hub

import (
	"sync"
	"time"

	"github.com/lei/firefly/internal/filetransfer"
	"github.com/lei/firefly/internal/identity"
	"github.com/lei/firefly/internal/store"
)

// ConversationID 形如 "dm:<peerID hex>" 或 "group:lan"。
type ConversationID = string

// GroupLAN 是局域网广播会话的固定 ID。
const GroupLAN ConversationID = "group:lan"

// DMConversation 构造一对一会话 ID。
func DMConversation(peer identity.PeerID) ConversationID {
	return "dm:" + peer.String()
}

// PeerFromConversation 从 DM 会话 ID 解析 PeerID。
func PeerFromConversation(conv ConversationID) (identity.PeerID, bool) {
	if len(conv) != 3+64 || conv[:3] != "dm:" {
		return identity.PeerID{}, false
	}
	id, err := identity.PeerIDFromHex(conv[3:])
	return id, err == nil
}

// EventKind 是 UI 事件类型。
type EventKind int

const (
	EventPeerUp           EventKind = iota // 新设备上线
	EventPeerDown                          // 设备离线
	EventPeerUpdated                       // 设备信息刷新（名称等）
	EventTrustChanged                      // 信任状态变化（含 TOFU 首见 → UI 弹配对框）
	EventMessageReceived                   // 收到聊天消息（已持久化）
	EventMessageSent                       // 本机消息已发出（已持久化）
	EventReceiptUpdated                    // 消息回执状态更新
	EventTransferState                     // 文件传输状态变化
	EventTransferProgress                  // 文件传输进度（UI latest-wins 合并）
	EventNotice                            // 系统提示（警告/信息）
)

// PeerInfo 是 UI 消费的设备视图（在线状态 + 持久化信任）。
type PeerInfo struct {
	ID          identity.PeerID
	Name        string
	Platform    string
	Fingerprint string
	Online      bool
	Trust       store.TrustState
	LastSeen    time.Time
}

// Event 是推送给 UI 的聚合事件。
type Event struct {
	Kind         EventKind
	Peer         *PeerInfo
	Conversation ConversationID
	Message      *store.Message
	ReceiptID    string // EventReceiptUpdated: 状态变化的消息 ID
	Receipt      store.MessageStatus
	Transfer     *filetransfer.State
	Progress     *filetransfer.Progress
	Notice       string
	Err          error
}

// bus 是简单多订阅者事件总线：尽力投递，订阅者通道满时丢弃该事件
// （UI 侧进度 latest-wins；256 深度缓冲兜底突发）。
// mu 保护 subs：publish/subscribe/退订 来自任意 goroutine。
type bus struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

func newBus() *bus {
	return &bus{subs: make(map[chan Event]struct{})}
}

// subscribe 返回事件流与同步退订函数；退订返回后保证不再收到事件。
func (b *bus) subscribe() (<-chan Event, func()) {
	c := make(chan Event, 256)
	b.mu.Lock()
	b.subs[c] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return c, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, c)
			b.mu.Unlock()
			close(c)
		})
	}
}

// publish 尽力投递给所有订阅者（通道满则跳过该订阅者的此事件，
// 由订阅者缓冲 256 深度兜底；UI 必须及时消费）。
func (b *bus) publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for c := range b.subs {
		select {
		case c <- ev:
		default:
		}
	}
}
