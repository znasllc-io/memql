-- Private frozen destinations support historical, credential-free projections.
ALTER TABLE release_publication_intents ADD COLUMN IF NOT EXISTS target_snapshot jsonb;
CREATE TABLE IF NOT EXISTS release_catalog (
    candidate_id text PRIMARY KEY REFERENCES release_candidates(candidate_id),
    publisher text NOT NULL,
    catalog_digest text NOT NULL UNIQUE,
    envelope bytea NOT NULL CHECK (octet_length(envelope) > 0 AND octet_length(envelope) <= 2097152),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS release_catalog_created_idx ON release_catalog(created_at DESC,candidate_id DESC);
