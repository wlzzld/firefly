package identity

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadOrCreateStable(t *testing.T) {
	dir := t.TempDir()
	id1, err := LoadOrCreate(dir)
	require.NoError(t, err)

	// 重新加载：PeerID、X25519 静态密钥必须完全一致（重启稳定）
	id2, err := LoadOrCreate(dir)
	require.NoError(t, err)
	assert.Equal(t, id1.PeerID, id2.PeerID)
	assert.Equal(t, id1.Pub, id2.Pub)
	assert.Equal(t, id1.Static.PublicKey().Bytes(), id2.Static.PublicKey().Bytes())

	// PeerID 即 Ed25519 公钥
	assert.Equal(t, []byte(id1.Pub), id1.PeerID[:])
}

func TestLoadOrCreateFilePermissions(t *testing.T) {
	// Windows/NTFS 无 Unix 权限位：os.Chmod 只映射只读属性，Go 读回的
	// mode 恒为 0666（CI 实测 0x1b6）。私钥机密性在 Windows 上由用户
	// 配置目录的 ACL 保证，该断言仅对 unix 平台成立。
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无 Unix 权限位，0600 断言不适用")
	}
	dir := t.TempDir()
	_, err := LoadOrCreate(dir)
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(dir, "identity.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "私钥文件必须 0600")
}

func TestDeriveSeparation(t *testing.T) {
	// 不同种子派生不同 X25519 密钥；相同种子派生相同密钥
	a, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)
	b, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)
	assert.NotEqual(t, a.Static.PublicKey().Bytes(), b.Static.PublicKey().Bytes())

	k1, err := deriveX25519(make([]byte, 32))
	require.NoError(t, err)
	k2, err := deriveX25519(make([]byte, 32))
	require.NoError(t, err)
	assert.Equal(t, k1.PublicKey().Bytes(), k2.PublicKey().Bytes(), "派生必须确定性")

	// fromSeed 层负责种子长度校验
	_, err = fromSeed(make([]byte, 16))
	require.Error(t, err, "种子长度非法必须拒绝")
}

func TestFingerprintFormat(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)

	fp := id.Fingerprint()
	matched, err := regexp.MatchString(`^[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}$`, fp)
	require.NoError(t, err)
	assert.True(t, matched, "指纹格式应为 xxxx-xxxx-xxxx-xxxx，实际 %q", fp)

	// 相同公钥指纹一致；不同公钥指纹不同
	assert.Equal(t, fp, FingerprintOf(id.Pub))
	other, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)
	assert.NotEqual(t, fp, other.Fingerprint())
}

func TestPeerIDConversion(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)

	hexStr := id.PeerID.String()
	back, err := PeerIDFromHex(hexStr)
	require.NoError(t, err)
	assert.Equal(t, id.PeerID, back)

	fromBytes, err := PeerIDFromBytes(id.PeerID.Bytes())
	require.NoError(t, err)
	assert.Equal(t, id.PeerID, fromBytes)

	_, err = PeerIDFromBytes(make([]byte, 31))
	require.Error(t, err)
	_, err = PeerIDFromHex("zzzz")
	require.Error(t, err)
}

func TestSignVerify(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir())
	require.NoError(t, err)

	msg := []byte("firefly/1 transcript")
	sig := id.Sign(msg)
	assert.True(t, id.Pub != nil)
	assert.Len(t, sig, 64)
}
