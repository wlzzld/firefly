package ui

// 布局回归测试：守护消息气泡与侧栏设备行两处自绘布局的几何不变量，
// 防止历史 bug 复发（气泡文本不居中；设备行高 0 导致列表不可点选）。

import (
	"math"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/lei/firefly/internal/hub"
	"github.com/lei/firefly/internal/identity"
	"github.com/lei/firefly/internal/store"
)

func closeEnough(got, want float32) bool { return math.Abs(float64(got-want)) < 0.5 }

// TestBubbleCentering 回归：气泡 = 文本 + Label 内部留白 + 外边距，四边对称。
// 历史 bug：漏算 Label 内部留白，折行比预期早、文本溢出气泡，表现为不居中。
func TestBubbleCentering(t *testing.T) {
	test.NewApp()
	msg := &store.Message{
		ID: "1", Conversation: "group:lan", Direction: store.DirectionOutbound,
		Text: "大家好", SentAt: 1729400000000, Status: store.StatusDelivered,
	}
	r := newMessageRow(msg, "", nil, nil)

	pad := theme.Padding()
	ip := labelInnerPad()
	textW := fyne.MeasureText(msg.Text, bubbleTextSize(), fyne.TextStyle{}).Width
	lineH := fyne.MeasureText("汉", bubbleTextSize(), fyne.TextStyle{}).Height

	w, h := r.bubbleSize(480)
	if want := textW + 2*pad + 2*ip; !closeEnough(w, want) {
		t.Fatalf("单行气泡宽 %v，期望 %v（文本+对称留白）", w, want)
	}
	if want := lineH + 2*ip + 2*pad; !closeEnough(h, want) {
		t.Fatalf("单行气泡高 %v，期望 %v", h, want)
	}

	// 布局后 Label 相对气泡四边各内缩一个外边距（对称即视觉居中）
	rr := test.WidgetRenderer(r).(*rowRenderer)
	rr.Layout(fyne.NewSize(480, r.rowHeight(480)))
	bp, bs := rr.bg.Position(), rr.bg.Size()
	lp, ls := rr.label.Position(), rr.label.Size()
	if !closeEnough(lp.X-bp.X, pad) || !closeEnough(lp.Y-bp.Y, pad) {
		t.Fatalf("Label 偏移 (%v,%v)，气泡位置 (%v,%v)，未留对称外边距 %v", lp.X, lp.Y, bp.X, bp.Y, pad)
	}
	if !closeEnough(bs.Width-ls.Width, 2*pad) || !closeEnough(bs.Height-ls.Height, 2*pad) {
		t.Fatalf("Label 尺寸 (%v,%v) 与气泡 (%v,%v) 之差不是对称外边距", ls.Width, ls.Height, bs.Width, bs.Height)
	}
}

// TestMessageSecondaryMenu 回归：右键消息必须触发复制菜单回调
// （复制/选中复制能力的入口，防止重构后回调丢失）。
func TestMessageSecondaryMenu(t *testing.T) {
	test.NewApp()
	msg := &store.Message{
		ID: "1", Conversation: "group:lan", Direction: store.DirectionInbound,
		Text: "复制我", SentAt: 1729400000000,
	}
	var got *store.Message
	r := newMessageRow(msg, "", nil, func(m *store.Message, _ fyne.Position) { got = m })
	r.TappedSecondary(&fyne.PointEvent{})
	if got != msg {
		t.Fatal("右键消息未触发复制菜单回调")
	}
}

// TestSidebarRows 回归：设备行模板必须有真实尺寸且选中事件正确派发。
// 历史 bug：createItem 返回空 Stack 模板 → 行高 0，行叠画且点击命中区为零，
// 用户无法选择设备发起一对一单聊。
func TestSidebarRows(t *testing.T) {
	test.NewApp()
	c := &controller{
		current:  hub.GroupLAN,
		peers:    make(map[string]*hub.PeerInfo),
		unread:   make(map[hub.ConversationID]int),
		pairings: make(map[string]bool),
	}
	var id1, id2 identity.PeerID
	id1[0], id2[0] = 0xaa, 0xbb
	c.peers[id1.String()] = &hub.PeerInfo{ID: id1, Name: "甲", Platform: "darwin", Online: true}
	c.peers[id2.String()] = &hub.PeerInfo{ID: id2, Name: "乙", Platform: "linux", Online: false}

	sb := newSidebarView(c)
	sb.refresh()

	w := test.NewWindow(sb.content())
	defer w.Close()
	w.Resize(fyne.NewSize(280, 420))

	if h := sb.list.MinSize().Height; h < 40 {
		t.Fatalf("设备列表高度异常（%v），行可能仍为 0 高", h)
	}

	row := sb.createItem()
	sb.updateItem(0, row)
	if h := row.MinSize().Height; h < 30 {
		t.Fatalf("设备行高度异常（%v），行模板退化会导致列表不可点选", h)
	}
	if rw := row.MinSize().Width; rw <= 0 {
		t.Fatalf("设备行宽度为零（%v）", rw)
	}

	// 选中路径应能触发 OnSelected（真实点击即走这条回调）
	var selected []widget.ListItemID
	sb.list.OnSelected = func(id widget.ListItemID) { selected = append(selected, id) }
	sb.list.Select(1)
	if len(selected) != 1 || selected[0] != 1 {
		t.Fatalf("列表选中事件未正确派发: %v", selected)
	}
}
