//go:build !windows

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireLock 通过 flock 排它锁保证同一数据目录只有一个实例。
// 返回释放函数（正常流程由进程退出自动释放）。
func acquireLock(dataDir string) (func(), error) {
	path := filepath.Join(dataDir, "firefly.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("另一个 Firefly 实例正在运行（数据目录 %s）", dataDir)
		}
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}, nil
}
