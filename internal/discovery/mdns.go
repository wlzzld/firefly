package discovery

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enbility/zeroconf/v2"

	fireflyv1 "github.com/lei/firefly/internal/protocol/pb/firefly/v1"
)

const (
	// ServiceType 是 mDNS 服务类型（macOS NSBonjourServices 需同步声明）。
	ServiceType = "_firefly._tcp"
	// ServiceDomain 是 mDNS 域。
	ServiceDomain = "local."
	// mdnsTTL 是 mDNS 记录 TTL（秒）。
	mdnsTTL = 10
)

// mdnsChannel 负责 mDNS 注册与浏览。任何失败只降级不致命（广播通道兜底）。
type mdnsChannel struct {
	self    func() *fireflyv1.Announce
	out     chan<- rawPeerSighting
	log     *slog.Logger
	selfPID [32]byte

	mu     sync.Mutex
	server *zeroconf.Server
}

func newMDNSChannel(self func() *fireflyv1.Announce, selfPID [32]byte, out chan<- rawPeerSighting, log *slog.Logger) *mdnsChannel {
	return &mdnsChannel{self: self, out: out, log: log, selfPID: selfPID}
}

// run 注册服务并浏览对端，阻塞直到 ctx 取消。
func (m *mdnsChannel) run(ctx context.Context) {
	if err := m.register(); err != nil {
		m.log.Warn("mDNS 注册失败（广播通道仍可用）", "err", err)
	} else {
		defer m.shutdown()
	}

	entries := make(chan *zeroconf.ServiceEntry, 32)
	removed := make(chan *zeroconf.ServiceEntry, 32)

	// Browse 运行期间持续投递，消费必须在独立 goroutine 中并行进行
	go func() {
		for e := range entries {
			m.handleEntry(e)
		}
	}()
	// 服务消失事件 → 立即 goodbye（加速离线判定，不等 TTL 清扫）
	go func() {
		for e := range removed {
			m.handleRemoval(e)
		}
	}()

	// Browse 阻塞直到 ctx 取消
	if err := zeroconf.Browse(ctx, ServiceType, ServiceDomain, entries, removed); err != nil {
		if ctx.Err() == nil {
			m.log.Warn("mDNS 浏览失败（广播通道仍可用）", "err", err)
		}
	}
}

func (m *mdnsChannel) register() error {
	ann := m.self()
	if ann.TcpPort == 0 {
		return fmt.Errorf("TCP 端口尚未确定")
	}

	txt := m.txtRecords(ann)

	// zeroconf.Register 需要 []net.Interface 值切片
	ifaces := eligibleInterfaces()
	nifs := make([]net.Interface, 0, len(ifaces))
	for _, ifi := range ifaces {
		nifs = append(nifs, *ifi)
	}

	server, err := zeroconf.Register(
		ann.Name,      // 实例名 = 设备显示名
		ServiceType,   // 服务类型
		ServiceDomain, // 域
		int(ann.TcpPort),
		txt,
		nifs,
		zeroconf.TTL(mdnsTTL),
	)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.server = server
	m.mu.Unlock()
	return nil
}

func (m *mdnsChannel) txtRecords(ann *fireflyv1.Announce) []string {
	return []string{
		"v=1",
		"pid=" + hex.EncodeToString(ann.PeerId),
		"fp=" + ann.Fingerprint,
		"os=" + ann.Platform,
		"name=" + ann.Name,
		"port=" + fmt.Sprint(ann.TcpPort),
	}
}

func (m *mdnsChannel) shutdown() {
	m.mu.Lock()
	srv := m.server
	m.mu.Unlock()
	if srv != nil {
		srv.Shutdown()
	}
}

// peerIDOf 从 TXT 记录解析对端 PeerID；非法或为自己时返回 false。
func (m *mdnsChannel) peerIDOf(e *zeroconf.ServiceEntry, txtKV map[string]string) ([32]byte, bool) {
	var pid [32]byte
	pidBytes, err := hex.DecodeString(txtKV["pid"])
	if err != nil || len(pidBytes) != 32 {
		return pid, false
	}
	copy(pid[:], pidBytes)
	if pid == m.selfPID {
		return pid, false // 忽略自己
	}
	if txtKV["v"] != "1" {
		return pid, false
	}
	return pid, true
}

