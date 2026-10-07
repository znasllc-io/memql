-- Native installation effect journal. Graph rows and client supplied objects
-- cannot authorize an update. An uncertain started attempt retains its slot.
CREATE TABLE IF NOT EXISTS installation_revision_attempts (
    plan_id text PRIMARY KEY CHECK (plan_id ~ '^memql-id:[0-9a-f]{64}$'),
    installation_id text NOT NULL,
    requested_by text NOT NULL,
    slot_epoch bigint NOT NULL CHECK (slot_epoch > 0),
    plan jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('prepared','applying','cancelled')),
    observation jsonb,
    observation_version bigint NOT NULL DEFAULT 0 CHECK (observation_version >= 0),
    started_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((state='applying' AND started_at IS NOT NULL) OR
           (state IN ('prepared','cancelled') AND started_at IS NULL)),
    CHECK ((observation_version=0 AND observation IS NULL) OR
           (observation_version>0 AND observation IS NOT NULL AND state='applying'))
);
CREATE TABLE IF NOT EXISTS installation_revision_heads (
    installation_id text PRIMARY KEY,
    active_plan_id text REFERENCES installation_revision_attempts(plan_id),
    slot_epoch bigint NOT NULL DEFAULT 0 CHECK (slot_epoch >= 0)
);
CREATE INDEX IF NOT EXISTS installation_revision_history_idx
    ON installation_revision_attempts(installation_id, created_at, plan_id);
