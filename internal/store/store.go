// Package store 提供本地持久化（SQLite，WAL 模式，纯 Go 驱动）。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// TrustState 是对端设备的信任状态（TOFU 状态机）。
type TrustState string

const (
	TrustUnverified TrustState = "unverified"  // 首次见到，未验证
	TrustTrusted    TrustState = "trusted"     // 用户已确认指纹
	TrustBlocked    TrustState = "blocked"     // 用户拉黑
	TrustKeyChanged TrustState = "key_changed" // 已知设备公钥变化，拒收
)

// Direction 表示消息/文件方向。
type Direction int

const (
	DirectionOutbound Direction = 0 // 本机发出
	DirectionInbound  Direction = 1 // 对端发来
)

// MessageKind 消息类型。
type MessageKind int

const (
	KindText   MessageKind = 0
	KindFile   MessageKind = 1
	KindSystem MessageKind = 2
	KindNotice MessageKind = 3
)

// MessageStatus 消息投递状态。
type MessageStatus int

const (
	StatusSending   MessageStatus = 0
	StatusSent      MessageStatus = 1
	StatusDelivered MessageStatus = 2
	StatusRead      MessageStatus = 3
	StatusFailed    MessageStatus = 4
)

// FileState 文件传输状态。
type FileState int

const (
	FileOffered  FileState = 0
	FileQueued   FileState = 1
	FileRunning  FileState = 2
	FilePaused   FileState = 3
	FileDone     FileState = 4
	FileFailed   FileState = 5
	FileCanceled FileState = 6
	FileDeclined FileState = 7
)

type Peer struct {
	ID          string // hex(Ed25519 pub)
	Name        string
	Platform    string
	Fingerprint string
	Trust       TrustState
	FirstSeen   int64 // unix ms
	LastSeen    int64
}

type Message struct {
	ID           string // UUIDv7，全局去重键
	Conversation string // dm:<hex> | group:lan
	PeerID       string // DM=对端；群聊=发送者
	Direction    Direction
	Kind         MessageKind
	Text         string
	FileID       *string
	ReplyTo      *string
	SentAt       int64 // unix ms
	ReceivedAt   *int64
	Status       MessageStatus
}

type File struct {
	ID           string // sha256 hex（内容寻址）
	TransferID   string
	PeerID       string
	Direction    Direction
	Name         string
	Size         int64
	State        FileState
	LocalPath    *string
	ResumeOffset int64
	CreatedAt    int64
	FinishedAt   *int64
}

// Store 是持久化门面。
type Store struct {
	db *sql.DB
}

// Open 打开（并迁移）dir/firefly.db。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "firefly.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite 写路径串行化：单连接足够聊天量级，且天然避免 busy
	db.SetMaxOpenConns(4)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func nowMs() int64 {
	return timeNowMs()
}
