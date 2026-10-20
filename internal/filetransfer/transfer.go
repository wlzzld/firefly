package filetransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/lei/firefly/internal/identity"
	fireflyv1 "github.com/lei/firefly/internal/protocol/pb/firefly/v1"
	"github.com/lei/firefly/internal/store"
)

// transfer 是单个传输的运行时状态。
type transfer struct {
	rec      *store.File
	path     string // 发送端的源文件路径
	peerName string
	sha256   []byte // 期望的全文件哈希（32B）

	resumeOffset  int64
	declineReason string

	mu        sync.Mutex
	done      int64
	cancelFn  context.CancelFunc
	startedAt time.Time
}

func (t *transfer) snapshot() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	peer, _ := identity.PeerIDFromHex(t.rec.PeerID)
	return State{
		TransferID: t.rec.TransferID,
		FileID:     t.rec.ID,
		Peer:       peer,
		PeerName:   t.peerName,
		Direction:  t.rec.Direction,
		Name:       t.rec.Name,
		Size:       t.rec.Size,
		Done:       t.done,
		Phase:      t.rec.State,
		CreatedAt:  time.UnixMilli(t.rec.CreatedAt),
	}
}

func (t *transfer) cancel(err error) {
	t.mu.Lock()
	fn := t.cancelFn
	if err != nil {
		t.rec.State = PhaseCanceled
	}
	t.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// runSend 是发送端数据面：拨号文件连接 → StreamHeader → 从 offset 推 chunk → eof。
// 若对端要求的 resume_offset>0，先校验本地源文件前缀哈希与 .part 语义一致
// （内容寻址下源文件哈希即 file_id，前缀天然一致，无需额外校验）。
func (m *Manager) runSend(t *transfer, resumeOffset int64) {
	// 并发信号量
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-time.After(0):
		// 并发已满：排队等待
		m.setPhase(t, PhaseQueued)
		m.sem <- struct{}{}
		defer func() { <-m.sem }()
	}

	peer, err := identity.PeerIDFromHex(t.rec.PeerID)
	if err != nil {
		m.fail(t, err)
		return
	}
	addr, ok := m.provider.Addr(peer)
	if !ok {
		m.fail(t, errors.New("对端离线，无法建立文件连接"))
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	t.cancelFn = cancel
	t.startedAt = time.Now()
	t.mu.Unlock()
	defer cancel()

	session, err := m.dialer.DialAddr(ctx, addr)
	if err != nil {
		m.fail(t, fmt.Errorf("拨号文件连接失败: %w", err))
		return
	}
	defer session.Close()

	if session.Peer() != peer {
		m.fail(t, errors.New("文件连接身份不匹配"))
		return
	}

	f, err := os.Open(t.path)
	if err != nil {
		m.fail(t, err)
		return
	}
	defer f.Close()

	m.setPhase(t, PhaseRunning)

	// 首帧：StreamHeader 绑定 transfer 与起始偏移
	header := &fireflyv1.Envelope{
		ProtoVersion: 1,
		MessageId:    uuid.Must(uuid.NewV7()).String(),
		SentAtUnixMs: time.Now().UnixMilli(),
		Payload: &fireflyv1.Envelope_StreamHeader{StreamHeader: &fireflyv1.StreamHeader{
			TransferId:  t.rec.TransferID,
			StartOffset: resumeOffset,
		}},
	}
	if err := session.Send(ctx, header); err != nil {
		m.fail(t, err)
		return
	}

	if _, err := f.Seek(resumeOffset, io.SeekStart); err != nil {
		m.fail(t, err)
		return
	}
	t.mu.Lock()
	t.done = resumeOffset
	t.mu.Unlock()

	buf := make([]byte, chunkSize)
	offset := resumeOffset
	stopAck := make(chan struct{})
	defer close(stopAck)
	// 读取对端 ProgressAck（同时作为流控与取消信号）
	go func() {
		for {
			select {
			case <-stopAck:
				return
			case env, ok := <-session.Recv():
				if !ok {
					return
				}
				if sp := env.GetStreamPacket(); sp != nil {
					if sp.GetAck() != nil {
						t.mu.Lock()
						t.done = int64(sp.GetAck().Offset)
						t.mu.Unlock()
					}
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			_ = m.db.UpdateFileProgress(t.rec.TransferID, offset)
			m.emitState(t)
			return
		default:
		}
		n, err := f.Read(buf)
		if n > 0 {
			pkt := &fireflyv1.Envelope{
				ProtoVersion: 1,
				MessageId:    uuid.Must(uuid.NewV7()).String(),
				SentAtUnixMs: time.Now().UnixMilli(),
				Payload: &fireflyv1.Envelope_StreamPacket{StreamPacket: &fireflyv1.StreamPacket{
					P: &fireflyv1.StreamPacket_Chunk{Chunk: buf[:n]},
				}},
			}
			if serr := session.Send(ctx, pkt); serr != nil {
				_ = m.db.UpdateFileProgress(t.rec.TransferID, offset)
				m.fail(t, serr)
				return
			}
			offset += int64(n)
			t.mu.Lock()
			t.done = offset
			t.mu.Unlock()
			m.emitProgress(t.progressAt(offset))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			m.fail(t, err)
			return
		}
	}

	// eof 帧
	eof := &fireflyv1.Envelope{
		ProtoVersion: 1,
		MessageId:    uuid.Must(uuid.NewV7()).String(),
		SentAtUnixMs: time.Now().UnixMilli(),
		Payload: &fireflyv1.Envelope_StreamPacket{StreamPacket: &fireflyv1.StreamPacket{
			P: &fireflyv1.StreamPacket_EofOffset{EofOffset: uint64(offset)},
		}},
	}
	if err := session.Send(ctx, eof); err != nil {
		m.fail(t, err)
		return
	}

	t.mu.Lock()
	t.done = t.rec.Size
	t.mu.Unlock()
	m.setPhase(t, PhaseDone)
	m.untrack(t.rec.TransferID)
	m.log.Info("文件发送完成", "name", t.rec.Name, "size", t.rec.Size)
}

// HandleStreamHeader 是接收端数据面入口：hub 收到文件连接首帧后调用。
// session 为已完成 Noise 握手的文件专用会话。
func (m *Manager) HandleStreamHeader(session interface {
	Peer() identity.PeerID
	Recv() <-chan *fireflyv1.Envelope
	Send(ctx context.Context, env *fireflyv1.Envelope) error
	Close() error
}, header *fireflyv1.StreamHeader) {
	peer := session.Peer()
	m.mu.Lock()
	t := m.transfers[header.TransferId]
	m.mu.Unlock()

	if t == nil || t.rec.Direction != Inbound {
		// 未 accept 过或 transfer 不存在：拒绝
		session.Close() //nolint:errcheck
		return
	}
	if t.rec.PeerID != peer.String() {
		// transfer 归属校验：防止其他设备冒领传输
		session.Close() //nolint:errcheck
		return
	}

	// 并发信号量
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	ctx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	t.cancelFn = cancel
	t.startedAt = time.Now()
	t.mu.Unlock()
	defer cancel()
	defer session.Close()

	m.setPhase(t, PhaseRunning)

	if err := os.MkdirAll(m.incomingDir(), 0o755); err != nil {
		m.fail(t, err)
		return
	}
	partPath := m.partPath(t.rec.ID)
	startOffset := header.StartOffset

	// .part 大小必须 ≥ startOffset 才能续传；否则从头写
	fi, err := os.Stat(partPath)
	if err == nil && fi.Size() < startOffset {
		startOffset = 0
	}
	if err == nil && fi.Size() != startOffset {
		// 截断到协商偏移，保证一致
		if terr := os.Truncate(partPath, startOffset); terr != nil {
			m.fail(t, terr)
			return
		}
	}

	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		m.fail(t, err)
		return
	}
	defer f.Close()
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		m.fail(t, err)
		return
	}

	t.mu.Lock()
	t.done = startOffset
	t.mu.Unlock()

	hasher := sha256.New()
	var written int64 = startOffset
	var sinceAck int64

	// 全文件哈希需要从头计算：若续传，先读 .part 前缀
	if startOffset > 0 {
		pf, err := os.Open(partPath)
		if err != nil {
			m.fail(t, err)
			return
		}
		if _, err := io.CopyN(hasher, pf, startOffset); err != nil && !errors.Is(err, io.EOF) {
			pf.Close()
			m.fail(t, err)
			return
		}
		pf.Close()
	}

	for {
		select {
		case <-ctx.Done():
			_ = m.db.UpdateFileProgress(t.rec.TransferID, written)
			m.setPhase(t, PhasePaused)
			return
		case env, ok := <-session.Recv():
			if !ok {
				m.fail(t, errors.New("文件连接中断"))
				return
			}
			sp := env.GetStreamPacket()
			if sp == nil {
				continue
			}
			switch p := sp.P.(type) {
			case *fireflyv1.StreamPacket_Chunk:
				if _, err := f.Write(p.Chunk); err != nil {
					m.fail(t, err)
					return
				}
				hasher.Write(p.Chunk)
				written += int64(len(p.Chunk))
				sinceAck += int64(len(p.Chunk))
				t.mu.Lock()
				t.done = written
				t.mu.Unlock()
				m.emitProgress(t.progressAt(written))
				if sinceAck >= ackIntervalBytes {
					sinceAck = 0
					_ = m.db.UpdateFileProgress(t.rec.TransferID, written)
					ack := &fireflyv1.Envelope{
						ProtoVersion: 1,
						MessageId:    uuid.Must(uuid.NewV7()).String(),
						SentAtUnixMs: time.Now().UnixMilli(),
						Payload: &fireflyv1.Envelope_StreamPacket{StreamPacket: &fireflyv1.StreamPacket{
							P: &fireflyv1.StreamPacket_Ack{Ack: &fireflyv1.ProgressAck{Offset: uint64(written)}},
						}},
					}
					_ = session.Send(ctx, ack)
				}
			case *fireflyv1.StreamPacket_EofOffset:
				if written != int64(p.EofOffset) {
					m.fail(t, fmt.Errorf("长度不一致: 写入 %d, eof %d", written, p.EofOffset))
					return
				}
				if err := f.Sync(); err != nil {
					m.fail(t, err)
					return
				}
				f.Close()
				// 全文件 SHA-256 校验（内容寻址 = file_id）
				got := hasher.Sum(nil)
				if hex.EncodeToString(got) != t.rec.ID {
					m.fail(t, errors.New("SHA-256 校验失败，文件损坏"))
					return
				}
				if err := m.finalize(t, partPath); err != nil {
					m.fail(t, err)
					return
				}
				t.mu.Lock()
				t.done = written
				t.mu.Unlock()
				m.setPhase(t, PhaseDone)
				m.untrack(t.rec.TransferID)
				m.log.Info("文件接收完成", "name", t.rec.Name, "size", written)
				return
			}
		}
	}
}

// finalize 校验通过后原子落位到下载目录。
func (m *Manager) finalize(t *transfer, partPath string) error {
	dl := m.dlDir()
	if err := os.MkdirAll(dl, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dl, uniqueDest(dl, t.rec.Name))
	// 优先 rename（同卷原子）；跨卷 EXDEV 时 copy+delete
	if err := os.Rename(partPath, dest); err != nil {
		if !errors.Is(err, os.ErrInvalid) && !isCrossDevice(err) {
			return err
		}
		if cerr := copyFile(partPath, dest); cerr != nil {
			return cerr
		}
		os.Remove(partPath) //nolint:errcheck
	}
	_ = m.db.UpsertFile(&store.File{
		ID: t.rec.ID, TransferID: t.rec.TransferID, PeerID: t.rec.PeerID,
		Direction: Inbound, Name: t.rec.Name, Size: t.rec.Size,
		State: PhaseDone, LocalPath: &dest, ResumeOffset: t.rec.Size,
		CreatedAt: t.rec.CreatedAt,
	})
	t.mu.Lock()
	destCopy := dest
	t.rec.LocalPath = &destCopy
	t.mu.Unlock()
	return nil
}

func (m *Manager) fail(t *transfer, err error) {
	m.log.Warn("传输失败", "transfer", t.rec.TransferID, "name", t.rec.Name, "err", err)
	t.mu.Lock()
	if t.rec.State != PhaseDone {
		t.rec.State = PhaseFailed
	}
	t.mu.Unlock()
	_ = m.db.UpdateFileState(t.rec.TransferID, PhaseFailed)
	s := t.snapshot()
	s.Err = err
	select {
	case m.states <- s:
	default:
	}
	m.untrack(t.rec.TransferID)
}

// progressAt 生成节流进度（调用方保证频率）。
func (t *transfer) progressAt(done int64) Progress {
	t.mu.Lock()
	start := t.startedAt
	size := t.rec.Size
	t.mu.Unlock()
	p := Progress{TransferID: t.rec.TransferID, Done: done, Size: size}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond && done > 0 {
		p.BytesPerSec = float64(done) / elapsed.Seconds()
		if remain := size - done; remain > 0 && p.BytesPerSec > 0 {
			p.ETA = time.Duration(float64(remain)/p.BytesPerSec) * time.Second
		}
	}
	return p
}

func uniqueDest(dir, name string) string {
	dest := filepath.Join(dir, name)
	if _, err := os.Stat(dest); err != nil {
		return name
	}
	ext := filepath.Ext(name)
	base := name[:len(name)-len(ext)]
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(filepath.Join(dir, candidate)); err != nil {
			return candidate
		}
	}
}

func isCrossDevice(err error) bool {
	if le, ok := err.(*os.LinkError); ok {
		return le.Err.Error() == "cross-device link" || errors.Is(le.Err, errors.New("invalid cross-device link"))
	}
	return false
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
