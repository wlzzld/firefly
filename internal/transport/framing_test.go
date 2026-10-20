package transport

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"github.com/flynn/noise"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statePair 构造同密钥的加密/解密 CipherState 对（仅测试用；
// 真实会话中双向密钥由 XX 握手派生且互不相同）。
func statePair(t *testing.T, seed byte) (enc, dec *noise.CipherState) {
	t.Helper()
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{seed}, 32))
	suite := cipherSuite()
	enc = noise.UnsafeNewCipherState(suite, key, 0)
	dec = noise.UnsafeNewCipherState(suite, key, 0)
	return
}

func TestFrameRoundtrip(t *testing.T) {
	enc, dec := statePair(t, 0x11)
	client, server := net.Pipe()

	msgs := [][]byte{
		[]byte("hello"),
		bytes.Repeat([]byte{0xab}, 32*1024), // 典型文件 chunk 大小
		{},
	}
	errCh := make(chan error, 1)
	go func() {
		for _, m := range msgs {
			if err := encryptFrame(client, enc, m); err != nil {
				errCh <- err
				return
			}
		}
		errCh <- nil
	}()
	for _, want := range msgs {
		got, err := readFrame(server, dec)
		require.NoError(t, err)
		assert.Equal(t, len(want), len(got))
		assert.True(t, bytes.Equal(want, got))
	}
	require.NoError(t, <-errCh)
}

// TestFrameMaxPlaintextBoundary 验证 Noise 单帧上限边界：
// 恰好 MaxPlaintextLen 允许，+1 必须拒绝（协议硬限制）。
func TestFrameMaxPlaintextBoundary(t *testing.T) {
	enc, dec := statePair(t, 0x22)
	client, server := net.Pipe()

	exact := bytes.Repeat([]byte{0x01}, MaxPlaintextLen)
	errCh := make(chan error, 1)
	go func() { errCh <- encryptFrame(client, enc, exact) }()

	got, err := readFrame(server, dec) // 消费帧，避免 net.Pipe 阻塞写方
	require.NoError(t, err)
	assert.Len(t, got, MaxPlaintextLen)
	require.NoError(t, <-errCh)

	// 超过上限：加密前即拒绝，不产生任何网络写入
	tooLarge := bytes.Repeat([]byte{0x01}, MaxPlaintextLen+1)
	require.ErrorIs(t, encryptFrame(client, enc, tooLarge), ErrFrameTooLarge)
}

func TestReadFrameRejectsBadLength(t *testing.T) {
	_, dec := statePair(t, 0x33)
	client, server := net.Pipe()

	for _, bad := range []uint32{0, maxCiphertextLen + 1, 0xffffffff} {
		go func(n uint32) {
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], n)
			_, _ = client.Write(hdr[:])
		}(bad)
		_, err := readFrame(server, dec)
		require.Error(t, err, "帧长 %d 应被拒绝", bad)
		assert.Contains(t, err.Error(), "非法帧长")
	}
}

// TestReadFrameDetectsTamper 验证密文被篡改时 AEAD 认证失败。
func TestReadFrameDetectsTamper(t *testing.T) {
	enc, dec := statePair(t, 0x44)
	client, server := net.Pipe()

	go func() {
		ciphertext, err := enc.Encrypt(nil, nil, []byte("secret message"))
		if err != nil {
			return
		}
		ciphertext[len(ciphertext)/2] ^= 0x01 // 中间人翻转一个比特
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(ciphertext)))
		_, _ = client.Write(append(hdr[:], ciphertext...))
	}()

	_, err := readFrame(server, dec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "解密失败")
}