func parseTxt(text []string) map[string]string {
	kv := make(map[string]string, len(text))
	for _, s := range text {
		if k, v, ok := strings.Cut(s, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

// unescapeInstance 还原 dns-sd 域名转义：miekg/dns 解包时把非打印/非 ASCII
// 字节写成 \DDD（十进制字节值），反斜杠自身等特写字写成 \c 字面量。
// 设备显示名含中文时，实例名读回即为转义文本，不还原会原样入库。
func unescapeInstance(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b = append(b, s[i])
			continue
		}
		if i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]) {
			if v, err := strconv.Atoi(s[i+1 : i+4]); err == nil && v <= 255 {
				b = append(b, byte(v))
				i += 3
				continue
			}
			b = append(b, s[i]) // 非法字节值（>255）：反斜杠原样保留
			continue
		}
		i++
		b = append(b, s[i])
	}
	return string(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (m *mdnsChannel) handleEntry(e *zeroconf.ServiceEntry) {
	if e == nil {
		return
	}
	// 优先 IPv4，其次 IPv6
	var ip netip.Addr
	if len(e.AddrIPv4) > 0 {
		a, ok := netip.AddrFromSlice(e.AddrIPv4[0])
		if !ok {
			return
		}
		ip = a.Unmap()
	} else if len(e.AddrIPv6) > 0 {
		a, ok := netip.AddrFromSlice(e.AddrIPv6[0])
		if !ok {
			return
		}
		ip = a.Unmap()
	} else {
		return
	}

	txtKV := parseTxt(e.Text)
	pid, ok := m.peerIDOf(e, txtKV)
	if !ok {
		return
	}

	port := e.Port
	if p := txtKV["port"]; p != "" {
		if n, err := fmt.Sscanf(p, "%d", &port); err != nil || n != 1 {
			port = e.Port
		}
	}
	if port <= 0 || port > 65535 {
		return
	}

	sighting := rawPeerSighting{
		id:          pid,
		name:        txtKV["name"],
		platform:    txtKV["os"],
		fingerprint: txtKV["fp"],
		tcpPort:     uint32(port),
		addr:        netip.AddrPortFrom(ip, uint16(port)),
		source:      SourceMDNS,
	}
	if sighting.name == "" {
		// 兜底：mDNS 实例名。miekg/dns 解包域名时把非 ASCII 字节转义为
		// \DDD（十进制），这里反向还原，否则设备名会变成转义文本入库
		sighting.name = unescapeInstance(e.Instance)
	}
	m.log.Debug("mDNS 发现服务", "instance", e.Instance, "addr", sighting.addr, "name", sighting.name)
	select {
	case m.out <- sighting:
	default:
	}
}

// handleRemoval 处理服务消失（goodbye 包或缓存过期）。
func (m *mdnsChannel) handleRemoval(e *zeroconf.ServiceEntry) {
	if e == nil {
		return
	}
	txtKV := parseTxt(e.Text)
	pid, ok := m.peerIDOf(e, txtKV)
	if !ok {
		return
	}
	select {
	case m.out <- rawPeerSighting{id: pid, goodbye: true, source: SourceMDNS}:
	default:
	}
}

// eligibleIfaceAddrs 汇总所有合格网卡的单播地址（IPv4 优先）。
func eligibleIfaceAddrs() []netip.Addr {
	var out []netip.Addr
	for _, iface := range eligibleInterfaces() {
		out = append(out, ifaceAddrs(iface)...)
	}
	if len(out) == 0 {
		// 兜底：全部非 loopback IPv4
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					if addr, ok := netip.AddrFromSlice(ip4); ok {
						out = append(out, addr.Unmap())
					}
				}
			}
		}
	}
	return out
}

// refreshTicker 周期性刷新 TXT 记录（显示名/端口可能变化，同时作 mDNS keepalive）。
func (m *mdnsChannel) refreshTicker(ctx context.Context) {
	t := time.NewTicker(8 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.mu.Lock()
			srv := m.server
			m.mu.Unlock()
			if srv != nil {
				srv.SetText(m.txtRecords(m.self()))
			}
		}
	}
}
