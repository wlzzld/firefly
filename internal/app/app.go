// Package app 负责装配全部模块并管理生命周期（ctx 树、优雅关停）。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/lei/firefly/internal/config"
	"github.com/lei/firefly/internal/discovery"
	"github.com/lei/firefly/internal/hub"
	"github.com/lei/firefly/internal/identity"
	"github.com/lei/firefly/internal/store"
	"github.com/lei/firefly/internal/transport"
)

// App 持有全部模块引用。
type App struct {
	Cfg       *config.Config
	ID        *identity.Identity
	Store     *store.Store
	Discovery *discovery.Composite
	Listener  *transport.Listener
	Dialer    *transport.Dialer
	Hub       *hub.Hub
	Log       *slog.Logger

	dataDir     string
	releaseLock func()
}

// New 装配应用：config → identity → store → listener → discovery → hub。
func New(log *slog.Logger) (*App, error) {
	dataDir, err := config.Dir()
	if err != nil {
		return nil, fmt.Errorf("app: 数据目录: %w", err)
	}
	// 单实例锁（同一数据目录只允许一个进程；锁持有至 Shutdown/进程退出）
	release, err := acquireLock(dataDir)
	if err != nil {
		return nil, err
	}

	cfg, err := config.Load(dataDir)
	if err != nil {
		return nil, err
	}
	if err := cfg.EnsureDownloadDir(); err != nil {
		log.Warn("下载目录不可用，使用数据目录兜底", "err", err)
		cfg.DownloadDir = filepath.Join(dataDir, "downloads")
		_ = os.MkdirAll(cfg.DownloadDir, 0o755)
	}

	id, err := identity.LoadOrCreate(dataDir)
	if err != nil {
		return nil, fmt.Errorf("app: 身份: %w", err)
	}
	log.Info("设备身份就绪", "peer_id", id.PeerID.String()[:16], "fingerprint", id.Fingerprint())

	db, err := store.Open(dataDir)
	if err != nil {
		return nil, fmt.Errorf("app: 存储: %w", err)
	}

	// TCP 监听（先绑定，实际端口交给发现层广告）
	ctx := context.Background()
	lst, err := transport.Listen(ctx, id, cfg.TCPPort, log)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("app: TCP 监听: %w", err)
	}

	disc := discovery.NewComposite(id, func() string { return cfg.DisplayName }, cfg.UDPPort, log)
	disc.SetTCPPort(uint32(lst.Port()))

	// 握手携带当前显示名
	transport.SetNameFunc(func(*identity.Identity) string { return cfg.DisplayName })

	h := hub.New(id, func() string { return cfg.DisplayName }, disc, lst,
		transport.NewDialer(id, log), db, dataDir, func() string { return cfg.DownloadDir },
		cfg.MaxTransfers, log)

	return &App{
		Cfg: cfg, ID: id, Store: db, Discovery: disc,
		Listener: lst, Dialer: transport.NewDialer(id, log),
		Hub: h, Log: log, dataDir: dataDir, releaseLock: release,
	}, nil
}

// DataDir 返回应用数据目录。
func (a *App) DataDir() string { return a.dataDir }

// Platform 返回当前平台标识。
func Platform() string { return runtime.GOOS }

// StartBackground 在独立 goroutine 中启动发现与 hub，返回统一的 errgroup 与根 ctx。
// UI 主循环由调用方（Fyne）持有；ctx 取消时全部后台模块优雅关停。
func (a *App) StartBackground(ctx context.Context) (*errgroup.Group, context.Context) {
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := a.Discovery.Start(gctx); err != nil {
			if gctx.Err() != nil {
				return nil
			}
			if isAddrInUse(err) {
				return fmt.Errorf("发现端口 %d 被占用：可能已有 Firefly 实例在运行", a.Cfg.UDPPort)
			}
			return fmt.Errorf("发现层: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		if err := a.Hub.Start(gctx); err != nil && gctx.Err() == nil {
			return fmt.Errorf("hub: %w", err)
		}
		return nil
	})
	return g, gctx
}

// Shutdown 按序关停：listener → store → 释放实例锁。
func (a *App) Shutdown() {
	_ = a.Listener.Close()
	_ = a.Store.Close()
	if a.releaseLock != nil {
		a.releaseLock()
	}
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) ||
		errors.Is(err, errors.New("address already in use")) ||
		strings.Contains(err.Error(), "address already in use")
}
