-- Mutable maintenance state; never a versioned graph row. A fence prevents a
-- slow, expired claimant from deleting or advancing a replacement's batch.
CREATE TABLE concept_retention_progress (
    policy text PRIMARY KEY,
    cursor_at timestamptz,
    cursor_id text NOT NULL DEFAULT '',
    generation bigint NOT NULL DEFAULT 0,
    lease_until timestamptz NOT NULL DEFAULT '-infinity'
);
