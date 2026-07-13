ALTER TABLE session ADD COLUMN selected_agent TEXT NOT NULL DEFAULT 'build';
ALTER TABLE session ADD COLUMN selected_provider TEXT NOT NULL DEFAULT '';
ALTER TABLE session ADD COLUMN selected_model TEXT NOT NULL DEFAULT '';

ALTER TABLE session_message ADD COLUMN parts_json BLOB NOT NULL DEFAULT '[]' CHECK (json_valid(parts_json));
ALTER TABLE session_message ADD COLUMN status TEXT NOT NULL DEFAULT 'complete'
    CHECK (status IN ('active', 'complete', 'error', 'interrupted'));
ALTER TABLE session_message ADD COLUMN finish_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE session_message ADD COLUMN error_text TEXT NOT NULL DEFAULT '';
ALTER TABLE session_message ADD COLUMN usage_json BLOB NOT NULL DEFAULT '{}' CHECK (json_valid(usage_json));

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
