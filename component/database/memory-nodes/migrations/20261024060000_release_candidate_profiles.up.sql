-- Opt-in immutable materialization; no row grants approval or remote authority.
-- Candidate mapping is established before candidate preparation/pinning, so it
-- intentionally has no FK requiring the later candidate row to exist first.
CREATE TABLE IF NOT EXISTS release_candidate_profile_bindings (
    binding_id text PRIMARY KEY CHECK (binding_id ~ '^sha256:[0-9a-f]{64}$'),
    owner_user_id text NOT NULL,
    body jsonb NOT NULL CHECK (octet_length(body::text) <= 1048576),
    candidate_id text UNIQUE CHECK (candidate_id IS NULL OR candidate_id ~ '^sha256:[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS release_candidate_profile_bindings_owner ON release_candidate_profile_bindings(owner_user_id);
