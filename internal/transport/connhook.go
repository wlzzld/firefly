package transport

import "net"

// ConnHook 供集成测试观测/包装每条 TCP 连接（在握手之前的裸连接层生效，
// 因此录到的字节即 wire 字节，可用于断言无明文泄露）。
// 生产代码从不设置（保持 nil）。测试必须在启动任何节点之前设置，
// 且同一进程内同一时间只允许一个测试使用（go test 各包独立进程，天然隔离）。
var ConnHook func(net.Conn) net.Conn

// wrapConn 应用 ConnHook（nil 时原样返回）。
func wrapConn(c net.Conn) net.Conn {
	if ConnHook != nil {
		return ConnHook(c)
	}
	return c
}
