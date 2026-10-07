-- Native immutable review and approval journal. Workflow policy and selection
-- remain in DSL; editable graph rows never grant publication authority.
CREATE TABLE IF NOT EXISTS release_candidates (
    candidate_id text PRIMARY KEY CHECK (candidate_id ~ '^sha256:[0-9a-f]{64}$'),
    owner_user_id text NOT NULL,
    manifest jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('preparing','ready','approved','retired')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE IF NOT EXISTS release_candidate_approvals (
    candidate_id text PRIMARY KEY REFERENCES release_candidates(candidate_id),
    approval_id text NOT NULL UNIQUE,
    approved_by text NOT NULL,
    approved_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE IF NOT EXISTS release_publication_intents (
    effect_id text NOT NULL UNIQUE,
    candidate_id text NOT NULL REFERENCES release_candidates(candidate_id),
    target_id text NOT NULL,
    component_name text NOT NULL,
    artifact_name text NOT NULL,
    approval_id text NOT NULL REFERENCES release_candidate_approvals(approval_id),
    state text NOT NULL CHECK (state IN ('pending','complete')),
    receipt jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(candidate_id,target_id,component_name,artifact_name),
    CHECK ((state='pending' AND receipt IS NULL) OR (state='complete' AND receipt IS NOT NULL))
);
