package ui

import (
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/lei/firefly/internal/hub"
	"github.com/lei/firefly/internal/identity"
	"github.com/lei/firefly/internal/store"
)

// showPairing 是 TOFU 首见设备的指纹配对对话框：信任 / 拉黑 / 稍后。
func (c *controller) showPairing(p *hub.PeerInfo) {
	fp := widget.NewLabelWithStyle(p.Fingerprint, fyne.TextAlignCenter, fyne.TextStyle{Monospace: true, Bold: true})
	fp.Wrapping = fyne.TextWrapWord

	body := widget.NewLabel("局域网内发现了一台新设备。请与对方屏幕上显示的指纹核对，一致后再点「信任」。\n如果无法核对，可以先「稍后」——消息仍会加密传输，但无法排除中间人。")
	body.Wrapping = fyne.TextWrapWord

	name := widget.NewLabelWithStyle(p.Name+"（"+platformLabel(p.Platform)+"）", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})

	var d dialog.Dialog
	trustBtn := widget.NewButtonWithIcon("信任此设备", theme.ConfirmIcon(), func() {
		if err := c.hub.SetTrust(p.ID, store.TrustTrusted); err != nil {
			c.notifyError("设置信任失败", err)
		}
		if d != nil {
			d.Hide()
		}
	})
	trustBtn.Importance = widget.HighImportance
	blockBtn := widget.NewButtonWithIcon("拉黑", theme.CancelIcon(), func() {
		if err := c.hub.SetTrust(p.ID, store.TrustBlocked); err != nil {
			c.notifyError("拉黑失败", err)
		}
		if d != nil {
			d.Hide()
		}
	})
	blockBtn.Importance = widget.DangerImportance
	laterBtn := widget.NewButton("稍后", func() {
		if d != nil {
			d.Hide()
		}
	})

	buttons := container.NewHBox(trustBtn, laterBtn, blockBtn)
	content := container.NewGridWrap(fyne.NewSize(420, 320),
		container.NewVScroll(container.NewVBox(body, widget.NewSeparator(), name, fp, buttons)))
	d = dialog.NewCustom("新设备配对", "关闭", content, c.win)
	d.Show()
}

// showPeerInfo 显示设备详情与信任管理。
func (c *controller) showPeerInfo(pid identity.PeerID) {
	peers := c.peerSnapshot()
	p, ok := peers[pid.String()]
	if !ok {
		rec, err := c.app.Store.GetPeer(pid.String())
		if err != nil || rec == nil {
			dialog.ShowInformation("设备", "暂无该设备的记录", c.win)
			return
		}
		p = &hub.PeerInfo{
			ID: pid, Name: rec.Name, Platform: rec.Platform,
			Fingerprint: rec.Fingerprint, Trust: rec.Trust,
			Online: false, LastSeen: time.UnixMilli(rec.LastSeen),
		}
	}

	fp := widget.NewLabelWithStyle(p.Fingerprint, fyne.TextAlignLeading, fyne.TextStyle{Monospace: true})
	state := "离线"
	if p.Online {
		state = "在线"
	}
	info := widget.NewLabel(
		"名称：" + p.Name +
			"\n平台：" + platformLabel(p.Platform) +
			"\n状态：" + state +
			"\n信任：" + trustLabel(p.Trust) +
			"\n最近见到：" + p.LastSeen.Local().Format("2006-01-02 15:04:05"))
	info.Wrapping = fyne.TextWrapWord

	var d dialog.Dialog
	trustBtn := widget.NewButton("设为信任", func() {
		_ = c.hub.SetTrust(pid, store.TrustTrusted)
		if d != nil {
			d.Hide()
		}
	})
	untrustBtn := widget.NewButton("取消信任", func() {
		_ = c.hub.SetTrust(pid, store.TrustUnverified)
		if d != nil {
			d.Hide()
		}
	})
	blockBtn := widget.NewButton("拉黑", func() {
		_ = c.hub.SetTrust(pid, store.TrustBlocked)
		if d != nil {
			d.Hide()
		}
	})
	blockBtn.Importance = widget.DangerImportance
	chatBtn := widget.NewButtonWithIcon("发消息", theme.MailSendIcon(), func() {
		if d != nil {
			d.Hide()
		}
		c.openConversation(hub.DMConversation(pid))
	})
	chatBtn.Importance = widget.HighImportance

	buttons := container.NewHBox(chatBtn, trustBtn, untrustBtn, blockBtn)
	content := container.NewGridWrap(fyne.NewSize(440, 300),
		container.NewVScroll(container.NewVBox(info, widget.NewSeparator(),
			widget.NewLabelWithStyle("指纹", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), fp,
			buttons)))
	d = dialog.NewCustom("设备详情", "关闭", content, c.win)
	d.Show()
}

