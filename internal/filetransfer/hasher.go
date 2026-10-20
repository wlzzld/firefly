package filetransfer

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
)

// HashFile 计算文件 SHA-256（ctx 取消时中止）。
func HashFile(ctx context.Context, path string) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()

	h := sha256.New()
	buf := make([]byte, 1024*1024)
	for {
		select {
		case <-ctx.Done():
			return [32]byte{}, ctx.Err()
		default:
		}
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return [32]byte{}, err
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// HashPrefix 计算文件前 offset 字节的 SHA-256（发送端校验续传前缀一致性）。
func HashPrefix(ctx context.Context, path string, offset int64) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.CopyN(h, f, offset); err != nil && err != io.EOF {
		return [32]byte{}, err
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}
