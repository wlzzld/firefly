// Package transport 实现设备间加密传输：Noise XX 握手（Ed25519 签名绑定身份）
// + u32be 长度分帧 + ChaCha20-Poly1305 会话加密。
package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/flynn/noise"
)

// Noise 单帧密文上限（协议硬限制）。
const maxCiphertextLen = 65535

// 单帧明文上限 = 密文上限 - Poly1305 tag(16B)。
const MaxPlaintextLen = maxCiphertextLen - 16

var (
	ErrFrameTooLarge = errors.New("transport: 帧超过 Noise 上限")
	ErrConnClosed    = errors.New("transport: 连接已关闭")
)

// encryptFrame 加密并写入一帧：[u32be len][ciphertext]。
func encryptFrame(conn net.Conn, cs *noise.CipherState, plaintext []byte) error {
	if len(plaintext) > MaxPlaintextLen {
		return ErrFrameTooLarge
	}
	ciphertext, err := cs.Encrypt(nil, nil, plaintext)
	if err != nil {
		return fmt.Errorf("transport: 加密失败: %w", err)
	}
	buf := make([]byte, 4+len(ciphertext))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(ciphertext)))
	copy(buf[4:], ciphertext)
	_, err = conn.Write(buf)
	return err
}

// readFrame 读取并解密一帧。
func readFrame(conn net.Conn, cs *noise.CipherState) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxCiphertextLen {
		return nil, fmt.Errorf("transport: 非法帧长 %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, err
	}
	plaintext, err := cs.Decrypt(nil, nil, buf)
	if err != nil {
		return nil, fmt.Errorf("transport: 解密失败(认证错误或密钥不同步): %w", err)
	}
	return plaintext, nil
}
