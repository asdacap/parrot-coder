CREATE TABLE snapshot_blob (
    hash TEXT PRIMARY KEY,
    data BLOB NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0 AND size = length(data))
);

CREATE TABLE snapshot_transaction (
    id TEXT PRIMARY KEY,
    workspace TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position > 0),
    created_at TEXT NOT NULL,
    UNIQUE (workspace, session_id, position)
);

CREATE INDEX snapshot_transaction_timeline
    ON snapshot_transaction(workspace, session_id, position);

CREATE TABLE snapshot_file (
    transaction_id TEXT NOT NULL REFERENCES snapshot_transaction(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    path TEXT NOT NULL,
    before_exists INTEGER NOT NULL CHECK (before_exists IN (0, 1)),
    before_mode INTEGER NOT NULL CHECK (before_mode >= 0),
    before_symlink TEXT,
    before_hash TEXT,
    before_blob_hash TEXT REFERENCES snapshot_blob(hash) ON DELETE RESTRICT,
    after_exists INTEGER NOT NULL CHECK (after_exists IN (0, 1)),
    after_mode INTEGER NOT NULL CHECK (after_mode >= 0),
    after_symlink TEXT,
    after_hash TEXT,
    after_blob_hash TEXT REFERENCES snapshot_blob(hash) ON DELETE RESTRICT,
    PRIMARY KEY (transaction_id, ordinal),
    UNIQUE (transaction_id, path)
);

CREATE TABLE snapshot_cursor (
    workspace TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    PRIMARY KEY (workspace, session_id)
);

CREATE TABLE session_todo (
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    id TEXT NOT NULL,
    position INTEGER NOT NULL CHECK (position >= 0),
    content TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'in_progress', 'completed', 'cancelled')),
    priority TEXT NOT NULL CHECK (priority IN ('high', 'medium', 'low')),
    PRIMARY KEY (session_id, id),
    UNIQUE (session_id, position)
);

CREATE INDEX session_todo_order ON session_todo(session_id, position);
