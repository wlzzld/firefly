// Package ui 是 Fyne 界面层：项目中唯一 import Fyne 的包。
// 线程模型约束：仅 controller.run 这一个后台 goroutine 消费 hub Event，
// 所有 widget 更新必须包在 fyne.Do 中调度到主线程。
package ui

import (
	"context"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/theme"

	"github.com/lei/firefly/internal/app"
)

// Run 启动 UI 主循环（必须在主 goroutine 调用），阻塞到窗口关闭
// 且后台模块（discovery/hub）全部退出、资源释放完毕。
func Run(a *app.App) error {
	fa := fyneapp.NewWithID("dev.lei.firefly")
	applyTheme(a.Cfg.Theme)

	w := fa.NewWindow("Firefly — 局域网聊天与文件传输")
	c := newController(a, w)
	w.SetContent(c.buildRoot())
	w.Resize(fyne.NewSize(1020, 680))
	w.CenterOnScreen()
	w.Show() // Fyne 不会自动显示窗口；漏掉则事件循环空转、界面无影

	ctx, cancel := context.WithCancel(context.Background())
	g, _ := a.StartBackground(ctx)

	// 后台模块退出后统一清理；fa.Run() 返回前等待完成
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := g.Wait(); err != nil {
			a.Log.Error("后台模块异常退出", "err", err)
			fa.SendNotification(fyne.NewNotification("Firefly 后台错误", err.Error()))
		}
		a.Shutdown()
	}()

	go c.run(ctx)

	w.SetOnClosed(cancel)
	fa.Run()
	<-done
	return nil
}

// applyTheme 按配置应用主题（auto 跟随系统）。
func applyTheme(mode string) {
	switch mode {
	case "dark":
		fyne.CurrentApp().Settings().SetTheme(theme.DarkTheme())
	case "light":
		fyne.CurrentApp().Settings().SetTheme(theme.LightTheme())
	default:
		fyne.CurrentApp().Settings().SetTheme(theme.DefaultTheme())
	}
}
