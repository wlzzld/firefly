package store

import "database/sql"

// UpsertPeer 插入或更新设备信息（不覆盖已有 trust 状态，除非显式调用 SetTrust）。
// FirstSeen 未设置时由存储层补当前时间；冲突更新保留原 first_seen 与 trust。
func (s *Store) UpsertPeer(p *Peer) error {
	if p.FirstSeen == 0 {
		p.FirstSeen = nowMs()
	}
	if p.Trust == "" {
		p.Trust = TrustUnverified
	}
	_, err := s.db.Exec(`
INSERT INTO peers (id, name, platform, fingerprint, trust, first_seen, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    name = excluded.name,
    platform = excluded.platform,
    fingerprint = excluded.fingerprint,
    last_seen = excluded.last_seen`,
		p.ID, p.Name, p.Platform, p.Fingerprint, string(p.Trust), p.FirstSeen, p.LastSeen)
	return err
}

func (s *Store) GetPeer(id string) (*Peer, error) {
	row := s.db.QueryRow(`SELECT id, name, platform, fingerprint, trust, first_seen, last_seen FROM peers WHERE id = ?`, id)
	p, err := scanPeer(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (s *Store) ListPeers() ([]*Peer, error) {
	rows, err := s.db.Query(`SELECT id, name, platform, fingerprint, trust, first_seen, last_seen FROM peers ORDER BY last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Peer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SetTrust(id string, trust TrustState) error {
	_, err := s.db.Exec(`UPDATE peers SET trust = ? WHERE id = ?`, string(trust), id)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanPeer(sc scanner) (*Peer, error) {
	var p Peer
	var trust string
	if err := sc.Scan(&p.ID, &p.Name, &p.Platform, &p.Fingerprint, &trust, &p.FirstSeen, &p.LastSeen); err != nil {
		return nil, err
	}
	p.Trust = TrustState(trust)
	return &p, nil
}
