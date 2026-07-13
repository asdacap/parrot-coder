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
