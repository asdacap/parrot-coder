ALTER TABLE session ADD COLUMN parent_session_id TEXT REFERENCES session(id) ON DELETE SET NULL;
