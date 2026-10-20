//go:build darwin || linux

package discovery

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reusePortControl 设置 SO_REUSEADDR + SO_REUSEPORT，允许同机多实例
// 绑定同一 UDP 广播端口。广播/组播包会被投递到所有设置了
// REUSEPORT 的 socket，因此各实例都能收到彼此的心跳。
//
// 注意：Linux 的标准库 syscall 包没有定义 SO_REUSEPORT（darwin 才有），
// 必须用 golang.org/x/sys/unix 提供的常量，否则 Linux 交叉编译失败。
func reusePortControl(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); e != nil {
			sockErr = e
			return
		}
		sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
	if err != nil {
		return err
	}
	return sockErr
}
