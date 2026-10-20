package discovery

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	fireflyv1 "github.com/lei/firefly/internal/protocol/pb/firefly/v1"
)

// 广播帧格式: magic(4) + u32be len + Announce protobuf
var broadcastMagic = [4]byte{'F', 'F', 'L', 'Y'}

const maxBroadcastFrame = 2048

// broadcaster 负责 UDP 广播通道的收发。
type broadcaster struct {
	udpPort int
	self    func() *fireflyv1.Announce // 每次调用取最新自身信息（端口可能降级）
	out     chan<- rawPeerSighting
	log     *slog.Logger

	conn *net.UDPConn
}

// rawPeerSighting 是两个通道汇入 PeerIndex 前的原始观测。
type rawPeerSighting struct {
	id          [32]byte
	name        string
	platform    string
	fingerprint string
	tcpPort     uint32
	addr        netip.AddrPort // 观测到该 peer 的地址（作为 TCP 候选）
	source      Source
	goodbye     bool // 对端主动下线
}

func newBroadcaster(udpPort int, self func() *fireflyv1.Announce, out chan<- rawPeerSighting, log *slog.Logger) *broadcaster {
	return &broadcaster{udpPort: udpPort, self: self, out: out, log: log}
}

// run 监听并周期性广播，直到 ctx 取消。
func (b *broadcaster) run(ctx context.Context) error {
	// SO_REUSEPORT：同机多实例可绑定同一 UDP 端口（广播包投递到所有实例）
	lc := net.ListenConfig{Control: reusePortControl}
	pc, err := lc.ListenPacket(ctx, "udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(b.udpPort)))
	if err != nil {
		return err
	}
	conn := pc.(*net.UDPConn)
	b.conn = conn
	defer conn.Close()
	b.log.Debug("broadcast 通道就绪", "udp_port", b.udpPort)

	// 允许接收定向广播
	_ = conn.SetReadBuffer(256 * 1024)

	go b.readLoop(ctx)

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	b.announce() // 启动立即广播一次
	b.probe()

	for {
		select {
		case <-ctx.Done():
			b.goodbye()
			return nil
		case <-ticker.C:
			b.announce()
		}
	}
}

func (b *broadcaster) readLoop(ctx context.Context) {
	buf := make([]byte, maxBroadcastFrame)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = b.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, raddr, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				b.log.Debug("broadcast 读取失败", "err", err)
			}
			return
		}
		b.handlePacket(buf[:n], raddr)
	}
}

func (b *broadcaster) handlePacket(data []byte, from *net.UDPAddr) {
	if len(data) < 8 || binary.BigEndian.Uint32(data[4:8]) != uint32(len(data)-8) {
		return
	}
	if data[0] != broadcastMagic[0] || data[1] != broadcastMagic[1] ||
		data[2] != broadcastMagic[2] || data[3] != broadcastMagic[3] {
		return
	}
	var ann fireflyv1.Announce
	if err := proto.Unmarshal(data[8:], &ann); err != nil {
		return
	}
	if len(ann.PeerId) != 32 || ann.ProtoVersion != 1 {
		return
	}

	ip, ok := netip.AddrFromSlice(from.IP.To4())
	if !ok {
		return
	}
	b.log.Debug("broadcast 收到对端通告", "kind", ann.Kind, "from", from, "name", ann.Name)
	sighting := rawPeerSighting{
		name:        ann.Name,
		platform:    ann.Platform,
		fingerprint: ann.Fingerprint,
		tcpPort:     ann.TcpPort,
		addr:        netip.AddrPortFrom(ip.Unmap(), uint16(ann.TcpPort)),
		source:      SourceBroadcast,
	}
	copy(sighting.id[:], ann.PeerId)

	switch ann.Kind {
	case fireflyv1.Announce_KIND_PROBE:
		// 有新节点冷启动，立即回应 announce 加速发现
		b.announce()
	case fireflyv1.Announce_KIND_GOODBYE:
		sighting.goodbye = true
		select {
		case b.out <- sighting:
		default:
		}
	default: // ANNOUNCE
		if ann.TcpPort == 0 {
			return
		}
		select {
		case b.out <- sighting:
		default:
		}
	}
}

// announce 向全网广播地址 + 各网卡子网广播地址发送心跳。
func (b *broadcaster) announce() {
	if b.conn == nil {
		return
	}
	data := b.encode(fireflyv1.Announce_KIND_ANNOUNCE)
	targets := []netip.Addr{netip.MustParseAddr("255.255.255.255")}
	for _, iface := range eligibleInterfaces() {
		targets = append(targets, broadcastAddrs(iface)...)
	}
	for _, t := range targets {
		uaddr := &net.UDPAddr{IP: t.AsSlice(), Port: b.udpPort}
		_, _ = b.conn.WriteToUDP(data, uaddr)
	}
}

func (b *broadcaster) probe() {
	if b.conn == nil {
		return
	}
	data := b.encode(fireflyv1.Announce_KIND_PROBE)
	targets := []netip.Addr{netip.MustParseAddr("255.255.255.255")}
	for _, iface := range eligibleInterfaces() {
		targets = append(targets, broadcastAddrs(iface)...)
	}
	for _, t := range targets {
		uaddr := &net.UDPAddr{IP: t.AsSlice(), Port: b.udpPort}
		_, _ = b.conn.WriteToUDP(data, uaddr)
	}
}

func (b *broadcaster) goodbye() {
	if b.conn == nil {
		return
	}
	data := b.encode(fireflyv1.Announce_KIND_GOODBYE)
	targets := []netip.Addr{netip.MustParseAddr("255.255.255.255")}
	for _, iface := range eligibleInterfaces() {
		targets = append(targets, broadcastAddrs(iface)...)
	}
	for _, t := range targets {
		uaddr := &net.UDPAddr{IP: t.AsSlice(), Port: b.udpPort}
		_, _ = b.conn.WriteToUDP(data, uaddr)
	}
}

func (b *broadcaster) encode(kind fireflyv1.Announce_Kind) []byte {
	ann := b.self()
	ann.Kind = kind
	ann.TsUnixMs = time.Now().UnixMilli()
	payload, err := proto.Marshal(ann)
	if err != nil {
		return nil
	}
	buf := make([]byte, 8+len(payload))
	copy(buf[:4], broadcastMagic[:])
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(payload)))
	copy(buf[8:], payload)
	return buf
}
