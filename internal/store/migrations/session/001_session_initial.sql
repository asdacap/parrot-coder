-- One database per session. Every table here belongs to exactly one session,
-- so every foreign key stays inside this file and SQLite keeps enforcing it.
--
-- The session row is a singleton: it carries the metadata that the shared
-- session table used to hold. project_id and project_root are denormalized
-- because project.StableID is a pure function of the repository identity, so a
-- project table would only cache a value any host can recompute.
CREATE TABLE session (
    id TEXT PRIMARY KEY,
    singleton INTEGER NOT NULL DEFAULT 0 UNIQUE CHECK (singleton = 0),
    project_id TEXT NOT NULL DEFAULT '',
    project_root TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL,
    selected_agent TEXT NOT NULL DEFAULT 'build',
    selected_provider TEXT NOT NULL DEFAULT '',
    selected_model TEXT NOT NULL DEFAULT '',
    selected_variant TEXT NOT NULL DEFAULT '',
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
    parts_json BLOB NOT NULL DEFAULT '[]' CHECK (json_valid(parts_json)),
    status TEXT NOT NULL DEFAULT 'complete'
        CHECK (status IN ('active', 'complete', 'error', 'interrupted')),
    finish_reason TEXT NOT NULL DEFAULT '',
    error_text TEXT NOT NULL DEFAULT '',
    usage_json BLOB NOT NULL DEFAULT '{}' CHECK (json_valid(usage_json)),
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

CREATE TABLE session_tool_call (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    message_id TEXT NOT NULL REFERENCES session_message(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    input_json BLOB NOT NULL CHECK (json_valid(input_json)),
    status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'success', 'failure', 'interrupted')),
    result_text TEXT NOT NULL DEFAULT '',
    error_text TEXT NOT NULL DEFAULT '',
    sequence INTEGER NOT NULL CHECK (sequence >= 0),
    settled_sequence INTEGER CHECK (settled_sequence >= 0),
    created_at TEXT NOT NULL,
    settled_at TEXT,
    UNIQUE (session_id, id),
    FOREIGN KEY (session_id, sequence) REFERENCES event(session_id, sequence),
    FOREIGN KEY (session_id, settled_sequence) REFERENCES event(session_id, sequence)
);

CREATE INDEX session_tool_call_active
    ON session_tool_call(session_id, status, sequence);

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

CREATE TABLE compaction_attempt (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    source_epoch_id TEXT NOT NULL REFERENCES session_context_epoch(id) ON DELETE RESTRICT,
    covered_from_sequence INTEGER NOT NULL CHECK (covered_from_sequence >= 0),
    covered_to_sequence INTEGER NOT NULL CHECK (covered_to_sequence >= covered_from_sequence),
    history_cutoff INTEGER NOT NULL CHECK (history_cutoff > covered_to_sequence),
    provider_id TEXT NOT NULL,
    model_id TEXT NOT NULL,
    forced INTEGER NOT NULL CHECK (forced IN (0, 1)),
    status TEXT NOT NULL CHECK (status IN ('active', 'completed', 'failed', 'interrupted')),
    error_text TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    finished_at TEXT
);

CREATE INDEX compaction_attempt_session_status
    ON compaction_attempt(session_id, status, created_at);

CREATE TABLE compaction_record (
    id TEXT PRIMARY KEY,
    attempt_id TEXT NOT NULL UNIQUE REFERENCES compaction_attempt(id) ON DELETE RESTRICT,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    source_epoch_id TEXT NOT NULL REFERENCES session_context_epoch(id) ON DELETE RESTRICT,
    target_epoch_id TEXT NOT NULL UNIQUE REFERENCES session_context_epoch(id) ON DELETE RESTRICT,
    covered_from_sequence INTEGER NOT NULL CHECK (covered_from_sequence >= 0),
    covered_to_sequence INTEGER NOT NULL CHECK (covered_to_sequence >= covered_from_sequence),
    history_cutoff INTEGER NOT NULL CHECK (history_cutoff > covered_to_sequence),
    summary TEXT NOT NULL,
    usage_json BLOB NOT NULL CHECK (json_valid(usage_json)),
    provider_id TEXT NOT NULL,
    model_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (session_id, source_epoch_id, covered_from_sequence, covered_to_sequence)
);

CREATE INDEX compaction_record_session_created
    ON compaction_record(session_id, created_at);
