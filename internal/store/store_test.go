package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrationsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	// 重开同一目录：迁移必须幂等（user_version 已到最新则跳过）
	s2, err := Open(dir)
	require.NoError(t, err)
	defer s2.Close()

	var version int
	require.NoError(t, s2.DB().QueryRow("PRAGMA user_version").Scan(&version))
	assert.Greater(t, version, 0)
}

func TestPeerUpsertAndTrust(t *testing.T) {
	s := openTestStore(t)

	p := &Peer{ID: "ab12", Name: "设备A", Platform: "darwin", Fingerprint: "AA-BB", Trust: TrustUnverified, LastSeen: 100}
	require.NoError(t, s.UpsertPeer(p))

	got, err := s.GetPeer("ab12")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "设备A", got.Name)
	assert.Equal(t, TrustUnverified, got.Trust)
	assert.Greater(t, got.FirstSeen, int64(0), "首次插入应记录 FirstSeen")

	// 更新名称不覆盖信任状态
	require.NoError(t, s.SetTrust("ab12", TrustTrusted))
	p.Name = "设备A改"
	require.NoError(t, s.UpsertPeer(p))
	got, err = s.GetPeer("ab12")
	require.NoError(t, err)
	assert.Equal(t, "设备A改", got.Name)
	assert.Equal(t, TrustTrusted, got.Trust, "Upsert 不得重置用户信任状态")

	all, err := s.ListPeers()
	require.NoError(t, err)
	assert.Len(t, all, 1)

	_, err = s.GetPeer("不存在")
	require.NoError(t, err) // 不存在返回 (nil, nil)
}

func TestMessageInsertIdempotent(t *testing.T) {
	s := openTestStore(t)

	m := &Message{ID: "01J0000000000000000000001", Conversation: "group:lan", PeerID: "ab12",
		Direction: DirectionInbound, Kind: KindText, Text: "hello", SentAt: 1000}
	inserted, err := s.InsertMessage(m)
	require.NoError(t, err)
	assert.True(t, inserted)

	// 相同 message_id 重复插入（群播扇出/重连重发场景）→ 幂等忽略
	inserted, err = s.InsertMessage(m)
	require.NoError(t, err)
	assert.False(t, inserted)

	msgs, err := s.ListMessages("group:lan", 0, 10)
	require.NoError(t, err)
	assert.Len(t, msgs, 1)
}

func TestMessagePagination(t *testing.T) {
	s := openTestStore(t)

	for i := 0; i < 25; i++ {
		m := &Message{ID: fmt.Sprintf("01J%024d", i), Conversation: "dm:ab12", PeerID: "ab12",
			Direction: DirectionOutbound, Kind: KindText, Text: fmt.Sprintf("msg-%02d", i), SentAt: int64(1000 + i)}
		_, err := s.InsertMessage(m)
		require.NoError(t, err)
	}

	// 第一页：最新 10 条，按时间升序返回（渲染顺序）
	page1, err := s.ListMessages("dm:ab12", 0, 10)
	require.NoError(t, err)
	require.Len(t, page1, 10)
	assert.Equal(t, "msg-24", page1[len(page1)-1].Text, "最新一条在最后")

	// 触顶加载：before=最早一条的 SentAt
	page2, err := s.ListMessages("dm:ab12", page1[0].SentAt, 10)
	require.NoError(t, err)
	require.Len(t, page2, 10)
	for _, m := range page2 {
		assert.Less(t, m.SentAt, page1[0].SentAt, "第二页必须全部早于第一页")
	}
}