// showSelfInfo 显示本机身份（供对方核对指纹）。
func (c *controller) showSelfInfo() {
	self := c.selfPeer()
	fp := widget.NewLabelWithStyle(self.Fingerprint, fyne.TextAlignCenter, fyne.TextStyle{Monospace: true, Bold: true})
	fp.Wrapping = fyne.TextWrapWord
	body := widget.NewLabel("这是本机的身份指纹。其他设备配对时，请核对对方屏幕上显示的指纹与此一致：")
	body.Wrapping = fyne.TextWrapWord
	content := container.NewGridWrap(fyne.NewSize(400, 220),
		container.NewVScroll(container.NewVBox(
			body,
			widget.NewSeparator(),
			widget.NewLabelWithStyle(self.Name, fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
			fp,
		)))
	dialog.ShowCustom("本机身份", "知道了", content, c.win)
}

// showSettings 是设置对话框：显示名、下载目录、主题、端口、并发数。
func (c *controller) showSettings() {
	cfg := c.app.Cfg

	nameEntry := widget.NewEntry()
	nameEntry.SetText(cfg.DisplayName)

	dirEntry := widget.NewEntry()
	dirEntry.SetText(cfg.DownloadDir)
	browseBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		dlg := dialog.NewFolderOpen(func(uri fyne.ListableURI, err error) {
			if err != nil {
				c.notifyError("选择目录失败", err)
				return
			}
			if uri != nil {
				dirEntry.SetText(uri.Path())
			}
		}, c.win)
		dlg.Show()
	})
	dirRow := container.NewBorder(nil, nil, nil, browseBtn, dirEntry)

	themeSel := widget.NewSelect([]string{"auto", "light", "dark"}, nil)
	themeSel.SetSelected(cfg.Theme)

	tcpEntry := widget.NewEntry()
	tcpEntry.SetText(itoa(cfg.TCPPort))
	udpEntry := widget.NewEntry()
	udpEntry.SetText(itoa(cfg.UDPPort))
	maxXfer := widget.NewEntry()
	maxXfer.SetText(itoa(cfg.MaxTransfers))

	items := []*widget.FormItem{
		{Text: "显示名称", Widget: nameEntry},
		{Text: "下载目录", Widget: dirRow},
		{Text: "主题", Widget: themeSel},
		{Text: "TCP 端口（重启生效）", Widget: tcpEntry},
		{Text: "UDP 端口（重启生效）", Widget: udpEntry},
		{Text: "并发传输数", Widget: maxXfer},
	}

	form := dialog.NewForm("设置", "保存", "取消", items, func(ok bool) {
		if !ok {
			return
		}
		if v := nameEntry.Text; v != "" {
			cfg.DisplayName = v
		}
		if v := dirEntry.Text; v != "" {
			cfg.DownloadDir = v
		}
		if v := themeSel.Selected; v != "" {
			cfg.Theme = v
		}
		if v, err := atoiSafe(tcpEntry.Text); err == nil && v > 0 && v <= 65535 {
			cfg.TCPPort = v
		}
		if v, err := atoiSafe(udpEntry.Text); err == nil && v > 0 && v <= 65535 {
			cfg.UDPPort = v
		}
		if v, err := atoiSafe(maxXfer.Text); err == nil && v > 0 && v <= 16 {
			cfg.MaxTransfers = v
		}
		if err := cfg.Save(c.app.DataDir()); err != nil {
			c.notifyError("保存配置失败", err)
			return
		}
		if err := cfg.EnsureDownloadDir(); err != nil {
			c.notifyError("下载目录不可用", err)
		}
		applyTheme(cfg.Theme)
	}, c.win)
	form.Resize(fyne.NewSize(560, 420))
	form.Show()
}

// showSearch 全文搜索历史消息。
func (c *controller) showSearch() {
	entry := widget.NewEntry()
	entry.SetPlaceHolder("输入关键词，回车搜索…")

	var results []*store.Message
	list := widget.NewList(
		func() int { return len(results) },
		func() fyne.CanvasObject {
			t := widget.NewLabelWithStyle("template", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			t.Truncation = fyne.TextTruncateEllipsis
			s := widget.NewLabel("template")
			s.Truncation = fyne.TextTruncateEllipsis
			return container.NewVBox(t, s)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			if id >= len(results) {
				return
			}
			m := results[id]
			box := obj.(*fyne.Container)
			peers := c.peerSnapshot()
			title := widget.NewLabelWithStyle(
				conversationTitle(m.Conversation, peers)+" · "+time.UnixMilli(m.SentAt).Format("01-02 15:04"),
				fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			title.Truncation = fyne.TextTruncateEllipsis
			snippet := widget.NewLabel(m.Text)
			snippet.Truncation = fyne.TextTruncateEllipsis
			box.Objects = []fyne.CanvasObject{container.NewVBox(title, snippet)}
			box.Refresh()
		},
	)
	var d dialog.Dialog
	list.OnSelected = func(id widget.ListItemID) {
		if id < len(results) {
			conv := results[id].Conversation
			if d != nil {
				d.Hide()
			}
			c.openConversation(conv)
		}
	}

	entry.OnSubmitted = func(q string) {
		if q == "" {
			return
		}
		found, err := c.app.Store.SearchMessages(q, 100)
		if err != nil {
			c.log.Warn("搜索失败", "err", err)
		}
		results = found
		list.Refresh()
	}

	content := container.NewGridWrap(fyne.NewSize(520, 460),
		container.NewBorder(entry, nil, nil, nil, list))
	d = dialog.NewCustom("搜索消息", "关闭", content, c.win)
	d.Show()
}

func atoiSafe(s string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(s))
}
