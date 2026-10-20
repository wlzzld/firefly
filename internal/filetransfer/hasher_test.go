package filetransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	content := bytes.Repeat([]byte("firefly"), 5000) // ~35KB
	require.NoError(t, os.WriteFile(path, content, 0o600))

	want := sha256.Sum256(content)
	got, err := HashFile(context.Background(), path)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestHashFileEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	want := sha256.Sum256(nil)
	got, err := HashFile(context.Background(), path)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestHashFileMissing(t *testing.T) {
	_, err := HashFile(context.Background(), filepath.Join(t.TempDir(), "nope"))
	require.Error(t, err)
}

func TestHashFileCanceled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte{1}, 4096), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := HashFile(ctx, path)
	require.ErrorIs(t, err, context.Canceled)
}

// TestHashPrefixConsistency 验证断点续传的前缀哈希语义：
// offset=文件全长时 HashPrefix == HashFile；短前缀是独立哈希。
func TestHashPrefixConsistency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	content := bytes.Repeat([]byte("abcdefgh"), 1024) // 8KB
	require.NoError(t, os.WriteFile(path, content, 0o600))

	full, err := HashFile(context.Background(), path)
	require.NoError(t, err)

	prefixFull, err := HashPrefix(context.Background(), path, int64(len(content)))
	require.NoError(t, err)
	assert.Equal(t, full, prefixFull, "offset=全长应与整文件哈希一致")

	half := sha256.Sum256(content[:len(content)/2])
	prefixHalf, err := HashPrefix(context.Background(), path, int64(len(content)/2))
	require.NoError(t, err)
	assert.Equal(t, half, prefixHalf)
	assert.NotEqual(t, full, prefixHalf)
}
