CREATE TABLE session_goal (
    session_id TEXT PRIMARY KEY REFERENCES session(id) ON DELETE CASCADE,
    id TEXT NOT NULL UNIQUE,
    objective TEXT NOT NULL CHECK (length(trim(objective)) > 0),
    status TEXT NOT NULL CHECK (status IN (
        'active', 'paused', 'blocked', 'usage_limited', 'budget_limited', 'complete'
    )),
    token_budget INTEGER CHECK (token_budget > 0),
    tokens_used INTEGER NOT NULL DEFAULT 0 CHECK (tokens_used >= 0),
    elapsed_seconds INTEGER NOT NULL DEFAULT 0 CHECK (elapsed_seconds >= 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
