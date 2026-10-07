-- A reversal retains its parent installation head. Recording it permanently
-- fences the forward recipe before another controller operation can begin.
CREATE TABLE installation_revision_rollbacks (
    parent_plan_id text PRIMARY KEY REFERENCES installation_revision_attempts(plan_id),
    rollback_id text NOT NULL UNIQUE CHECK (rollback_id ~ '^memql-id:[0-9a-f]{64}$'),
    plan jsonb NOT NULL,
    started_at timestamptz,
    observation jsonb,
    observation_version bigint NOT NULL DEFAULT 0 CHECK (observation_version >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((observation_version=0 AND observation IS NULL) OR
           (observation_version>0 AND observation IS NOT NULL AND started_at IS NOT NULL))
);
