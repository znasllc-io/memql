-- Account export, and the governed move of rows between accounts (design record
-- docs/superpowers/specs/2026-11-12-account-export-design.md). Mutable state of
-- the cluster's own; none of it is a versioned graph row.
--
-- WHY TABLES AND NOT CONCEPTS. An export of one account must never be part of
-- another account's data, a run's heartbeat must not write a version, and the
-- sealed passphrase must leave no history. A row in the graph would do all
-- three.

-- THE EXPORT'S RECORD. One row per export, kept for good.
--
--  state            draft, queued, running, complete, failed, cancelled.
--                   A draft is prepared and not started; one that is not
--                   started within fifteen minutes is deleted.
--  phase            where a running export stands: checking, rows, bytes,
--                   sealing, verifying. Empty otherwise.
--  sealed_passphrase
--                   the passphrase of a protected export, sealed under the
--                   cluster's key (component/secret). Present from prepare
--                   until the run reaches any final state, then NULL: after
--                   that the cluster cannot open its own protected file.
--  attempt, claimed_by, heartbeat_at
--                   the lease. An agent node claims a queued row FOR UPDATE
--                   SKIP LOCKED and heartbeats every twenty seconds. A running
--                   row whose heartbeat is two minutes old is queued again and
--                   started from the beginning, three attempts at most.
--  kinds_*, rows_written, versions_written, bytes_*
--                   progress, read by the Export tab.
--  object_key, file_name, size_bytes, sha256
--                   the finished file in blob storage, under exports/accounts/.
--  report           the file's own report.json, kept beside the record.
--  removed_at, removed_by
--                   set when a person deletes the file. The record stays.
CREATE TABLE account_exports (
 id text PRIMARY KEY,
 account_id text NOT NULL,
 account_name text NOT NULL DEFAULT '',
 requested_by text NOT NULL,
 requested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 state text NOT NULL CHECK (state IN ('draft', 'queued', 'running', 'complete', 'failed', 'cancelled')),
 phase text NOT NULL DEFAULT '',
 protected boolean NOT NULL DEFAULT false,
 sealed_passphrase text,
 attempt integer NOT NULL DEFAULT 0,
 claimed_by text NOT NULL DEFAULT '',
 heartbeat_at timestamptz,
 started_at timestamptz,
 finished_at timestamptz,
 cancel_requested boolean NOT NULL DEFAULT false,
 kinds_total integer NOT NULL DEFAULT 0,
 kinds_done integer NOT NULL DEFAULT 0,
 rows_written bigint NOT NULL DEFAULT 0,
 versions_written bigint NOT NULL DEFAULT 0,
 bytes_total bigint NOT NULL DEFAULT 0,
 bytes_done bigint NOT NULL DEFAULT 0,
 object_key text NOT NULL DEFAULT '',
 file_name text NOT NULL DEFAULT '',
 size_bytes bigint NOT NULL DEFAULT 0,
 sha256 text NOT NULL DEFAULT '',
 report jsonb,
 failure_code text NOT NULL DEFAULT '',
 failure_detail text NOT NULL DEFAULT '',
 removed_at timestamptz,
 removed_by text NOT NULL DEFAULT ''
);
CREATE INDEX account_exports_account_idx ON account_exports (account_id, requested_at DESC, id);
-- An account has at most one export that is prepared, queued or running.
CREATE UNIQUE INDEX account_exports_one_active_idx ON account_exports (account_id)
 WHERE state IN ('draft', 'queued', 'running');
-- The runner's claim: the oldest queued row, and running rows by heartbeat.
CREATE INDEX account_exports_work_idx ON account_exports (state, requested_at)
 WHERE state IN ('queued', 'running');

-- WHAT WAS MOVED. One row per root the governed act moved or placed. A move
-- writes the account on a stored version in place and appends no version, so
-- this table and the audit trail are the only record that it happened.
-- from_account is empty for a row that was placed (it had no account).
CREATE TABLE account_row_moves (
 id bigserial PRIMARY KEY,
 concept text NOT NULL,
 row_id text NOT NULL,
 from_account text NOT NULL DEFAULT '',
 to_account text NOT NULL,
 moved_by text NOT NULL,
 moved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 followers bigint NOT NULL DEFAULT 0,
 note text NOT NULL DEFAULT ''
);
CREATE INDEX account_row_moves_to_idx ON account_row_moves (to_account, moved_at DESC, id);
CREATE INDEX account_row_moves_from_idx ON account_row_moves (from_account, moved_at DESC, id);

-- FOLLOWERS FOLLOW. When a row's recorded account changes, the rows that hang
-- off it in the account it left have to take the new one. The change records a
-- task here, and the assignment runner works tasks off in batches
-- (component/memql/account_follow.go): for each kind that follows the parent's
-- kind, the rows whose `via` field names the parent and whose own account is
-- from_account take to_account, and each of those that has followers of its own
-- records a task in turn.
--
--  cursor_concept, cursor_id   where the task stands, so a batch that is cut
--                              short resumes and any replica can continue.
--  done_at                     set when no kind has a row left to follow.
--
-- An export waits while a task that names its account is not done.
CREATE TABLE account_follow_tasks (
 id bigserial PRIMARY KEY,
 parent_concept text NOT NULL,
 parent_id text NOT NULL,
 from_account text NOT NULL DEFAULT '',
 to_account text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 cursor_concept text NOT NULL DEFAULT '',
 cursor_id text NOT NULL DEFAULT '',
 followed bigint NOT NULL DEFAULT 0,
 next_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 done_at timestamptz
);
CREATE INDEX account_follow_tasks_pending_idx ON account_follow_tasks (next_at, id) WHERE done_at IS NULL;

-- A CONFLICT RECORDS ITS CANDIDATES. The accounts a row the job will not guess
-- at could belong to, so "does a conflict name this account" is a lookup and
-- not a reading of the prose in `detail`. Filled by the job from this release
-- on; a conflict recorded before gains them on the job's next pass.
--
-- left_out_at and left_out_by record an owner's decision that the row stays in
-- no account for good. Such a conflict is resolved and holds no export back.
ALTER TABLE account_assignment_conflict ADD COLUMN candidates text[] NOT NULL DEFAULT '{}';
ALTER TABLE account_assignment_conflict ADD COLUMN left_out_at timestamptz;
ALTER TABLE account_assignment_conflict ADD COLUMN left_out_by text NOT NULL DEFAULT '';

-- A FULL PASS ON REQUEST. An export that finds rows waiting for an account asks
-- for a full pass of their kind instead of waiting a day for the next one. The
-- job honors a request newer than the kind's last full pass at once.
ALTER TABLE account_assignment_progress ADD COLUMN full_pass_requested_at timestamptz;
