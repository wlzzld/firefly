package ui

import (
	"image/color"
	"math"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/lei/firefly/internal/hub"
	"github.com/lei/firefly/internal/store"
)

// messageRow 是单条消息的自绘行 widget：
// 出站长气泡右对齐、入站左对齐，行高按文本折行精确计算
// （避免 Fyne 自动布局在 wrapping 下的高度震荡/裁剪）。
type messageRow struct {
	widget.BaseWidget

	msg      *store.Message
	outbound bool
	// 群聊中入站消息显示的发送者名
	senderName string

	onTap func(*store.Message)
}

func newMessageRow(m *store.Message, senderName string, onTap func(*store.Message)) *messageRow {
	r := &messageRow{
		msg:        m,
		outbound:   m.Direction == store.DirectionOutbound,
		senderName: senderName,
		onTap:      onTap,
	}
	r.ExtendBaseWidget(r)
	return r
}

// SetMessage 更新底层消息（状态回执变化后调用 Refresh）。
func (r *messageRow) SetMessage(m *store.Message) { r.msg = m }

func (r *messageRow) Tapped(*fyne.PointEvent) {
	if r.onTap != nil {
		r.onTap(r.msg)
	}
}

func (r *messageRow) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(theme.PrimaryColor())
	bg.CornerRadius = bubbleRadius

	label := widget.NewLabelWithStyle(r.msg.Text, fyne.TextAlignLeading, fyne.TextStyle{})
	label.Wrapping = fyne.TextWrapWord

	meta := canvas.NewText(r.metaText(), theme.ForegroundColor())
	meta.TextSize = metaTextSize()
	meta.Alignment = fyne.TextAlignTrailing

	objs := []fyne.CanvasObject{bg, label, meta}
	var sender *canvas.Text
	if r.senderName != "" {
		sender = canvas.NewText(r.senderName, theme.ForegroundColor())
		sender.TextSize = metaTextSize()
		sender.TextStyle = fyne.TextStyle{Bold: true}
		objs = append(objs, sender)
	}
	return &rowRenderer{row: r, bg: bg, label: label, meta: meta, sender: sender, objects: objs}
}

func (r *messageRow) metaText() string {
	ts := time.UnixMilli(r.msg.SentAt).Format("15:04")
	if r.msg.Kind == store.KindFile {
		return "📎 " + ts
	}
	if !r.outbound {
		return ts
	}
	switch r.msg.Status {
	case store.StatusSending:
		return ts + " · 发送中…"
	case store.StatusSent:
		return ts + " · ✓"
	case store.StatusDelivered:
		return ts + " · ✓✓"
	case store.StatusRead:
		return ts + " · ✓✓ 已读"
	case store.StatusFailed:
		return ts + " · ✗ 发送失败"
	}
	return ts
}

func metaTextSize() float32 {
	return theme.Size(theme.SizeNameCaptionText)
}

func bubbleTextSize() float32 {
	return theme.Size(theme.SizeNameText)
}

const (
	bubbleRadius   = 10.0
	bubbleMaxRatio = 0.72 // 气泡最大宽度占行宽比例
	bubbleMinWidth = 60.0
)

// bubbleSize 计算文本在 maxWidth 行宽内折行后的气泡尺寸。
func (r *messageRow) bubbleSize(rowWidth float32) (w, h float32) {
	pad := theme.Padding()
	maxTextW := rowWidth*bubbleMaxRatio - 2*pad
	if maxTextW < bubbleMinWidth {
		maxTextW = bubbleMinWidth
	}
	size := bubbleTextSize()
	lineH := fyne.MeasureText("汉", size, fyne.TextStyle{}).Height

	lines := 0
	maxW := float32(0)
	for _, seg := range strings.Split(r.msg.Text, "\n") {
		segW := fyne.MeasureText(seg, size, fyne.TextStyle{}).Width
		if segW > maxTextW {
			lines += int(math.Ceil(float64(segW*1.02) / float64(maxTextW)))
			if maxW < maxTextW {
				maxW = maxTextW
			}
		} else {
			lines++
			if segW > maxW {
				maxW = segW
			}
		}
	}
	if lines == 0 {
		lines = 1
	}
	return maxW + 2*pad, float32(lines)*lineH + pad
}

