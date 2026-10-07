-- Native authority and attempt fences for one remote release. The immutable
-- candidate and installed DSL decide what to do; rows grant no editable scope.
CREATE TABLE IF NOT EXISTS release_draft_intents (
    resource_key text PRIMARY KEY,
    intent_id text NOT NULL UNIQUE,
    owner_user_id text NOT NULL,
    candidate_id text NOT NULL REFERENCES release_candidates(candidate_id),
    approval_id text NOT NULL REFERENCES release_candidate_approvals(approval_id),
    plan_digest text NOT NULL,
    plan jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('prepared','creating','ready','promoting','published')),
    release_id bigint CHECK (release_id > 0),
    receipt jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((state IN ('prepared','creating') AND release_id IS NULL) OR
           (state IN ('ready','promoting','published') AND release_id IS NOT NULL)),
    CHECK ((state='published' AND receipt IS NOT NULL) OR
           (state<>'published' AND receipt IS NULL))
);
CREATE INDEX IF NOT EXISTS release_draft_intents_candidate_idx ON release_draft_intents(candidate_id);
