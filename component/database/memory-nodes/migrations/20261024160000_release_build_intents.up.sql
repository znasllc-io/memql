-- Native original-owner release selection and effect fences. No editable graph
-- row can grant a tag, build, publication approval, or installation authority.
CREATE TABLE IF NOT EXISTS release_build_intents (
    intent_id text PRIMARY KEY CHECK (intent_id ~ '^sha256:[0-9a-f]{64}$'),
    owner_user_id text NOT NULL,
    body jsonb NOT NULL CHECK (octet_length(body::text) <= 1048576),
    lease_id text NOT NULL DEFAULT '',
    lease_until timestamptz NOT NULL DEFAULT 'epoch',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS release_build_intents_owner ON release_build_intents(owner_user_id, created_at DESC, intent_id);
