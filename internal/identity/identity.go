// Package identity 管理设备长期身份：Ed25519 签名密钥 + 派生的 X25519 传输密钥。
// PeerID 即 Ed25519 公钥字节，是全系统的身份锚点。
package identity

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PeerID 是设备身份公钥（Ed25519，32 字节）。
type PeerID [32]byte

func PeerIDFromBytes(b []byte) (PeerID, error) {
	var id PeerID
	if len(b) != len(id) {
		return id, fmt.Errorf("identity: 非法 PeerID 长度 %d", len(b))
	}
	copy(id[:], b)
	return id, nil
}

func PeerIDFromHex(s string) (PeerID, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return PeerID{}, err
	}
	return PeerIDFromBytes(b)
}

func (p PeerID) String() string { return hex.EncodeToString(p[:]) }

func (p PeerID) Bytes() []byte { b := make([]byte, 32); copy(b, p[:]); return b }

// Identity 是本机的长期身份密钥对。
type Identity struct {
	PeerID PeerID
	Pub    ed25519.PublicKey
	Priv   ed25519.PrivateKey
	// Static 是 X25519 传输静态密钥，由 Ed25519 种子经 HKDF 派生，
	// 保证签名密钥与 DH 密钥分离且重启后稳定。
	Static *ecdh.PrivateKey
}

// seedInfo 是派生 X25519 密钥的 HKDF info 参数。
const deriveInfo = "firefly/1/x25519-static"

// LoadOrCreate 从 dir/identity.key 加载身份，不存在则生成并保存（0600）。
func LoadOrCreate(dir string) (*Identity, error) {
	path := filepath.Join(dir, "identity.key")
	if data, err := os.ReadFile(path); err == nil {
		return fromSeed(data)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("identity: 读取密钥失败: %w", err)
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("identity: 生成随机种子失败: %w", err)
	}
	id, err := fromSeed(seed)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// 先写临时文件再原子 rename，避免半写状态
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, seed, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return id, nil
}

func fromSeed(seed []byte) (*Identity, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("identity: 种子长度非法 %d", len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	// HKDF-SHA256（单轮 extract+expand 即可，输出 32 字节）派生 X25519 种子
	static, err := deriveX25519(seed)
	if err != nil {
		return nil, err
	}

	var pid PeerID
	copy(pid[:], pub)
	return &Identity{PeerID: pid, Pub: pub, Priv: priv, Static: static}, nil
}

func deriveX25519(seed []byte) (*ecdh.PrivateKey, error) {
	// extract: PRK = HMAC-SHA256(salt="firefly/1", IKM=seed)
	extract := hmac.New(sha256.New, []byte("firefly/1"))
	extract.Write(seed)
	prk := extract.Sum(nil)
	// expand: T(1) = HMAC-SHA256(PRK, info || 0x01)
	expand := hmac.New(sha256.New, prk)
	expand.Write([]byte(deriveInfo))
	expand.Write([]byte{0x01})
	okm := expand.Sum(nil)
	key, err := ecdh.X25519().NewPrivateKey(okm)
	if err != nil {
		return nil, fmt.Errorf("identity: 派生 X25519 失败: %w", err)
	}
	return key, nil
}

// Sign 用身份私钥签名。
func (id *Identity) Sign(msg []byte) []byte {
	return ed25519.Sign(id.Priv, msg)
}

// Fingerprint 返回人类可读指纹，如 "7F3A-9C21-B8E0-4D52"（SHA-256(pub) 前 8 字节）。
func (id *Identity) Fingerprint() string {
	return FingerprintOf(id.Pub)
}

// FingerprintOf 计算任意 Ed25519 公钥的指纹。
func FingerprintOf(pub []byte) string {
	sum := sha256.Sum256(pub)
	hexStr := hex.EncodeToString(sum[:8])
	out := make([]byte, 0, 19)
	for i := 0; i < 16; i += 4 {
		if len(out) > 0 {
			out = append(out, '-')
		}
		out = append(out, hexStr[i:i+4]...)
	}
	return string(out)
}
