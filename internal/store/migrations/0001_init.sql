CREATE TABLE peers (
    id          TEXT PRIMARY KEY,               -- hex(Ed25519 pub) = PeerID
    name        TEXT NOT NULL,
    platform    TEXT NOT NULL DEFAULT '',
    fingerprint TEXT NOT NULL,
    trust       TEXT NOT NULL DEFAULT 'unverified', -- unverified|trusted|blocked|key_changed
    first_seen  INTEGER NOT NULL,               -- unix ms
    last_seen   INTEGER NOT NULL
);

CREATE TABLE messages (
    id           TEXT PRIMARY KEY,              -- UUIDv7；PK 即广播去重键
    conversation TEXT NOT NULL,                 -- dm:<hex> | group:lan
    peer_id      TEXT NOT NULL,                 -- DM=对端；群聊=发送者
    direction    INTEGER NOT NULL,              -- 0=outbound 1=inbound
    kind         INTEGER NOT NULL DEFAULT 0,    -- text|file|system|notice
    text         TEXT NOT NULL DEFAULT '',
    file_id      TEXT,
    reply_to     TEXT,
    sent_at      INTEGER NOT NULL,              -- unix ms
    received_at  INTEGER,
    status       INTEGER NOT NULL DEFAULT 0     -- sending|sent|delivered|read|failed
);

CREATE INDEX idx_messages_conv_time ON messages(conversation, sent_at DESC);

CREATE TABLE files (
    id            TEXT NOT NULL,                -- sha256 hex（内容寻址）
    transfer_id   TEXT PRIMARY KEY,
    peer_id       TEXT NOT NULL,
    direction     INTEGER NOT NULL,
    name          TEXT NOT NULL,
    size          INTEGER NOT NULL,
    state         INTEGER NOT NULL DEFAULT 0,
    local_path    TEXT,
    resume_offset INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    finished_at   INTEGER
);

CREATE INDEX idx_files_peer ON files(peer_id, created_at DESC);

-- FTS5 全文搜索（external content，随 messages 同步）
CREATE VIRTUAL TABLE messages_fts USING fts5(
    text,
    content='messages',
    content_rowid='rowid',
    tokenize='unicode61'
);

CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, text) VALUES (new.rowid, new.text);
END;

CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
END;

CREATE TRIGGER messages_au AFTER UPDATE OF text ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
    INSERT INTO messages_fts(rowid, text) VALUES (new.rowid, new.text);
END;

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL                         -- JSON
);