// rowHeight 行总高度 = sender(可选) + 气泡 + meta + 间距。
func (r *messageRow) rowHeight(rowWidth float32) float32 {
	pad := theme.Padding()
	_, bh := r.bubbleSize(rowWidth)
	h := bh + metaTextSize()*1.4 + pad
	if r.senderName != "" {
		h += metaTextSize() * 1.4
	}
	return h
}

type rowRenderer struct {
	row     *messageRow
	bg      *canvas.Rectangle
	label   *widget.Label
	meta    *canvas.Text
	sender  *canvas.Text
	objects []fyne.CanvasObject

	lastWidth float32
}

func (rr *rowRenderer) Layout(size fyne.Size) {
	pad := theme.Padding()
	rr.lastWidth = size.Width

	bw, bh := rr.row.bubbleSize(size.Width)
	x := float32(0)
	if rr.row.outbound {
		x = size.Width - bw
	}

	top := pad / 2
	if rr.sender != nil {
		sh := metaTextSize() * 1.4
		rr.sender.TextSize = metaTextSize()
		rr.sender.Resize(fyne.NewSize(bw, sh))
		rr.sender.Move(fyne.NewPos(x, top))
		top += sh
	}

	rr.bg.Move(fyne.NewPos(x, top))
	rr.bg.Resize(fyne.NewSize(bw, bh))

	rr.label.Resize(fyne.NewSize(bw-2*pad, bh-pad))
	rr.label.Move(fyne.NewPos(x+pad, top))

	mh := metaTextSize() * 1.4
	rr.meta.TextSize = metaTextSize()
	rr.meta.Resize(fyne.NewSize(bw, mh))
	rr.meta.Move(fyne.NewPos(x, top+bh))
}

func (rr *rowRenderer) MinSize() fyne.Size {
	w := rr.lastWidth
	if w <= 0 {
		w = 480 // 首次布局前的兜底宽度
	}
	return fyne.NewSize(w, rr.row.rowHeight(w))
}

func (rr *rowRenderer) Refresh() {
	// 主题/内容变化时重取颜色
	if rr.row.outbound {
		rr.bg.FillColor = theme.PrimaryColor()
		rr.label.TextStyle = fyne.TextStyle{}
	} else {
		rr.bg.FillColor = theme.InputBackgroundColor()
		rr.label.TextStyle = fyne.TextStyle{}
	}
	rr.label.SetText(rr.row.msg.Text)
	rr.meta.Text = rr.row.metaText()
	rr.meta.Color = metaColor(rr.row)
	rr.bg.Refresh()
	rr.meta.Refresh()
	rr.label.Refresh()
}

func metaColor(r *messageRow) color.Color {
	if r.outbound && r.msg.Status == store.StatusFailed {
		return theme.ErrorColor()
	}
	if r.outbound && r.msg.Status == store.StatusRead {
		return theme.PrimaryColor()
	}
	return theme.ForegroundColor()
}

func (rr *rowRenderer) Objects() []fyne.CanvasObject { return rr.objects }
func (rr *rowRenderer) Destroy()                     {}

// conversationTitle 返回会话显示标题。
func conversationTitle(conv hub.ConversationID, peers map[string]*hub.PeerInfo) string {
	if conv == hub.GroupLAN {
		return "🌐 局域网群聊"
	}
	if pid, ok := hub.PeerFromConversation(conv); ok {
		if p, ok2 := peers[pid.String()]; ok2 && p.Name != "" {
			return p.Name
		}
		return pid.String()[:8]
	}
	return conv
}
