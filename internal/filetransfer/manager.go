// Package filetransfer 实现点对点文件传输：
// 控制面（offer/accept/decline/cancel）走消息连接，
// 数据面每个传输独立 TCP 连接 + 独立 Noise 会话，32KiB 分块，
// SHA-256 内容寻址，支持断点续传。
package filetransfer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/lei/firefly/internal/identity"
	fireflyv1 "github.com/lei/firefly/internal/protocol/pb/firefly/v1"
	"github.com/lei/firefly/internal/store"
	"github.com/lei/firefly/internal/transport"
)

// chunkSize 是数据分块大小（Noise 单帧明文上限 65519B 内）。
const chunkSize = 32 * 1024

// ackIntervalBytes 接收端每写入这么多字节回一次 ProgressAck。
const ackIntervalBytes = 1024 * 1024

// Phase 是传输生命周期状态。
type Phase = store.FileState

const (
	PhaseOffered  = store.FileOffered
	PhaseQueued   = store.FileQueued
	PhaseRunning  = store.FileRunning
	PhasePaused   = store.FilePaused
	PhaseDone     = store.FileDone
	PhaseFailed   = store.FileFailed
	PhaseCanceled = store.FileCanceled
	PhaseDeclined = store.FileDeclined
)

// Direction 传输方向。
type Direction = store.Direction

const (
	Outbound = store.DirectionOutbound // 本机发送
	Inbound  = store.DirectionInbound  // 本机接收
)

// State 是对外暴露的传输状态快照（UI 消费）。
type State struct {
	TransferID string
	FileID     string
	Peer       identity.PeerID
	PeerName   string
	Direction  Direction
	Name       string
	Size       int64
	Done       int64
	Phase      Phase
	Err        error
	CreatedAt  time.Time
}

// Progress 是节流后的进度事件。
type Progress struct {
	TransferID  string
	Done, Size  int64
	BytesPerSec float64
	ETA         time.Duration
}

// SessionProvider 由 hub 实现：为指定 peer 提供消息会话（控制面）与地址（数据面拨号）。
type SessionProvider interface {
	// Session 返回与该 peer 的活跃消息会话（不存在则 nil）。
	Session(peer identity.PeerID) transport.Session
	// Addr 返回该 peer 当前可达的 TCP 地址（用于拨文件连接）。
	Addr(peer identity.PeerID) (netip.AddrPort, bool)
	// Send 通过消息会话发送控制 Envelope。
	Send(ctx context.Context, peer identity.PeerID, env *fireflyv1.Envelope) error
}

// Manager 管理全部文件传输。
type Manager struct {
	id       *identity.Identity
	provider SessionProvider
	db       *store.Store
	dataDir  string // 数据目录（files/incoming 所在）
	dlDir    func() string
	dialer   *transport.Dialer
	log      *slog.Logger

	sem       chan struct{} // 并发传输信号量
	mu        sync.Mutex
	transfers map[string]*transfer

	states   chan State
	progress chan Progress
	subMu    sync.Mutex
}

// NewManager 创建传输管理器。maxConcurrent 为全局并发传输上限。
func NewManager(id *identity.Identity, provider SessionProvider, db *store.Store, dataDir string, dlDir func() string, maxConcurrent int, log *slog.Logger) *Manager {
	if maxConcurrent <= 0 {
		maxConcurrent = 3
	}
	return &Manager{
		id:        id,
		provider:  provider,
		db:        db,
		dataDir:   dataDir,
		dlDir:     dlDir,
		dialer:    transport.NewDialer(id, log),
		log:       log,
		sem:       make(chan struct{}, maxConcurrent),
		transfers: make(map[string]*transfer),
		states:    make(chan State, 128),
		progress:  make(chan Progress, 256),
	}
}

// States 返回状态变化流（含进度合并前的完整状态）。
func (m *Manager) States() <-chan State { return m.states }

// Progresses 返回进度流（UI 应 latest-wins 合并）。
func (m *Manager) Progresses() <-chan Progress { return m.progress }

func (m *Manager) incomingDir() string { return filepath.Join(m.dataDir, "files", "incoming") }

