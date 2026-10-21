-- Enrollment captures a real owner's authority ceiling; cron can never lend
-- its maintenance role to publication cleanup. Cursors and leases are shared
-- by every replica, and only a finished reference release leaves the scan.
ALTER TABLE release_candidate_archives ADD COLUMN inputs_released_at timestamptz;
CREATE TABLE release_input_archive_schedules (
    owner_user_id text PRIMARY KEY,
    authority jsonb NOT NULL,
    enabled boolean NOT NULL,
    after_created_at timestamptz NOT NULL DEFAULT 'epoch',
    after_candidate_id text NOT NULL DEFAULT '',
    lease_id text NOT NULL DEFAULT '',
    lease_until timestamptz NOT NULL DEFAULT 'epoch',
    visited_at timestamptz NOT NULL DEFAULT 'epoch'
);
