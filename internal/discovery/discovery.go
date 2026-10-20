package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/lei/firefly/internal/identity"
	fireflyv1 "github.com/lei/firefly/internal/protocol/pb/firefly/v1"
)

// AppVersion 由 main 注入（构建时 -ldflags 可覆盖）。
var AppVersion = "dev"

// composite 实现 Discoverer：广播 + mDNS 双通道并行，汇入 PeerIndex。
type Composite struct {
	id      *identity.Identity
	nameFn  func() string // 显示名可动态修改
	tcpPort atomic.Uint32 // 实际广告的 TCP 端口（可能因冲突降级）
	udpPort int
	index   *peerIndex
	bc      *broadcaster
	md      *mdnsChannel
	log     *slog.Logger

	sightings chan rawPeerSighting
}

// New 创建组合发现器。
func NewComposite(id *identity.Identity, nameFn func() string, udpPort int, log *slog.Logger) *Composite {
	c := &Composite{
		id:        id,
		nameFn:    nameFn,
		udpPort:   udpPort,
		index:     newPeerIndex(id.PeerID),
		log:       log,
		sightings: make(chan rawPeerSighting, 128),
	}
	c.bc = newBroadcaster(udpPort, c.selfAnnounce, c.sightings, log)
	c.md = newMDNSChannel(c.selfAnnounce, id.PeerID, c.sightings, log)
	return c
}

// SetTCPPort 更新实际对外广告的 TCP 端口（listener 绑定后调用）。
func (c *Composite) SetTCPPort(port uint32) {
	c.tcpPort.Store(port)
}

func (c *Composite) selfAnnounce() *fireflyv1.Announce {
	return &fireflyv1.Announce{
		PeerId:       c.id.PeerID.Bytes(),
		Name:         c.nameFn(),
		Platform:     runtime.GOOS,
		TcpPort:      c.tcpPort.Load(),
		Fingerprint:  c.id.Fingerprint(),
		ProtoVersion: 1,
	}
}

func (c *Composite) Start(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)

	// 广播通道（致命：端口被占通常意味着已有实例在运行）
	g.Go(func() error {
		if err := c.bc.run(gctx); err != nil {
			if errors.Is(err, net.ErrClosed) || gctx.Err() != nil {
				return nil
			}
			return err
		}
		return nil
	})

	// mDNS 通道（尽力而为，失败只记日志）
	g.Go(func() error {
		c.md.run(gctx)
		return nil
	})
	g.Go(func() error {
		c.md.refreshTicker(gctx)
		return nil
	})

	// PeerIndex 汇聚 + TTL 清扫
	g.Go(func() error {
		sweep := time.NewTicker(1 * time.Second)
		defer sweep.Stop()
		for {
			select {
			case <-gctx.Done():
				return nil
			case s := <-c.sightings:
				c.log.Debug("发现观测", "source", s.source, "peer", fmt.Sprintf("%x", s.id[:4]),
					"addr", s.addr, "goodbye", s.goodbye)
				c.index.observe(s)
			case <-sweep.C:
				c.index.sweep()
			}
		}
	})

	return g.Wait()
}

func (c *Composite) Events() <-chan PeerEvent { return c.index.events }

func (c *Composite) Self() Peer {
	ann := c.selfAnnounce()
	return Peer{
		ID:          c.id.PeerID,
		Name:        ann.Name,
		Platform:    ann.Platform,
		Fingerprint: ann.Fingerprint,
		Source:      SourceBroadcast | SourceMDNS,
		LastSeen:    time.Now(),
	}
}

func (c *Composite) NotifyDown(id identity.PeerID) { c.index.notifyDown(id) }

// Probe 立即发送一次探测广播，请求所有在线节点尽快 announce。
func (c *Composite) Probe() { c.bc.probe() }

// OnlinePeers 返回当前在线 peer 快照。
func (c *Composite) OnlinePeers() []Peer { return c.index.snapshot() }