// SendOffer 发起文件发送：计算 SHA-256 → 记录 DB → 通过消息会话发 FileOffer。
func (m *Manager) SendOffer(ctx context.Context, peer identity.PeerID, peerName, conversationID, path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", errors.New("filetransfer: 暂不支持发送目录")
	}
	if fi.Size() == 0 {
		return "", errors.New("filetransfer: 不支持空文件")
	}

	sum, err := HashFile(ctx, path)
	if err != nil {
		return "", fmt.Errorf("filetransfer: 计算哈希失败: %w", err)
	}
	fileID := hex.EncodeToString(sum[:])
	transferID := uuid.Must(uuid.NewV7()).String()

	rec := &store.File{
		ID:         fileID,
		TransferID: transferID,
		PeerID:     peer.String(),
		Direction:  Outbound,
		Name:       filepath.Base(path),
		Size:       fi.Size(),
		State:      PhaseOffered,
		LocalPath:  &path,
		CreatedAt:  time.Now().UnixMilli(),
	}
	if err := m.db.UpsertFile(rec); err != nil {
		return "", err
	}

	t := &transfer{
		rec:      rec,
		path:     path,
		peerName: peerName,
		sha256:   sum[:],
	}
	m.track(t)

	env := &fireflyv1.Envelope{
		ProtoVersion: 1,
		MessageId:    transferID,
		SentAtUnixMs: time.Now().UnixMilli(),
		Payload: &fireflyv1.Envelope_FileOffer{FileOffer: &fireflyv1.FileOffer{
			TransferId:     transferID,
			FileId:         fileID,
			Name:           rec.Name,
			Size:           fi.Size(),
			Sha256:         sum[:],
			ConversationId: conversationID,
		}},
	}
	if err := m.provider.Send(ctx, peer, env); err != nil {
		m.untrack(transferID)
		_ = m.db.UpdateFileState(transferID, PhaseFailed)
		return "", fmt.Errorf("filetransfer: 发送 offer 失败（对端可能离线）: %w", err)
	}
	m.emitState(t)
	return transferID, nil
}

// HandleOffer 处理入站 FileOffer（对端想发文件给我）。
func (m *Manager) HandleOffer(peer identity.PeerID, peerName string, offer *fireflyv1.FileOffer) {
	rec := &store.File{
		ID:         offer.FileId,
		TransferID: offer.TransferId,
		PeerID:     peer.String(),
		Direction:  Inbound,
		Name:       sanitizeName(offer.Name),
		Size:       offer.Size,
		State:      PhaseOffered,
		CreatedAt:  time.Now().UnixMilli(),
	}
	if err := m.db.UpsertFile(rec); err != nil {
		m.log.Error("filetransfer: 记录 offer 失败", "err", err)
		return
	}
	t := &transfer{rec: rec, peerName: peerName, sha256: offer.Sha256}
	m.track(t)
	m.emitState(t) // UI 弹出接受/拒绝卡片
}

// Accept 接受入站传输：检查本地 .part 断点，回 FileResponse.accept(offset)。
func (m *Manager) Accept(ctx context.Context, transferID string) error {
	m.mu.Lock()
	t := m.transfers[transferID]
	m.mu.Unlock()
	if t == nil {
		return errors.New("filetransfer: 未知 transfer")
	}
	if t.rec.Direction != Inbound {
		return errors.New("filetransfer: 只能接受入站传输")
	}

	offset := m.localResumeOffset(t)
	peer, err := identity.PeerIDFromHex(t.rec.PeerID)
	if err != nil {
		return err
	}
	env := &fireflyv1.Envelope{
		ProtoVersion: 1,
		MessageId:    uuid.Must(uuid.NewV7()).String(),
		SentAtUnixMs: time.Now().UnixMilli(),
		Payload: &fireflyv1.Envelope_FileResponse{FileResponse: &fireflyv1.FileResponse{
			TransferId: transferID,
			Decision:   &fireflyv1.FileResponse_AcceptResumeOffset{AcceptResumeOffset: offset},
		}},
	}
	if err := m.provider.Send(ctx, peer, env); err != nil {
		return err
	}
	t.resumeOffset = offset
	m.setPhase(t, PhaseQueued)
	return nil
}

// localResumeOffset 返回本地已有 .part 文件的可信续传偏移（校验前缀哈希由发送端二次确认）。
func (m *Manager) localResumeOffset(t *transfer) int64 {
	partPath := m.partPath(t.rec.ID)
	fi, err := os.Stat(partPath)
	if err != nil || fi.Size() >= t.rec.Size || fi.Size() == 0 {
		return 0
	}
	return fi.Size()
}