func TestMessageStatusAndRead(t *testing.T) {
	s := openTestStore(t)

	out := &Message{ID: "m-out", Conversation: "dm:ab12", PeerID: "ab12",
		Direction: DirectionOutbound, Kind: KindText, Text: "q", SentAt: 1, Status: StatusSending}
	_, err := s.InsertMessage(out)
	require.NoError(t, err)
	in := &Message{ID: "m-in", Conversation: "dm:ab12", PeerID: "ab12",
		Direction: DirectionInbound, Kind: KindText, Text: "a", SentAt: 2, Status: StatusSent}
	_, err = s.InsertMessage(in)
	require.NoError(t, err)

	require.NoError(t, s.UpdateMessageStatus("m-out", StatusDelivered))
	msgs, err := s.ListMessages("dm:ab12", 0, 10)
	require.NoError(t, err)
	for _, m := range msgs {
		if m.ID == "m-out" {
			assert.Equal(t, StatusDelivered, m.Status)
		}
	}

	// MarkConversationRead 只回传入站未读消息的 ID（用于发已读回执）
	readIDs, err := s.MarkConversationRead("dm:ab12")
	require.NoError(t, err)
	assert.Equal(t, []string{"m-in"}, readIDs)

	readIDs, err = s.MarkConversationRead("dm:ab12")
	require.NoError(t, err)
	assert.Empty(t, readIDs, "第二次标记已读不应重复回执")
}

func TestSearchMessages(t *testing.T) {
	s := openTestStore(t)

	msgs := []struct{ id, conv, text string }{
		{"s1", "dm:ab12", "明天下午三点开会"},
		{"s2", "dm:ab12", "带一下项目文档"},
		{"s3", "group:lan", "会议取消了"},
	}
	for i, m := range msgs {
		_, err := s.InsertMessage(&Message{ID: m.id, Conversation: m.conv, PeerID: "ab12",
			Direction: DirectionInbound, Kind: KindText, Text: m.text, SentAt: int64(100 + i)})
		require.NoError(t, err)
	}

	// CJK 查询走 LIKE 子串匹配（FTS5 unicode61 无法命中中文子串）
	found, err := s.SearchMessages("会", 10)
	require.NoError(t, err)
	assert.Len(t, found, 2, "「会」应命中 s1（开会）与 s3（会议）")

	found, err = s.SearchMessages("会议", 10)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "s3", found[0].ID)

	found, err = s.SearchMessages("文档", 10)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "s2", found[0].ID)

	found, err = s.SearchMessages("不存在的词", 10)
	require.NoError(t, err)
	assert.Empty(t, found)

	// 西文查询走 FTS5
	_, err = s.InsertMessage(&Message{ID: "s4", Conversation: "dm:ab12", PeerID: "ab12",
		Direction: DirectionInbound, Kind: KindText, Text: "review the quarterly report", SentAt: 200})
	require.NoError(t, err)
	found, err = s.SearchMessages("quarterly", 10)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "s4", found[0].ID)

	// FTS 语法非法输入（不成对引号）退化为 LIKE 而非报错
	found, err = s.SearchMessages(`say "hi`, 10)
	require.NoError(t, err)
	assert.Empty(t, found)
}

func TestFileLifecycle(t *testing.T) {
	s := openTestStore(t)

	f := &File{ID: "sha256hex", TransferID: "t1", PeerID: "ab12",
		Direction: DirectionInbound, Name: "book.pdf", Size: 1024, State: FileOffered, CreatedAt: 1}
	require.NoError(t, s.UpsertFile(f))

	got, err := s.GetFile("t1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "book.pdf", got.Name)
	assert.Equal(t, FileOffered, got.State)

	require.NoError(t, s.UpdateFileState("t1", FileRunning))
	require.NoError(t, s.UpdateFileProgress("t1", 512))
	got, err = s.GetFile("t1")
	require.NoError(t, err)
	assert.Equal(t, FileRunning, got.State)
	assert.Equal(t, int64(512), got.ResumeOffset)

	byContent, err := s.GetFileByContentID("ab12", "sha256hex")
	require.NoError(t, err)
	require.NotNil(t, byContent)
	assert.Equal(t, "t1", byContent.TransferID)

	list, err := s.ListFiles("ab12", 10)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestSettings(t *testing.T) {
	s := openTestStore(t)

	_, ok, err := s.GetSetting("missing")
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, s.SetSetting("k", "v1"))
	v, ok, err := s.GetSetting("k")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "v1", v)

	require.NoError(t, s.SetSetting("k", "v2"))
	v, _, err = s.GetSetting("k")
	require.NoError(t, err)
	assert.Equal(t, "v2", v)
}
