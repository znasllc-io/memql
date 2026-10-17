-- Native external-effect authority; not a client-writable MemoryNodes concept.
CREATE TABLE IF NOT EXISTS pipeline_sarif_publications (
    publication_id text PRIMARY KEY CHECK (publication_id ~ '^[0-9a-f]{64}$'),
    owner_user_id text NOT NULL,
    evidence jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('prepared','uploading','accepted','complete','failed','rejected','uncertain')),
    upload_id text NOT NULL DEFAULT '',
    report_sha256 text NOT NULL CHECK (report_sha256 ~ '^[0-9a-f]{64}$'),
    errors jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
