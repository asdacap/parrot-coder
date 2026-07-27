ALTER TABLE compaction_attempt ADD COLUMN owner_host_key TEXT NOT NULL DEFAULT '';
ALTER TABLE compaction_attempt ADD COLUMN owner_pid INTEGER NOT NULL DEFAULT 0;
ALTER TABLE compaction_attempt ADD COLUMN owner_process_key TEXT NOT NULL DEFAULT '';
