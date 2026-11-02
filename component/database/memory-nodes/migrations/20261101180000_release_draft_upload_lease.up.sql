-- One upload at a time per remote release, held by a renewed lease rather than a
-- transaction. The request pool ends a session idle in a transaction after 60 s
-- and a blocked read after 8 s, so a row lock cannot be held across an upload.
ALTER TABLE release_draft_intents
    ADD COLUMN IF NOT EXISTS upload_lease_id text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS upload_lease_until timestamptz NOT NULL DEFAULT 'epoch';
