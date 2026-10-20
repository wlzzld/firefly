// Firefly — 去中心化的局域网聊天与文件传输客户端。
package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/lei/firefly/internal/app"
	"github.com/lei/firefly/internal/config"
	"github.com/lei/firefly/internal/discovery"
	"github.com/lei/firefly/internal/ui"
)

// version 由构建时 -ldflags 注入。
var version = "dev"

func main() {
	discovery.AppVersion = version
	log := setupLog()

	a, err := app.New(log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Firefly 启动失败：", err)
		os.Exit(1)
	}
	log.Info("Firefly 启动", "version", version, "data_dir", a.DataDir(),
		"tcp_port", a.Listener.Port(), "udp_port", a.Cfg.UDPPort)

	if err := ui.Run(a); err != nil {
		log.Error("UI 异常退出", "err", err)
		os.Exit(1)
	}
}

// setupLog 初始化日志：默认写入数据目录 firefly.log，
// FIREFLY_DEBUG=1 时同时输出 stderr 并开启 debug 级别。
func setupLog() *slog.Logger {
	level := slog.LevelInfo
	var writers []io.Writer
	if os.Getenv("FIREFLY_DEBUG") != "" {
		level = slog.LevelDebug
		writers = append(writers, os.Stderr)
	}
	if dir, err := config.Dir(); err == nil {
		if f, err := os.OpenFile(filepath.Join(dir, "firefly.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			writers = append(writers, f)
		}
	}
	var w io.Writer = io.Discard
	if len(writers) == 1 {
		w = writers[0]
	} else if len(writers) > 1 {
		w = io.MultiWriter(writers...)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
