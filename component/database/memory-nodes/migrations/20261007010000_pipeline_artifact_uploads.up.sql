-- Native storage journal: immutable intent and capacity survive node loss.
-- No age/lease-based deletion: an old producer can still finish a blob commit.
CREATE TABLE IF NOT EXISTS pipeline_artifact_uploads (
    intent_id text PRIMARY KEY,
    owner_user_id text NOT NULL,
    identity jsonb NOT NULL,
    file_id text NOT NULL UNIQUE,
    object_key text NOT NULL UNIQUE,
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0 AND size_bytes <= 2147483648),
    generation bigint NOT NULL CHECK (generation > 0),
    state text NOT NULL CHECK (state IN ('reserved', 'blob_verified', 'ready')),
    blob_url text NOT NULL DEFAULT '',
    etag text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS pipeline_artifact_uploads_owner
    ON pipeline_artifact_uploads (owner_user_id);
