-- A fresh owner approval may continue an unchanged file release after the
-- engine/workflow identity changes. Keep both approvals and the original
-- publication proofs; this journal permanently fences the previous authority.
CREATE TABLE IF NOT EXISTS release_candidate_successions (
    previous_candidate_id text PRIMARY KEY REFERENCES release_candidates(candidate_id),
    candidate_id text NOT NULL UNIQUE REFERENCES release_candidates(candidate_id),
    previous_approval_id text NOT NULL REFERENCES release_candidate_approvals(approval_id),
    approval_id text NOT NULL REFERENCES release_candidate_approvals(approval_id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (previous_candidate_id <> candidate_id),
    CHECK (previous_approval_id <> approval_id)
);
