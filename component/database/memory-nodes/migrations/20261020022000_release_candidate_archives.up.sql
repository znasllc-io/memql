-- A published candidate can release duplicate build inputs only after fresh
-- canonical-byte verification. Its immutable catalog and publication evidence
-- survive. The terminal state fences every later preparation/publication.
ALTER TABLE release_candidates DROP CONSTRAINT release_candidates_state_check;
ALTER TABLE release_candidates ADD CONSTRAINT release_candidates_state_check
    CHECK (state IN ('preparing','ready','approved','retired','archived'));

CREATE TABLE release_candidate_archives (
    candidate_id text PRIMARY KEY REFERENCES release_candidates(candidate_id),
    owner_user_id text NOT NULL,
    approval_id text NOT NULL REFERENCES release_candidate_approvals(approval_id),
    catalog_digest text NOT NULL CHECK (catalog_digest ~ '^sha256:[0-9a-f]{64}$'),
    verified_at timestamptz NOT NULL,
    archived_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    receipt jsonb NOT NULL
);
