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
-- The list of an account's exports, newest first, in the order it pages.
CREATE INDEX account_exports_account_idx ON account_exports (account_id, requested_at DESC, id DESC);
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
-- The assignment job reads a batch of rows' latest lines by concept and id, to
-- put back a row whose newest version names the account it was moved out of.
CREATE INDEX account_row_moves_row_idx ON account_row_moves (concept, row_id, moved_at DESC, id DESC);

-- FOLLOWERS FOLLOW. When a row's recorded account changes from one account to
-- another, the rows that hang off it in the account it left have to take the
-- new one. This table is the record of such changes: one row per row whose
-- account changed (a moved root, a re-tagged list row, a kind's own mutation,
-- and each row the assignment job carried). The job carries followers
-- (component/memql/account_assignment_rules.go): a row whose account is the
-- one its parent left, where the parent is now in another account and has a
-- record here, takes the parent's account as it stands, and gets a record of
-- its own. The table keeps its first name; the rows are change records.
--
--  parent_concept, parent_id   the row whose account changed.
--  from_account, to_account    the account it left and the one it went to.
--  done_at                     set when every kind that can hang off the row
--                              has finished a full pass begun after created_at.
--  cursor_concept, cursor_id, followed, next_at   not used.
--
-- An export waits while a record that names its account is not settled.
-- Settled records are deleted ninety days later.
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
-- Records not settled: the count an export's readiness reads, and the job's
-- settling.
CREATE INDEX account_follow_tasks_pending_idx ON account_follow_tasks (parent_concept, created_at) WHERE done_at IS NULL;
-- The job reads a batch of parents' records by concept and id.
CREATE INDEX account_follow_tasks_parent_idx ON account_follow_tasks (parent_concept, parent_id);
-- The job deletes settled records by age.
CREATE INDEX account_follow_tasks_done_idx ON account_follow_tasks (done_at) WHERE done_at IS NOT NULL;

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

-- LATE COMMITS ARE NOT MISSED. A row inserted in a transaction that is still
-- open is invisible to the pass reading past it, so a pass must not advance
-- verified_through past the start of the oldest transaction open while it
-- read. Each batch reads that start from pg_stat_activity; this column keeps
-- the earliest one any batch of the current pass saw, because a transaction
-- open during an early batch may have committed before the last. NULL when no
-- batch saw one. Reset when a pass starts; at the pass's end verified_through
-- is the earlier of the pass's start and this.
ALTER TABLE account_assignment_progress ADD COLUMN pass_horizon_at timestamptz;
