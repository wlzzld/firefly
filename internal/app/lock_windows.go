//go:build windows

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

type overlapped struct {
	Internal     uintptr
	InternalHigh uintptr
	Offset       uint32
	OffsetHigh   uint32
	HEvent       syscall.Handle
}

// acquireLock 通过 LockFileEx 排它锁保证同一数据目录只有一个实例。
func acquireLock(dataDir string) (func(), error) {
	path := filepath.Join(dataDir, "firefly.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var ol overlapped
	ret, _, e := procLockFileEx.Call(
		uintptr(f.Fd()),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0, ^uintptr(0), ^uintptr(0),
		uintptr(unsafe.Pointer(&ol)),
	)
	if ret == 0 {
		f.Close()
		return nil, fmt.Errorf("另一个 Firefly 实例正在运行（数据目录 %s: %v）", dataDir, e)
	}
	return func() {
		var ol2 overlapped
		procUnlockFileEx.Call(uintptr(f.Fd()), 0, ^uintptr(0), ^uintptr(0), uintptr(unsafe.Pointer(&ol2))) //nolint:errcheck
		f.Close()
	}, nil
}
