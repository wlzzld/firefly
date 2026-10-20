package ui

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/lei/firefly/internal/app"
	"github.com/lei/firefly/internal/filetransfer"
	"github.com/lei/firefly/internal/hub"
	"github.com/lei/firefly/internal/store"
)

const (
	initialPageSize = 100 // 打开会话时加载的最近消息数
	morePageSize    = 50  // 「加载更早」每页条数
	maxRendered     = 500 // 消息列表渲染上限（超出裁剪最旧）
)

// controller 是唯一的 hub Event 消费者：后台 goroutine 收事件，
// 全部 widget 更新包在 fyne.Do 中调度到主线程。
type controller struct {
	app *app.App
	hub *hub.Hub
	win fyne.Window
	log *slog.Logger

	mu       sync.Mutex
	current  hub.ConversationID
	peers    map[string]*hub.PeerInfo // PeerID hex → info
	unread   map[hub.ConversationID]int
	pairings map[string]bool // peer hex → 是否已弹配对框（TOFU，避免重复打扰）

	sidebar *sidebarView
	chat    *chatView
	xfer    *transferPanel
}

func newController(a *app.App, win fyne.Window) *controller {
	c := &controller{
		app:      a,
		hub:      a.Hub,
		win:      win,
		log:      a.Log,
		current:  hub.GroupLAN,
		peers:    make(map[string]*hub.PeerInfo),
		unread:   make(map[hub.ConversationID]int),
		pairings: make(map[string]bool),
	}
	for _, p := range a.Hub.Peers() {
		c.peers[p.ID.String()] = p
	}
	return c
}

// buildRoot 构建主窗口内容（主线程调用）。
func (c *controller) buildRoot() fyne.CanvasObject {
	c.sidebar = newSidebarView(c)
	c.chat = newChatView(c)
	c.xfer = newTransferPanel(c)

	right := container.NewHSplit(c.chat.content(), c.xfer.content())
	right.Offset = 1.0 // 传输面板默认收起
	c.xfer.split = right

	main := container.NewHSplit(c.sidebar.content(), right)
	main.Offset = 0.26

	toolbar := c.buildToolbar()
	c.sidebar.refresh()
	c.chat.setConversation(c.current)
	return container.NewBorder(toolbar, nil, nil, nil, main)
}

func (c *controller) buildToolbar() fyne.CanvasObject {
	return widget.NewToolbar(
		widget.NewToolbarAction(theme.SearchIcon(), c.showSearch),
		widget.NewToolbarAction(theme.FileIcon(), c.xfer.toggle),
		widget.NewToolbarAction(theme.SettingsIcon(), c.showSettings),
		widget.NewToolbarSpacer(),
		widget.NewToolbarAction(theme.AccountIcon(), c.showSelfInfo),
	)
}

// run 消费事件流直到 ctx 取消（后台 goroutine）。
func (c *controller) run(ctx context.Context) {
	events, unsub := c.hub.Subscribe()
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			c.handle(ev)
		}
	}
}

func (c *controller) handle(ev hub.Event) {
	switch ev.Kind {
	case hub.EventPeerUp, hub.EventPeerUpdated, hub.EventPeerDown:
		if ev.Peer == nil {
			return
		}
		c.mu.Lock()
		c.peers[ev.Peer.ID.String()] = ev.Peer
		c.mu.Unlock()
		fyne.Do(func() {
			c.sidebar.refresh()
			c.chat.refreshHeader()
		})

	case hub.EventTrustChanged:
		if ev.Peer == nil {
			return
		}
		c.mu.Lock()
		c.peers[ev.Peer.ID.String()] = ev.Peer
		shown := c.pairings[ev.Peer.ID.String()]
		if ev.Peer.Trust == store.TrustUnverified && !shown && ev.Peer.Online {
			c.pairings[ev.Peer.ID.String()] = true
		}
		c.mu.Unlock()
		fyne.Do(func() {
			c.sidebar.refresh()
			c.chat.refreshHeader()
			if ev.Peer.Trust == store.TrustUnverified && ev.Peer.Online && !shown {
				c.showPairing(ev.Peer)
			}
		})

	case hub.EventMessageReceived:
		if ev.Message == nil {
			return
		}
		conv := ev.Message.Conversation
		c.mu.Lock()
		cur := c.current
		if conv != cur {
			c.unread[conv]++
		}
		c.mu.Unlock()
		m := ev.Message
		fyne.Do(func() {
			c.chat.appendMessage(m)
			if conv != cur {
				c.sidebar.refresh()
				c.notifyMessage(m)
			}
		})

	case hub.EventMessageSent:
		if ev.Message == nil {
			return
		}
		m := ev.Message
		fyne.Do(func() { c.chat.appendMessage(m) })

	case hub.EventReceiptUpdated:
		id, status := ev.ReceiptID, ev.Receipt
		fyne.Do(func() { c.chat.updateReceipt(id, status) })

	case hub.EventTransferState:
		if ev.Transfer == nil {
			return
		}
		st := ev.Transfer
		fyne.Do(func() {
			c.xfer.upsertState(st)
			if st.Phase == store.FileDone {
				c.notifyTransferDone(st)
			}
		})

	case hub.EventTransferProgress:
		if ev.Progress == nil {
			return
		}
		p := ev.Progress
		fyne.Do(func() { c.xfer.updateProgress(p) })

	case hub.EventNotice:
		msg := ev.Notice
		fyne.Do(func() { c.notify("Firefly", msg) })
	}
}

