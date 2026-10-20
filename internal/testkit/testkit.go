// Package testkit 提供 localhost 双节点集成测试基建：
// 在测试进程内装配完整 firefly 节点（identity → store → discovery →
// transport → hub），并提供连接字节录制、事件/状态等待等断言辅助。
package testkit

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lei/firefly/internal/config"
	"github.com/lei/firefly/internal/discovery"
	"github.com/lei/firefly/internal/hub"
	"github.com/lei/firefly/internal/identity"
	"github.com/lei/firefly/internal/store"
	"github.com/lei/firefly/internal/transport"
)

// ---------------------------------------------------------------------------
// 连接字节录制（密文断言用）
// ---------------------------------------------------------------------------

// Recorder 录制所有被观测 TCP 连接的 wire 字节（读 + 写）。
type Recorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *Recorder) record(p []byte) {
	r.mu.Lock()
	r.buf.Write(p)
	r.mu.Unlock()
}

// Bytes 返回已录制字节的副本。
func (r *Recorder) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf.Bytes()...)
}

// Len 返回已录制字节数。
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Len()
}

// Contains 报告录制字节中是否出现 sub（明文泄露检查）。
func (r *Recorder) Contains(sub string) bool {
	return bytes.Contains(r.Bytes(), []byte(sub))
}

// recordedConn 把双向字节 tee 进 Recorder。
type recordedConn struct {
	net.Conn
	rec *Recorder
}

func (c *recordedConn) Write(p []byte) (int, error) {
	c.rec.record(p)
	return c.Conn.Write(p)
}

func (c *recordedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.rec.record(p[:n])
	}
	return n, err
}

// CaptureConns 安装 transport.ConnHook，把测试期间所有 TCP 连接的
// wire 字节录入 rec；测试结束自动恢复。必须在 NewNode 之前调用。
func CaptureConns(t *testing.T, rec *Recorder) {
	t.Helper()
	prev := transport.ConnHook
	transport.ConnHook = func(c net.Conn) net.Conn { return &recordedConn{Conn: c, rec: rec} }
	t.Cleanup(func() { transport.ConnHook = prev })
}

// ---------------------------------------------------------------------------
// 节点装配
// ---------------------------------------------------------------------------

// transport.SetNameFunc 是包级全局；测试进程内多节点共享一个
// PeerID → 显示名 的注册表。
var (
	nameOnce sync.Once
	nameMu   sync.Mutex
	nameReg  = map[identity.PeerID]string{}
)

func registerName(id *identity.Identity, name string) {
	nameOnce.Do(func() {
		transport.SetNameFunc(func(i *identity.Identity) string {
			nameMu.Lock()
			defer nameMu.Unlock()
			if n, ok := nameReg[i.PeerID]; ok {
				return n
			}
			return "unknown"
		})
	})
	nameMu.Lock()
	nameReg[id.PeerID] = name
	nameMu.Unlock()
}

// Node 是测试进程内的一个完整 firefly 节点。
type Node struct {
	Name      string
	ID        *identity.Identity
	Cfg       *config.Config
	Store     *store.Store
	Disc      *discovery.Composite
	Lst       *transport.Listener
	Hub       *hub.Hub
	Log       *slog.Logger
	Dir       string // 数据目录
	Downloads string // 下载目录

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewNode 装配并启动一个节点：TCP 用系统分配端口，UDP 用调用方指定的
// 共享端口（同机双节点依赖 SO_REUSEPORT 绑定同一广播端口互发通告）。
// 清理（停 goroutine、关 listener/store）注册在 t.Cleanup。
func NewNode(t *testing.T, name string, udpPort int) *Node {
	t.Helper()

	dir := t.TempDir()
	dl := filepath.Join(dir, "downloads")
	require.NoError(t, os.MkdirAll(dl, 0o755))

	cfg, err := config.Load(dir)
	require.NoError(t, err)
	cfg.DisplayName = name
	cfg.TCPPort = 0 // 系统分配，避免与真实实例/其他测试冲突
	cfg.UDPPort = udpPort
	cfg.DownloadDir = dl

	id, err := identity.LoadOrCreate(dir)
	require.NoError(t, err)
	registerName(id, name)

	db, err := store.Open(dir)
	require.NoError(t, err)

	log := newTestLog()

	lst, err := transport.Listen(context.Background(), id, cfg.TCPPort, log)
	require.NoError(t, err)

	nameFn := func() string { return name }
	disc := discovery.NewComposite(id, nameFn, udpPort, log)
	disc.SetTCPPort(uint32(lst.Port()))

	h := hub.New(id, nameFn, disc, lst, transport.NewDialer(id, log),
		db, dir, func() string { return dl }, cfg.MaxTransfers, log)

	ctx, cancel := context.WithCancel(context.Background())
	n := &Node{
		Name: name, ID: id, Cfg: cfg, Store: db, Disc: disc, Lst: lst,
		Hub: h, Log: log, Dir: dir, Downloads: dl, cancel: cancel,
	}
	n.wg.Add(2)
	go func() {
		defer n.wg.Done()
		if err := disc.Start(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("节点 %s 发现层异常: %v", name, err)
		}
	}()
	go func() {
		defer n.wg.Done()
		if err := h.Start(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("节点 %s hub 异常: %v", name, err)
		}
	}()

	t.Cleanup(n.shutdown)
	return n
}

func (n *Node) shutdown() {
	n.cancel()
	n.wg.Wait()
	_ = n.Lst.Close()
	_ = n.Store.Close()
}

// newTestLog：默认丢弃日志；FIREFLY_TESTKIT_LOG=1 时输出 stderr debug 级。
func newTestLog() *slog.Logger {
	if os.Getenv("FIREFLY_TESTKIT_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// 等待/断言辅助
// ---------------------------------------------------------------------------

// FreeUDPPort 探测一个空闲 UDP 端口并释放（存在极小竞态窗口，测试可接受）。
func FreeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer pc.Close() //nolint:errcheck
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// WaitFor 以 50ms 间隔轮询 cond，超时判失败。
func WaitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", desc)
}

// WaitSession 等待节点与 peer 的消息会话建立（发现 + Noise XX 握手完成）。
func WaitSession(t *testing.T, n *Node, peer identity.PeerID) {
	t.Helper()
	WaitFor(t, 20*time.Second, "会话建立 "+n.Name+" ↔ "+peer.String()[:8], func() bool {
		return n.Hub.Session(peer) != nil
	})
}

// WaitEvent 从事件流中等待首个满足 match 的事件，跳过其他事件。
func WaitEvent(t *testing.T, ch <-chan hub.Event, timeout time.Duration, desc string, match func(hub.Event) bool) hub.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("事件通道已关闭，等待: %s", desc)
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("等待事件超时: %s", desc)
		}
	}
}
