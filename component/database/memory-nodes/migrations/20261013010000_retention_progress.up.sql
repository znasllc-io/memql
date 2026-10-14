-- A checkpoint per policy and configured window. It is advanced only after
-- completed archive/delete work, and reset after a complete scan so records
-- kept by a dependency, denied admission, or a backdated insert are revisited.
CREATE TABLE work_retention_progress (
    policy text PRIMARY KEY,
    cursor_at timestamptz,
    cursor_id text NOT NULL DEFAULT '',
    generation bigint NOT NULL DEFAULT 0
);