// ---------------------------------------------------------------------------
// 状态访问（线程安全）
// ---------------------------------------------------------------------------

func (c *controller) peerSnapshot() map[string]*hub.PeerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]*hub.PeerInfo, len(c.peers))
	for k, v := range c.peers {
		cp := *v
		out[k] = &cp
	}
	return out
}

func (c *controller) peerName(peerHex string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.peers[peerHex]; ok && p.Name != "" {
		return p.Name
	}
	if len(peerHex) >= 8 {
		return peerHex[:8]
	}
	return peerHex
}

func (c *controller) currentConv() hub.ConversationID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *controller) unreadCount(conv hub.ConversationID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unread[conv]
}

// openConversation 切换当前会话并清未读（主线程调用）。
func (c *controller) openConversation(conv hub.ConversationID) {
	c.mu.Lock()
	c.current = conv
	delete(c.unread, conv)
	c.mu.Unlock()
	c.chat.setConversation(conv)
	c.sidebar.refresh()
}

// markReadAsync 异步将当前会话入站消息标记已读（发 READ 回执）。
func (c *controller) markReadAsync(conv hub.ConversationID) {
	if conv == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c.hub.MarkRead(ctx, conv)
	}()
}

// onMessageTap 点击消息：文件消息提示位置，文本消息无操作。
func (c *controller) onMessageTap(m *store.Message) {
	if m == nil || m.Kind != store.KindFile || m.FileID == nil {
		return
	}
	rec, err := c.app.Store.GetFile(*m.FileID)
	if err != nil || rec == nil {
		return
	}
	path := "(未记录)"
	if rec.LocalPath != nil {
		path = *rec.LocalPath
	}
	if rec.State == store.FileDone && rec.Direction == store.DirectionInbound {
		path = c.app.Cfg.DownloadDir
	}
	fyne.Do(func() {
		dialog.ShowInformation("文件", m.Text+"\n位置："+path, c.win)
	})
}

// showMessageMenu 消息右键菜单：整条复制进剪贴板，或弹窗部分选中复制。
func (c *controller) showMessageMenu(m *store.Message, pos fyne.Position) {
	if m == nil {
		return
	}
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("复制消息内容", func() {
			fyne.CurrentApp().Clipboard().SetContent(m.Text)
		}),
		fyne.NewMenuItem("选中复制…", func() { c.showCopyDialog(m) }),
	)
	widget.NewPopUpMenu(menu, c.win.Canvas()).ShowAtPosition(pos)
}

// showCopyDialog 用多行 Entry 展示消息全文：可拖拽选中任意片段后 Cmd+C 复制
// （聊天气泡内的 Label 不支持选中，这里补足该能力）。
func (c *controller) showCopyDialog(m *store.Message) {
	// Fyne 2.8 的 Entry 无只读态（Disable 会把文本置灰影响阅读）；
	// 弹窗内是临时副本，误编辑不影响已存消息，故保持可交互的正常态
	e := widget.NewMultiLineEntry()
	e.Wrapping = fyne.TextWrapWord
	e.SetText(m.Text)
	body := container.NewVBox(
		widget.NewLabel("拖拽选中文本，Cmd+C（Windows 为 Ctrl+C）复制："),
		container.NewGridWrap(fyne.NewSize(460, 200), e),
	)
	dialog.NewCustom("选中复制", "关闭", body, c.win).Show()
	c.win.Canvas().Focus(e)
}

// notify 弹系统通知（尽力而为）。
func (c *controller) notify(title, body string) {
	fyne.CurrentApp().SendNotification(fyne.NewNotification(title, body))
}

func (c *controller) notifyError(context string, err error) {
	c.log.Warn(context, "err", err)
	fyne.Do(func() {
		dialog.ShowError(err, c.win)
	})
}

func (c *controller) notifyMessage(m *store.Message) {
	from := c.peerName(m.PeerID)
	if m.Conversation == hub.GroupLAN {
		from += " @ 局域网群聊"
	}
	body := m.Text
	if len(body) > 60 {
		body = body[:60] + "…"
	}
	c.notify(from, body)
}

func (c *controller) notifyTransferDone(st *filetransfer.State) {
	dir := "发送完成"
	if st.Direction == store.DirectionInbound {
		dir = "接收完成，已存到下载目录"
	}
	c.notify("文件 "+st.Name, dir)
}

// selfPeer 返回本机信息视图。
func (c *controller) selfPeer() *hub.PeerInfo {
	self := c.app.Discovery.Self()
	return &hub.PeerInfo{
		ID:          self.ID,
		Name:        self.Name,
		Platform:    self.Platform,
		Fingerprint: self.Fingerprint,
		Online:      true,
		Trust:       store.TrustTrusted,
	}
}
