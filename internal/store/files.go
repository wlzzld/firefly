package store

import "database/sql"

// UpsertFile 写入或更新文件传输记录（transfer_id 为主键）。
func (s *Store) UpsertFile(f *File) error {
	_, err := s.db.Exec(`
INSERT INTO files (id, transfer_id, peer_id, direction, name, size, state, local_path, resume_offset, created_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(transfer_id) DO UPDATE SET
    state = excluded.state,
    local_path = excluded.local_path,
    resume_offset = excluded.resume_offset,
    finished_at = excluded.finished_at`,
		f.ID, f.TransferID, f.PeerID, int(f.Direction), f.Name, f.Size,
		int(f.State), f.LocalPath, f.ResumeOffset, f.CreatedAt, f.FinishedAt)
	return err
}

func (s *Store) GetFile(transferID string) (*File, error) {
	row := s.db.QueryRow(`
SELECT id, transfer_id, peer_id, direction, name, size, state, local_path, resume_offset, created_at, finished_at
FROM files WHERE transfer_id = ?`, transferID)
	f, err := scanFile(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

// GetFileByContentID 按内容哈希查找本机已有的（部分）文件，用于断点续传与秒传判断。
func (s *Store) GetFileByContentID(peerID, fileID string) (*File, error) {
	row := s.db.QueryRow(`
SELECT id, transfer_id, peer_id, direction, name, size, state, local_path, resume_offset, created_at, finished_at
FROM files WHERE peer_id = ? AND id = ? AND direction = 1
ORDER BY created_at DESC LIMIT 1`, peerID, fileID)
	f, err := scanFile(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

func (s *Store) UpdateFileState(transferID string, state FileState) error {
	var err error
	if state == FileDone || state == FileFailed || state == FileCanceled || state == FileDeclined {
		_, err = s.db.Exec(`UPDATE files SET state = ?, finished_at = ? WHERE transfer_id = ?`,
			int(state), nowMs(), transferID)
	} else {
		_, err = s.db.Exec(`UPDATE files SET state = ? WHERE transfer_id = ?`, int(state), transferID)
	}
	return err
}

func (s *Store) UpdateFileProgress(transferID string, offset int64) error {
	_, err := s.db.Exec(`UPDATE files SET resume_offset = ? WHERE transfer_id = ?`, offset, transferID)
	return err
}

// ListFiles 按时间倒序列出传输记录；peerID 为空表示全部。
func (s *Store) ListFiles(peerID string, limit int) ([]*File, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	if peerID == "" {
		rows, err = s.db.Query(`
SELECT id, transfer_id, peer_id, direction, name, size, state, local_path, resume_offset, created_at, finished_at
FROM files ORDER BY created_at DESC LIMIT ?`, limit)
	} else {
		rows, err = s.db.Query(`
SELECT id, transfer_id, peer_id, direction, name, size, state, local_path, resume_offset, created_at, finished_at
FROM files WHERE peer_id = ? ORDER BY created_at DESC LIMIT ?`, peerID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func scanFile(sc scanner) (*File, error) {
	var f File
	var dir, state int
	if err := sc.Scan(&f.ID, &f.TransferID, &f.PeerID, &dir, &f.Name, &f.Size,
		&state, &f.LocalPath, &f.ResumeOffset, &f.CreatedAt, &f.FinishedAt); err != nil {
		return nil, err
	}
	f.Direction = Direction(dir)
	f.State = FileState(state)
	return &f, nil
}
