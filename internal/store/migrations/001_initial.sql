CREATE TABLE project (
    id TEXT PRIMARY KEY,
    root_path TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE session (
    id TEXT PRIMARY KEY,
    project_id TEXT REFERENCES project(id) ON DELETE SET NULL,
    title TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE event_sequence (
    session_id TEXT PRIMARY KEY REFERENCES session(id) ON DELETE CASCADE,
    next_sequence INTEGER NOT NULL CHECK (next_sequence >= 0)
);

CREATE TABLE event (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence >= 0),
    type TEXT NOT NULL,
    data_json BLOB NOT NULL CHECK (json_valid(data_json)),
    created_at TEXT NOT NULL,
    UNIQUE (session_id, sequence)
);

CREATE INDEX event_session_sequence ON event(session_id, sequence);

CREATE TABLE session_input (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    message_id TEXT NOT NULL,
    content TEXT NOT NULL,
    delivery TEXT NOT NULL CHECK (delivery IN ('steer', 'queue')),
    status TEXT NOT NULL CHECK (status IN ('pending', 'promoted')),
    admitted_sequence INTEGER NOT NULL CHECK (admitted_sequence >= 0),
    promoted_sequence INTEGER CHECK (promoted_sequence >= 0),
    created_at TEXT NOT NULL,
    promoted_at TEXT,
    UNIQUE (session_id, message_id),
    FOREIGN KEY (session_id, admitted_sequence) REFERENCES event(session_id, sequence),
    FOREIGN KEY (session_id, promoted_sequence) REFERENCES event(session_id, sequence)
);

CREATE INDEX session_input_pending
    ON session_input(session_id, delivery, status, admitted_sequence);

CREATE TABLE session_message (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    content TEXT NOT NULL,
    input_id TEXT UNIQUE REFERENCES session_input(id) ON DELETE RESTRICT,
    sequence INTEGER NOT NULL CHECK (sequence >= 0),
    created_at TEXT NOT NULL,
    UNIQUE (session_id, sequence)
);

CREATE INDEX session_message_order ON session_message(session_id, sequence);

CREATE TABLE session_context_epoch (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    baseline TEXT NOT NULL,
    sources_json BLOB NOT NULL CHECK (json_valid(sources_json)),
    history_cutoff INTEGER NOT NULL CHECK (history_cutoff >= 0),
    created_at TEXT NOT NULL,
    UNIQUE (session_id, ordinal)
);

CREATE INDEX session_context_epoch_order
    ON session_context_epoch(session_id, ordinal);
