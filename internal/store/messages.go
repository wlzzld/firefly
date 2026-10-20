package store

import (
	"database/sql"
	"time"
)

// InsertMessage 写入消息；message_id 冲突时静默忽略（广播扇出/重试幂等）。
// 返回 true 表示实际插入了新行。
func (s *Store) InsertMessage(m *Message) (bool, error) {
	res, err := s.db.Exec(`
INSERT OR IGNORE INTO messages
    (id, conversation, peer_id, direction, kind, text, file_id, reply_to, sent_at, received_at, status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Conversation, m.PeerID, int(m.Direction), int(m.Kind), m.Text,
		m.FileID, m.ReplyTo, m.SentAt, m.ReceivedAt, int(m.Status))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListMessages 按时间倒序分页拉取（before=0 表示最新），返回结果按时间升序排列。
func (s *Store) ListMessages(conversation string, before int64, limit int) ([]*Message, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	if before <= 0 {
		rows, err = s.db.Query(`
SELECT id, conversation, peer_id, direction, kind, text, file_id, reply_to, sent_at, received_at, status
FROM messages WHERE conversation = ?
ORDER BY sent_at DESC, id DESC LIMIT ?`, conversation, limit)
	} else {
		rows, err = s.db.Query(`
SELECT id, conversation, peer_id, direction, kind, text, file_id, reply_to, sent_at, received_at, status
FROM messages WHERE conversation = ? AND sent_at < ?
ORDER BY sent_at DESC, id DESC LIMIT ?`, conversation, before, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 反转为时间升序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s *Store) UpdateMessageStatus(id string, status MessageStatus) error {
	// 状态只前进不回退（read > delivered > sent > sending；failed 例外可覆盖）
	_, err := s.db.Exec(`
UPDATE messages SET status = ?
WHERE id = ? AND (? = 4 OR status < ?)`,
		int(status), id, int(status), int(status))
	return err
}

// MarkConversationRead 将某会话所有入站消息标记为已读，返回受影响消息 ID 列表。
func (s *Store) MarkConversationRead(conversation string) ([]string, error) {
	rows, err := s.db.Query(`
SELECT id FROM messages
WHERE conversation = ? AND direction = 1 AND status < 2`, conversation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	_, err = s.db.Exec(`
UPDATE messages SET status = 3, received_at = COALESCE(received_at, ?)
WHERE conversation = ? AND direction = 1 AND status < 2`,
		time.Now().UnixMilli(), conversation)
	return ids, err
}

// SearchMessages 全文搜索，跨会话，时间倒序。
// 西文查询走 FTS5 索引；含 CJK 字符的查询退化为 LIKE 子串匹配——
// FTS5 默认 unicode61 分词器把连续中文当作单一 token，无法命中子串。
// 本地聊天历史为万级行数，LIKE 扫描的代价可接受。
func (s *Store) SearchMessages(query string, limit int) ([]*Message, error) {
	if limit <= 0 {
		limit = 50
	}
	if containsCJK(query) {
		return s.searchMessagesLike(query, limit)
	}
	rows, err := s.db.Query(`
SELECT m.id, m.conversation, m.peer_id, m.direction, m.kind, m.text, m.file_id, m.reply_to, m.sent_at, m.received_at, m.status
FROM messages_fts f
JOIN messages m ON m.rowid = f.rowid
WHERE messages_fts MATCH ?
ORDER BY m.sent_at DESC LIMIT ?`, query, limit)
	if err != nil {
		// FTS 语法错误（用户输入含特殊字符）时退化为 LIKE
		return s.searchMessagesLike(query, limit)
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// containsCJK 报告查询是否含中日韩字符（CJK 统一表意文字及扩展、兼容区）。
func containsCJK(q string) bool {
	for _, r := range q {
		if (r >= 0x2E80 && r <= 0x9FFF) || (r >= 0xF900 && r <= 0xFAFF) ||
			(r >= 0xFE30 && r <= 0xFE4F) || (r >= 0x3000 && r <= 0x303F) {
			return true
		}
	}
	return false
}

func (s *Store) searchMessagesLike(query string, limit int) ([]*Message, error) {
	rows, err := s.db.Query(`
SELECT id, conversation, peer_id, direction, kind, text, file_id, reply_to, sent_at, received_at, status
FROM messages WHERE text LIKE ? ORDER BY sent_at DESC LIMIT ?`, "%"+query+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecentConversations 返回有消息的会话及各自最后一条消息时间，倒序。
func (s *Store) RecentConversations(limit int) ([]string, error) {
	rows, err := s.db.Query(`
SELECT conversation FROM messages
GROUP BY conversation ORDER BY MAX(sent_at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanMessage(sc scanner) (*Message, error) {
	var m Message
	var dir, kind, status int
	if err := sc.Scan(&m.ID, &m.Conversation, &m.PeerID, &dir, &kind, &m.Text,
		&m.FileID, &m.ReplyTo, &m.SentAt, &m.ReceivedAt, &status); err != nil {
		return nil, err
	}
	m.Direction = Direction(dir)
	m.Kind = MessageKind(kind)
	m.Status = MessageStatus(status)
	return &m, nil
}
