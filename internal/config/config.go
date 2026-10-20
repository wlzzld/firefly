// Package config 管理应用配置（JSON，持久化于用户配置目录）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	// DefaultTCPPort 是消息/文件传输的默认 TCP 监听端口。
	DefaultTCPPort = 51770
	// DefaultUDPPort 是发现广播的 UDP 端口。
	DefaultUDPPort = 51771
	// MaxConcurrentTransfers 是全局并发文件传输上限。
	MaxConcurrentTransfers = 3
)

type Config struct {
	DisplayName string `json:"display_name"`
	TCPPort     int    `json:"tcp_port"`
	UDPPort     int    `json:"udp_port"`
	DownloadDir string `json:"download_dir"`
	// Theme: "auto" | "light" | "dark"
	Theme string `json:"theme"`
	// MaxTransfers 并发文件传输数
	MaxTransfers int `json:"max_transfers"`
}

func Default() *Config {
	name, _ := os.Hostname()
	if name == "" {
		name = "firefly-" + runtime.GOOS
	}
	dl := filepath.Join(UserHome(), "Downloads", "Firefly")
	return &Config{
		DisplayName:  name,
		TCPPort:      DefaultTCPPort,
		UDPPort:      DefaultUDPPort,
		DownloadDir:  dl,
		Theme:        "auto",
		MaxTransfers: MaxConcurrentTransfers,
	}
}

// Dir 返回应用数据目录（identity.key、firefly.db、files/ 均在其中）。
// 可通过环境变量 FIREFLY_DATA_DIR 覆盖（多实例测试用）。
func Dir() (string, error) {
	if d := os.Getenv("FIREFLY_DATA_DIR"); d != "" {
		return d, os.MkdirAll(d, 0o700)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "firefly")
	return dir, os.MkdirAll(dir, 0o700)
}

func UserHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// Load 读取 dir/config.json，不存在则返回默认值（不落盘，Save 时创建）。
func Load(dir string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if os.IsNotExist(err) {
		return cfg, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: 解析失败: %w", err)
	}
	cfg.sanitize()
	return cfg, nil
}

// Save 原子写入配置文件。
func (c *Config) Save(dir string) error {
	c.sanitize()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) sanitize() {
	if c.DisplayName == "" {
		c.DisplayName = Default().DisplayName
	}
	if c.TCPPort <= 0 || c.TCPPort > 65535 {
		c.TCPPort = DefaultTCPPort
	}
	if c.UDPPort <= 0 || c.UDPPort > 65535 {
		c.UDPPort = DefaultUDPPort
	}
	if c.DownloadDir == "" {
		c.DownloadDir = Default().DownloadDir
	}
	if c.Theme != "light" && c.Theme != "dark" {
		c.Theme = "auto"
	}
	if c.MaxTransfers <= 0 {
		c.MaxTransfers = MaxConcurrentTransfers
	}
}

// EnsureDownloadDir 确保下载目录存在。
func (c *Config) EnsureDownloadDir() error {
	return os.MkdirAll(c.DownloadDir, 0o755)
}
