package ui

import (
	"context"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/lei/firefly/internal/hub"
	"github.com/lei/firefly/internal/store"
)

// newChatEntry 创建多行输入框：Enter 换行、Shift+Enter 发送
// （Fyne MultiLineEntry 的原生提交语义），另配发送按钮。
func newChatEntry(onSend func(string)) *widget.Entry {
	e := widget.NewMultiLineEntry()
	e.Wrapping = fyne.TextWrapWord
	e.SetPlaceHolder("输入消息…（Shift+Enter 发送，Enter 换行）")
	e.OnSubmitted = func(s string) {
		if strings.TrimSpace(s) != "" {
			e.SetText("")
			onSend(s)
		}
	}
	return e
}

// chatView 是右侧聊天区：标题栏 + 消息滚动列表 + 输入区。
type chatView struct {
	c *controller

	title *widget.Label
	sub   *widget.Label

	loadMore *widget.Button
	rowsBox  *fyne.Container
	scroll   *container.Scroll

	entry *widget.Entry

	conv    hub.ConversationID
	rows    []*messageRow
	rowByID map[string]*messageRow
	hasMore bool

	root fyne.CanvasObject
}

func newChatView(c *controller) *chatView {
	cv := &chatView{
		c:       c,
		title:   widget.NewLabelWithStyle("局域网群聊", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		sub:     widget.NewLabel(""),
		rowsBox: container.NewVBox(),
		rowByID: make(map[string]*messageRow),
	}
	cv.scroll = container.NewVScroll(cv.rowsBox)

	cv.loadMore = widget.NewButton("加载更早的消息", cv.loadOlder)
	cv.loadMore.Hide()

	cv.entry = newChatEntry(cv.send)

	sendBtn := widget.NewButtonWithIcon("", theme.MailSendIcon(), func() {
		text := strings.TrimSpace(cv.entry.Text)
		if text != "" {
			cv.entry.SetText("")
			cv.send(text)
		}
	})
	sendBtn.Importance = widget.HighImportance
	attachBtn := widget.NewButtonWithIcon("", theme.FileIcon(), cv.attachFile)

	infoBtn := widget.NewButtonWithIcon("", theme.InfoIcon(), cv.showInfo)

	header := container.NewBorder(nil, nil, nil, infoBtn, container.NewVBox(cv.title, cv.sub))
	bottom := container.NewBorder(nil, nil, attachBtn, sendBtn, cv.entry)
	center := container.NewBorder(cv.loadMore, nil, nil, nil, cv.scroll)
	cv.root = container.NewBorder(header, bottom, nil, nil, center)
	return cv
}

func (cv *chatView) content() fyne.CanvasObject { return cv.root }

// setConversation 切换当前会话并加载最近一页历史。
func (cv *chatView) setConversation(conv hub.ConversationID) {
	cv.conv = conv
	cv.rows = nil
	cv.rowByID = make(map[string]*messageRow)
	cv.rowsBox.Objects = nil

	page := initialPageSize
	msgs, err := cv.c.app.Store.ListMessages(conv, 0, page)
	if err != nil {
		cv.c.log.Error("加载历史消息失败", "err", err)
	}
	cv.hasMore = len(msgs) >= page
	if cv.hasMore {
		cv.loadMore.Show()
	} else {
		cv.loadMore.Hide()
	}
	for _, m := range msgs {
		cv.appendRow(m)
	}
	cv.rowsBox.Refresh()
	cv.refreshHeader()
	cv.scroll.ScrollToBottom()
	cv.c.markReadAsync(conv)
}

// appendMessage 增量追加一条消息（来自事件总线，主线程调用）。
func (cv *chatView) appendMessage(m *store.Message) {
	if m.Conversation != cv.conv {
		return
	}
	if _, dup := cv.rowByID[m.ID]; dup {
		return
	}
	cv.appendRow(m)
	cv.rowsBox.Refresh()
	cv.scroll.ScrollToBottom()
	cv.c.markReadAsync(cv.conv)
}

func (cv *chatView) appendRow(m *store.Message) {
	sender := ""
	if cv.conv == hub.GroupLAN && m.Direction == store.DirectionInbound {
		sender = cv.c.peerName(m.PeerID)
	}
	row := newMessageRow(m, sender, cv.c.onMessageTap, cv.c.showMessageMenu)
	cv.rowsBox.Add(row)
	cv.rows = append(cv.rows, row)
	cv.rowByID[m.ID] = row
	cv.trimRows()
}

// trimRows 限制渲染上限（Fyne 长列表性能保护）。
func (cv *chatView) trimRows() {
	for len(cv.rows) > maxRendered {
		old := cv.rows[0]
		cv.rows = cv.rows[1:]
		delete(cv.rowByID, old.msg.ID)
		cv.rowsBox.Remove(old)
	}
}

// loadOlder 向上加载更早一页消息。
func (cv *chatView) loadOlder() {
	if len(cv.rows) == 0 {
		return
	}
	before := cv.rows[0].msg.SentAt
	msgs, err := cv.c.app.Store.ListMessages(cv.conv, before, morePageSize)
	if err != nil {
		cv.c.log.Error("加载历史消息失败", "err", err)
		return
	}
	if len(msgs) < morePageSize {
		cv.hasMore = false
		cv.loadMore.Hide()
	}
	if len(msgs) == 0 {
		return
	}
	pre := make([]fyne.CanvasObject, 0, len(msgs))
	preRows := make([]*messageRow, 0, len(msgs))
	for _, m := range msgs {
		sender := ""
		if cv.conv == hub.GroupLAN && m.Direction == store.DirectionInbound {
			sender = cv.c.peerName(m.PeerID)
		}
		row := newMessageRow(m, sender, cv.c.onMessageTap, cv.c.showMessageMenu)
		pre = append(pre, row)
		preRows = append(preRows, row)
		cv.rowByID[m.ID] = row
	}
	cv.rowsBox.Objects = append(pre, cv.rowsBox.Objects...)
	cv.rows = append(preRows, cv.rows...)
	cv.rowsBox.Refresh()
	cv.trimRows()
}

// updateReceipt 更新某条出站消息的状态显示。
func (cv *chatView) updateReceipt(msgID string, status store.MessageStatus) {
	row, ok := cv.rowByID[msgID]
	if !ok {
		return
	}
	row.msg.Status = status
	row.Refresh()
}

func (cv *chatView) refreshHeader() {
	peers := cv.c.peerSnapshot()
	cv.title.SetText(conversationTitle(cv.conv, peers))
	if cv.conv == hub.GroupLAN {
		n := 0
		for _, p := range peers {
			if p.Online {
				n++
			}
		}
		cv.sub.SetText("广播到局域网内所有在线设备 · 当前在线 " + itoa(n) + " 台")
		return
	}
	if pid, ok := hub.PeerFromConversation(cv.conv); ok {
		if p, ok2 := peers[pid.String()]; ok2 {
			state := "离线"
			if p.Online {
				state = "在线"
			}
			trust := trustLabel(p.Trust)
			cv.sub.SetText(state + " · " + p.Platform + " · 指纹 " + p.Fingerprint + " · " + trust)
			return
		}
	}
	cv.sub.SetText("")
}

func (cv *chatView) showInfo() {
	if cv.conv == hub.GroupLAN {
		cv.c.showSelfInfo()
		return
	}
	if pid, ok := hub.PeerFromConversation(cv.conv); ok {
		cv.c.showPeerInfo(pid)
	}
}

// send 异步发送文本；消息回显由 EventMessageSent 事件驱动（保证与落库一致）。
func (cv *chatView) send(text string) {
	text = strings.TrimSpace(text)
	if text == "" || cv.conv == "" {
		return
	}
	conv := cv.conv
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := cv.c.hub.SendChat(ctx, conv, text, ""); err != nil {
			cv.c.notifyError("发送失败", err)
		}
	}()
}

// attachFile 选择本地文件并向当前 DM 对端发起传输 offer。
func (cv *chatView) attachFile() {
	if cv.conv == hub.GroupLAN {
		cv.c.notify("提示", "文件传输目前仅支持一对一会话，请先在左侧选择一台设备。")
		return
	}
	pid, ok := hub.PeerFromConversation(cv.conv)
	if !ok {
		return
	}
	dlg := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
		if err != nil {
			cv.c.notifyError("打开文件失败", err)
			return
		}
		if rc == nil {
			return // 用户取消
		}
		path := rc.URI().Path()
		rc.Close()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			name := cv.c.peerName(pid.String())
			if _, err := cv.c.hub.Files().SendOffer(ctx, pid, name, cv.conv, path); err != nil {
				cv.c.notifyError("无法发起文件传输（对端可能离线）", err)
			}
		}()
	}, cv.c.win)
	dlg.Show()
}

func itoa(n int) string { return strconv.Itoa(n) }

func trustLabel(t store.TrustState) string {
	switch t {
	case store.TrustTrusted:
		return "已信任"
	case store.TrustBlocked:
		return "已拉黑"
	case store.TrustKeyChanged:
		return "密钥已变更（已拒收）"
	default:
		return "未验证"
	}
}