func (m *Manager) partPath(fileID string) string {
	return filepath.Join(m.incomingDir(), fileID+".part")
}

// Decline 拒绝入站传输。
func (m *Manager) Decline(ctx context.Context, transferID string, reason string) error {
	m.mu.Lock()
	t := m.transfers[transferID]
	m.mu.Unlock()
	if t == nil {
		return errors.New("filetransfer: 未知 transfer")
	}
	peer, err := identity.PeerIDFromHex(t.rec.PeerID)
	if err != nil {
		return err
	}
	env := &fireflyv1.Envelope{
		ProtoVersion: 1,
		MessageId:    uuid.Must(uuid.NewV7()).String(),
		SentAtUnixMs: time.Now().UnixMilli(),
		Payload: &fireflyv1.Envelope_FileResponse{FileResponse: &fireflyv1.FileResponse{
			TransferId: transferID,
			Decision:   &fireflyv1.FileResponse_DeclineReason{DeclineReason: reason},
		}},
	}
	_ = m.provider.Send(ctx, peer, env)
	m.setPhase(t, PhaseDeclined)
	m.untrack(transferID)
	return nil
}

// Cancel 取消传输（双向）。关闭数据连接，通知对端，保留 .part 供续传。
func (m *Manager) Cancel(ctx context.Context, transferID string) error {
	m.mu.Lock()
	t := m.transfers[transferID]
	m.mu.Unlock()
	if t == nil {
		return nil
	}
	t.cancel(nil)
	peer, err := identity.PeerIDFromHex(t.rec.PeerID)
	if err == nil {
		env := &fireflyv1.Envelope{
			ProtoVersion: 1,
			MessageId:    uuid.Must(uuid.NewV7()).String(),
			SentAtUnixMs: time.Now().UnixMilli(),
			Payload: &fireflyv1.Envelope_FileCancel{FileCancel: &fireflyv1.FileCancel{
				TransferId: transferID,
				Reason:     "canceled by user",
			}},
		}
		_ = m.provider.Send(ctx, peer, env)
	}
	m.setPhase(t, PhaseCanceled)
	m.untrack(transferID)
	return nil
}

// HandleResponse 处理对端对我方 offer 的 accept/decline。
func (m *Manager) HandleResponse(peer identity.PeerID, resp *fireflyv1.FileResponse) {
	m.mu.Lock()
	t := m.transfers[resp.TransferId]
	m.mu.Unlock()
	if t == nil {
		return
	}
	switch d := resp.Decision.(type) {
	case *fireflyv1.FileResponse_DeclineReason:
		t.declineReason = d.DeclineReason
		m.setPhase(t, PhaseDeclined)
		m.untrack(t.rec.TransferID)
	case *fireflyv1.FileResponse_AcceptResumeOffset:
		go m.runSend(t, d.AcceptResumeOffset)
	}
}

// HandleCancel 处理对端取消。
func (m *Manager) HandleCancel(peer identity.PeerID, c *fireflyv1.FileCancel) {
	m.mu.Lock()
	t := m.transfers[c.TransferId]
	m.mu.Unlock()
	if t == nil {
		return
	}
	t.cancel(errors.New("对端取消: " + c.Reason))
	m.setPhase(t, PhaseCanceled)
	m.untrack(t.rec.TransferID)
}

func (m *Manager) track(t *transfer) {
	m.mu.Lock()
	m.transfers[t.rec.TransferID] = t
	m.mu.Unlock()
}

func (m *Manager) untrack(transferID string) {
	m.mu.Lock()
	delete(m.transfers, transferID)
	m.mu.Unlock()
}

func (m *Manager) setPhase(t *transfer, p Phase) {
	t.mu.Lock()
	t.rec.State = p
	t.mu.Unlock()
	_ = m.db.UpdateFileState(t.rec.TransferID, p)
	m.emitState(t)
}

func (m *Manager) emitState(t *transfer) {
	select {
	case m.states <- t.snapshot():
	default:
	}
}

func (m *Manager) emitProgress(p Progress) {
	select {
	case m.progress <- p:
	default:
	}
}

// sanitizeName 去除路径分隔符等危险字符，防止路径穿越。
func sanitizeName(name string) string {
	base := filepath.Base(filepath.Clean("/" + name))
	if base == "." || base == ".." || base == "/" || base == string(filepath.Separator) {
		return "unnamed"
	}
	return base
}
