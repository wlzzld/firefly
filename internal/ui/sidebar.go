package ui

import (
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
	list   *widget.List

	items []*hub.PeerInfo
	root  fyne.CanvasObject
}

func newSidebarView(c *controller) *sidebarView {
	sb := &sidebarView{c: c}
	sb.lanBtn = widget.NewButton("💬 局域网群聊", func() {
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

	devLabel := widget.NewLabelWithStyle("设备", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	bottom := container.NewBorder(devLabel, nil, nil, nil, sb.list)
	sb.root = container.NewBorder(sb.lanBtn, nil, nil, nil, bottom)
	return sb
}

func (sb *sidebarView) content() fyne.CanvasObject { return sb.root }

func (sb *sidebarView) createItem() fyne.CanvasObject {
	// 占位容器；updateItem 每次重建内部内容（设备数量有限，代价可接受）
	return container.NewStack()
}

func (sb *sidebarView) updateItem(id widget.ListItemID, obj fyne.CanvasObject) {
	if id < 0 || id >= len(sb.items) {
		return
	}
	p := sb.items[id]
	box := obj.(*fyne.Container)
	dotColor := theme.DisabledColor()
	if p.Online {
		dotColor = theme.SuccessColor()
	}
	if p.Trust == store.TrustBlocked || p.Trust == store.TrustKeyChanged {
		dotColor = theme.ErrorColor()
	}
	dot := container.NewGridWrap(fyne.NewSize(14, 14), canvas.NewCircle(dotColor))

	name := widget.NewLabelWithStyle(p.Name, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	name.Truncation = fyne.TextTruncateEllipsis
	subText := platformLabel(p.Platform) + " · " + shortFP(p.Fingerprint)
	switch p.Trust {
	case store.TrustBlocked:
		subText += " · 已拉黑"
	case store.TrustKeyChanged:
		subText += " · ⚠ 密钥变更"
	case store.TrustTrusted:
		subText += " · ✓"
	}
	sub := widget.NewLabel(subText)
	sub.TextStyle = fyne.TextStyle{Italic: true}
	sub.Truncation = fyne.TextTruncateEllipsis

	badge := widget.NewLabel("")
	badge.Alignment = fyne.TextAlignTrailing
	if n := sb.c.unreadCount(hub.DMConversation(p.ID)); n > 0 {
		badge.SetText(itoa(n))
		badge.Importance = widget.HighImportance
		badge.TextStyle = fyne.TextStyle{Bold: true}
	}

	texts := container.NewVBox(name, sub)
	box.Objects = []fyne.CanvasObject{container.NewBorder(nil, nil, dot, badge, texts)}
	box.Refresh()
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
