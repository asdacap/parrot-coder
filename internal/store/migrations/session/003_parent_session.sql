-- Parent sessions live in separate databases, so this relationship cannot be
-- enforced by a SQLite foreign key in the per-session database.
ALTER TABLE session ADD COLUMN parent_session_id TEXT NOT NULL DEFAULT '';
