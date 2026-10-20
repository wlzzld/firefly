package ui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/lei/firefly/internal/filetransfer"
)

// transferPanel 是右侧传输面板：每个传输一张卡片
// （名称、方向、进度条、速度/ETA、按状态显示的操作按钮）。
type transferPanel struct {
	c     *controller
	split *container.Split // 由 controller 注入，用于显隐面板

	cards map[string]*transferCard
	order []string // transferID 顺序（新→旧）
	list  *fyne.Container
	box   fyne.CanvasObject
}

const splitVisibleOffset = 0.72

func newTransferPanel(c *controller) *transferPanel {
	tp := &transferPanel{
		c:     c,
		cards: make(map[string]*transferCard),
		list:  container.NewVBox(),
	}
	header := widget.NewLabelWithStyle("文件传输", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	hideBtn := widget.NewButtonWithIcon("", theme.VisibilityOffIcon(), tp.toggle)
	hideBtn.Importance = widget.LowImportance

	scroll := container.NewVScroll(tp.list)
	tp.box = container.NewBorder(
		container.NewBorder(nil, nil, header, hideBtn, nil),
		nil, nil, nil, scroll)
	return tp
}

func (tp *transferPanel) content() fyne.CanvasObject { return tp.box }

// toggle 通过 HSplit 偏移显隐面板。
func (tp *transferPanel) toggle() {
	if tp.split == nil {
		return
	}
	if tp.split.Offset >= 1.0 {
		tp.split.SetOffset(splitVisibleOffset)
	} else {
		tp.split.SetOffset(1.0)
	}
}

// show 强制显示面板（新传输到达时）。
func (tp *transferPanel) show() {
	if tp.split != nil && tp.split.Offset >= 1.0 {
		tp.split.SetOffset(splitVisibleOffset)
	}
}

// upsertState 处理传输状态事件（主线程调用）。
func (tp *transferPanel) upsertState(st *filetransfer.State) {
	card, ok := tp.cards[st.TransferID]
	if !ok {
		card = newTransferCard(tp, st)
		tp.cards[st.TransferID] = card
		tp.order = append([]string{st.TransferID}, tp.order...)
		tp.show() // 新传输到达时自动展开面板
	}
	card.update(st)
	card.refreshSort()
}

func (tp *transferPanel) reorder() {
	objs := make([]fyne.CanvasObject, 0, len(tp.order))
	for _, id := range tp.order {
		if card, ok := tp.cards[id]; ok {
			objs = append(objs, card.box)
		}
	}
	tp.list.Objects = objs
	tp.list.Refresh()
}

// updateProgress 处理进度事件（latest-wins，直接覆盖显示）。
func (tp *transferPanel) updateProgress(p *filetransfer.Progress) {
	card, ok := tp.cards[p.TransferID]
	if !ok {
		return
	}
	card.setProgress(p)
}

// transferCard 是单个传输卡片。
type transferCard struct {
	tp *transferPanel
	st filetransfer.State

	name   *widget.Label
	dir    *widget.Label
	bar    *widget.ProgressBar
	detail *widget.Label
	action *fyne.Container
	box    *fyne.Container
}

func newTransferCard(tp *transferPanel, st *filetransfer.State) *transferCard {
	card := &transferCard{tp: tp, st: *st}
	card.name = widget.NewLabelWithStyle(st.Name, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	card.name.Truncation = fyne.TextTruncateEllipsis
	card.dir = widget.NewLabel("")
	card.dir.TextStyle = fyne.TextStyle{Italic: true}
	card.bar = widget.NewProgressBar()
	card.detail = widget.NewLabel("")
	card.action = container.NewHBox()
	inner := container.NewVBox(
		card.name,
		card.dir,
		card.bar,
		card.detail,
		card.action,
	)
	card.box = container.NewStack(
		widget.NewCard("", "", inner),
	)
	return card
}

func (tc *transferCard) update(st *filetransfer.State) {
	tc.st = *st
	tc.name.SetText(st.Name)
	if st.Direction == filetransfer.Outbound {
		tc.dir.SetText("发送给 " + st.PeerName)
	} else {
		tc.dir.SetText("接收自 " + st.PeerName)
	}
	if st.Size > 0 {
		tc.bar.SetValue(float64(st.Done) / float64(st.Size))
	}
	tc.detail.SetText(fmt.Sprintf("%s / %s · %s",
		humanBytes(st.Done), humanBytes(st.Size), phaseLabel(st.Phase, st.Err)))
	tc.rebuildActions()
}

func (tc *transferCard) setProgress(p *filetransfer.Progress) {
	tc.st.Done = p.Done
	if p.Size > 0 {
		tc.st.Size = p.Size
		tc.bar.SetValue(float64(p.Done) / float64(p.Size))
	}
	eta := ""
	if p.ETA > 0 {
		eta = " · 剩余 " + p.ETA.Round(time.Second).String()
	}
	tc.detail.SetText(fmt.Sprintf("%s / %s · %s/s%s",
		humanBytes(p.Done), humanBytes(p.Size), humanBytes(int64(p.BytesPerSec)), eta))
}

func (tc *transferCard) rebuildActions() {
	var btns []fyne.CanvasObject
	transferID := tc.st.TransferID
	switch {
	case tc.st.Phase == filetransfer.PhaseOffered && tc.st.Direction == filetransfer.Inbound:
		btns = append(btns,
			widget.NewButtonWithIcon("接受", theme.DownloadIcon(), func() {
				go tc.tp.c.hub.Files().Accept(context.Background(), transferID) //nolint:errcheck
			}),
			widget.NewButtonWithIcon("拒绝", theme.CancelIcon(), func() {
				go tc.tp.c.hub.Files().Decline(context.Background(), transferID, "用户拒绝") //nolint:errcheck
			}),
		)
	case tc.st.Phase == filetransfer.PhaseQueued || tc.st.Phase == filetransfer.PhaseRunning ||
		tc.st.Phase == filetransfer.PhasePaused || tc.st.Phase == filetransfer.PhaseOffered:
		btns = append(btns, widget.NewButtonWithIcon("取消", theme.CancelIcon(), func() {
			go tc.tp.c.hub.Files().Cancel(context.Background(), transferID) //nolint:errcheck
		}))
	case tc.st.Phase == filetransfer.PhaseDone && tc.st.Direction == filetransfer.Inbound:
		btns = append(btns, widget.NewButtonWithIcon("打开下载目录", theme.FolderOpenIcon(), func() {
			openDir(tc.tp.c.app.Cfg.DownloadDir)
		}))
	}
	tc.action.Objects = btns
	tc.action.Refresh()
}

func (tc *transferCard) refreshSort() {
	// 完成/失败的传输沉底
	sort.SliceStable(tc.tp.order, func(i, j int) bool {
		a, okA := tc.tp.cards[tc.tp.order[i]]
		b, okB := tc.tp.cards[tc.tp.order[j]]
		if !okA || !okB {
			return okA
		}
		return phaseRank(a.st.Phase) < phaseRank(b.st.Phase)
	})
	tc.tp.reorder()
}

func phaseRank(p filetransfer.Phase) int {
	switch p {
	case filetransfer.PhaseRunning, filetransfer.PhaseQueued:
		return 0
	case filetransfer.PhaseOffered, filetransfer.PhasePaused:
		return 1
	default:
		return 2
	}
}

func phaseLabel(p filetransfer.Phase, err error) string {
	switch p {
	case filetransfer.PhaseOffered:
		return "等待对方确认"
	case filetransfer.PhaseQueued:
		return "排队中"
	case filetransfer.PhaseRunning:
		return "传输中"
	case filetransfer.PhasePaused:
		return "已暂停"
	case filetransfer.PhaseDone:
		return "完成"
	case filetransfer.PhaseFailed:
		if err != nil {
			return "失败：" + err.Error()
		}
		return "失败"
	case filetransfer.PhaseCanceled:
		return "已取消"
	case filetransfer.PhaseDeclined:
		return "已拒绝"
	default:
		return ""
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// openDir 跨平台打开目录。
func openDir(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	_ = cmd.Start()
}
