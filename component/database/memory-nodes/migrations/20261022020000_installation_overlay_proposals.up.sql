CREATE TABLE release_installation_proposals (
 proposal_id text PRIMARY KEY CHECK (proposal_id ~ '^[0-9a-f]{64}$'),
 owner_user_id text NOT NULL,
 installation_id text NOT NULL,
 record jsonb NOT NULL CHECK (jsonb_typeof(record)='object'),
 branch_commit text CHECK (branch_commit ~ '^[0-9a-f]{40}$'),
 pull_request integer CHECK (pull_request > 0),
 pull_request_url text,
 merged_commit text CHECK (merged_commit ~ '^[0-9a-f]{40}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK (pull_request IS NULL OR (branch_commit IS NOT NULL AND pull_request_url IS NOT NULL)),
 CHECK (merged_commit IS NULL OR pull_request IS NOT NULL)
);
CREATE INDEX release_installation_proposals_owner ON release_installation_proposals(owner_user_id,installation_id,created_at DESC);
