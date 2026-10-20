package ui

import (
	"fmt"
	"sort"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/lei/firefly/internal/hub"
	"github.com/lei/firefly/internal/store"
)

// sidebarView 是左侧栏：固定「局域网群聊」入口 + 已知设备列表
// （在线优先，含状态点、平台、未读徽标；点击进入 DM）。
type sidebarView struct {
	c *controller

	lanBtn *widget.Button
	header *widget.Label
	list   *widget.List

	items []*hub.PeerInfo
	root  fyne.CanvasObject
}

func newSidebarView(c *controller) *sidebarView {
	sb := &sidebarView{c: c}
	sb.lanBtn = widget.NewButtonWithIcon("局域网群聊", theme.MailReplyAllIcon(), func() {
		c.openConversation(hub.GroupLAN)
	})
	sb.lanBtn.Alignment = widget.ButtonAlignLeading

	sb.list = widget.NewList(
		func() int { return len(sb.items) },
		sb.createItem,
		sb.updateItem,
	)
	sb.list.OnSelected = func(id widget.ListItemID) {
		if id < 0 || id >= len(sb.items) {
			return
		}
		c.openConversation(hub.DMConversation(sb.items[id].ID))
	}

	sb.header = widget.NewLabelWithStyle("设备", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	headerBox := container.NewPadded(sb.header)
	bottom := container.NewBorder(headerBox, nil, nil, nil, sb.list)
	sb.root = container.NewBorder(sb.lanBtn, nil, nil, nil, bottom)
	return sb
}

func (sb *sidebarView) content() fyne.CanvasObject { return sb.root }

// peerRow 是侧栏单个设备行：状态点 + 设备名 + 平台/指纹副行 + 未读徽标。
// 作为 widget.List 的模板对象，其 MinSize 决定行高——必须是真实结构，
// 空占位容器会让所有行高为 0（行叠画、无法点击）。
type peerRow struct {
	widget.BaseWidget

	dot   *canvas.Circle
	name  *widget.Label
	sub   *widget.Label
	badge *widget.Label
}

func newPeerRow() *peerRow {
	r := &peerRow{
		dot:   canvas.NewCircle(theme.DisabledColor()),
		name:  widget.NewLabelWithStyle("设备名称占位", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		sub:   widget.NewLabel("平台 · 指纹占位"),
		badge: widget.NewLabel(""),
	}
	r.name.Truncation = fyne.TextTruncateEllipsis
	r.sub.TextStyle = fyne.TextStyle{Italic: true}
	r.sub.Truncation = fyne.TextTruncateEllipsis
	r.badge.Alignment = fyne.TextAlignTrailing
	r.badge.TextStyle = fyne.TextStyle{Bold: true}
	r.badge.Importance = widget.HighImportance
	r.badge.Hide()
	r.ExtendBaseWidget(r)
	return r
}

func (r *peerRow) CreateRenderer() fyne.WidgetRenderer {
	// Circle 无固有 MinSize，直接套 Center 会被压成 1×1；
	// 先用 GridWrap 强制 10×10，再套 Center 在 Border 左侧全高列里垂直居中
	dotBox := container.NewCenter(container.NewGridWrap(fyne.NewSize(10, 10), r.dot))
	texts := container.NewVBox(r.name, r.sub)
	content := container.NewBorder(nil, nil, dotBox, r.badge, texts)
	return widget.NewSimpleRenderer(container.NewPadded(content))
}

func (sb *sidebarView) createItem() fyne.CanvasObject {
	return newPeerRow()
}

func (sb *sidebarView) updateItem(id widget.ListItemID, obj fyne.CanvasObject) {
	if id < 0 || id >= len(sb.items) {
		return
	}
	row, ok := obj.(*peerRow)
	if !ok {
		return
	}
	p := sb.items[id]

	dotColor := theme.DisabledColor()
	if p.Online {
		dotColor = theme.SuccessColor()
	}
	if p.Trust == store.TrustBlocked || p.Trust == store.TrustKeyChanged {
		dotColor = theme.ErrorColor()
	}
	row.dot.FillColor = dotColor

	row.name.SetText(p.Name)
	subText := platformLabel(p.Platform) + " · " + shortFP(p.Fingerprint)
	switch p.Trust {
	case store.TrustBlocked:
		subText += " · 已拉黑"
	case store.TrustKeyChanged:
		subText += " · ⚠ 密钥变更"
	case store.TrustTrusted:
		subText += " · ✓ 已信任"
	case store.TrustUnverified:
		subText += " · 未验证"
	}
	row.sub.SetText(subText)

	if n := sb.c.unreadCount(hub.DMConversation(p.ID)); n > 0 {
		row.badge.SetText(" " + itoa(n) + " ")
		row.badge.Show()
	} else {
		row.badge.Hide()
	}
	row.Refresh()
}

// refresh 重新拉取设备快照并刷新列表（主线程调用）。
func (sb *sidebarView) refresh() {
	peers := sb.c.peerSnapshot()
	items := make([]*hub.PeerInfo, 0, len(peers))
	for _, p := range peers {
		items = append(items, p)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Online != items[j].Online {
			return items[i].Online
		}
		return items[i].Name < items[j].Name
	})
	sb.items = items
	online := 0
	for _, p := range items {
		if p.Online {
			online++
		}
	}
	sb.header.SetText(fmt.Sprintf("设备 · 在线 %d/%d", online, len(items)))
	sb.list.Refresh()
	sb.selectCurrent()
}

// selectCurrent 高亮当前会话对应的设备。
func (sb *sidebarView) selectCurrent() {
	conv := sb.c.currentConv()
	for i, p := range sb.items {
		if hub.DMConversation(p.ID) == conv {
			sb.list.Select(i)
			return
		}
	}
	sb.list.UnselectAll()
}

func platformLabel(p string) string {
	switch p {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "":
		return "未知平台"
	default:
		return p
	}
}

func shortFP(fp string) string {
	if len(fp) > 9 {
		return fp[:9]
	}
	return fp
}
