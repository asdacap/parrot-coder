CREATE TABLE interactive_session_owner (
    session_id TEXT PRIMARY KEY REFERENCES session(id) ON DELETE CASCADE,
    working_directory TEXT NOT NULL,
    host_key TEXT NOT NULL,
    owner_pid INTEGER NOT NULL CHECK(owner_pid > 0),
    claimed_at TEXT NOT NULL
);

CREATE INDEX interactive_session_owner_directory
    ON interactive_session_owner(working_directory, claimed_at DESC, session_id DESC);

CREATE UNIQUE INDEX interactive_session_owner_process
    ON interactive_session_owner(working_directory, host_key, owner_pid);
