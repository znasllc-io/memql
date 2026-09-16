-- Versioned cluster policy overrides. Shipped definitions stay in the binary;
-- reset records a new revision without that override. No existing data changes.
CREATE TABLE IF NOT EXISTS router_policy_revision (
    revision BIGINT PRIMARY KEY CHECK (revision >= 0),
    overrides JSONB NOT NULL DEFAULT '{}'::jsonb,
    rules JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO router_policy_revision (revision) VALUES (0) ON CONFLICT DO NOTHING;
