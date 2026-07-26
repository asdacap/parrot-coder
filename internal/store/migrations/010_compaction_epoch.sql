ALTER TABLE compaction_record RENAME TO old_compaction_record;
ALTER TABLE compaction_attempt RENAME TO old_compaction_attempt;
ALTER TABLE session_context_epoch RENAME TO old_session_context_epoch;

CREATE TABLE session_compaction_epoch (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    summary_prompt TEXT NOT NULL,
    history_cutoff INTEGER NOT NULL CHECK (history_cutoff >= 0),
    created_at TEXT NOT NULL,
    UNIQUE (session_id, ordinal)
);

INSERT INTO session_compaction_epoch(id, session_id, ordinal, summary_prompt, history_cutoff, created_at)
SELECT epoch.id, epoch.session_id, epoch.ordinal,
       COALESCE((
           SELECT printf(
               '----- BEGIN COMPACTED SESSION HISTORY -----
Source epoch: %s
Covered sequences: %d-%d
History cutoff: %d

%s
----- END COMPACTED SESSION HISTORY -----',
               record.source_epoch_id, record.covered_from_sequence,
               record.covered_to_sequence, record.history_cutoff, trim(record.summary))
           FROM old_compaction_record AS record
           WHERE record.target_epoch_id = epoch.id
       ), ''),
       epoch.history_cutoff, epoch.created_at
FROM old_session_context_epoch AS epoch;

WITH RECURSIVE
missing(session_id, created_at, base) AS (
    SELECT session.id, session.created_at, 'ctx_migrated_' || hex(session.id)
    FROM session
    WHERE NOT EXISTS (
        SELECT 1 FROM session_compaction_epoch AS epoch WHERE epoch.session_id = session.id
    )
),
candidate(session_id, created_at, base, suffix, id) AS (
    SELECT session_id, created_at, base, 0, base FROM missing
    UNION ALL
    SELECT session_id, created_at, base, suffix + 1, base || '_' || (suffix + 1)
    FROM candidate
    WHERE EXISTS (SELECT 1 FROM session_compaction_epoch AS epoch WHERE epoch.id = candidate.id)
)
INSERT INTO session_compaction_epoch(id, session_id, ordinal, summary_prompt, history_cutoff, created_at)
SELECT id, session_id, 0, '', 0, created_at
FROM candidate
WHERE NOT EXISTS (SELECT 1 FROM session_compaction_epoch AS epoch WHERE epoch.id = candidate.id);

CREATE TABLE compaction_attempt (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    source_epoch_id TEXT NOT NULL REFERENCES session_compaction_epoch(id) ON DELETE RESTRICT,
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

INSERT INTO compaction_attempt SELECT * FROM old_compaction_attempt;

CREATE TABLE compaction_record (
    id TEXT PRIMARY KEY,
    attempt_id TEXT NOT NULL UNIQUE REFERENCES compaction_attempt(id) ON DELETE RESTRICT,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    source_epoch_id TEXT NOT NULL REFERENCES session_compaction_epoch(id) ON DELETE RESTRICT,
    target_epoch_id TEXT NOT NULL UNIQUE REFERENCES session_compaction_epoch(id) ON DELETE RESTRICT,
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

INSERT INTO compaction_record SELECT * FROM old_compaction_record;

DROP TABLE old_compaction_record;
DROP TABLE old_compaction_attempt;
DROP TABLE old_session_context_epoch;

CREATE INDEX session_compaction_epoch_order
    ON session_compaction_epoch(session_id, ordinal);
CREATE INDEX compaction_attempt_session_status
    ON compaction_attempt(session_id, status, created_at);
CREATE INDEX compaction_record_session_created
    ON compaction_record(session_id, created_at);
