//go:build !darwin && !linux

package discovery

import "syscall"

// reusePortControl 在 Windows 上为空操作：SO_REUSEADDR 语义不同
// （会允许端口劫持），同机多实例调试请使用不同 UDP 端口 + mDNS 通道。
func reusePortControl(network, address string, c syscall.RawConn) error {
	return nil
}
