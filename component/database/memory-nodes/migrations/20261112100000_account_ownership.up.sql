-- Account stamping's rollout switch, and the state of the job that assigns
-- existing rows. Mutable maintenance state; never a versioned graph row.
--
-- THE SWITCH. The release that records the account of every row adds the
-- accountId field to the concepts that gain one. While that release rolls out,
-- nodes of the previous one still serve: their copy of those concepts does not
-- declare the field, and a node refuses to update a row carrying a key its
-- concept does not declare. So the engine neither stamps a new row nor assigns
-- an existing one until every node that has not stopped runs an engine that
-- knows the field.
--
-- account_stamping_storage is that switch, one row. account_stamping_readers is
-- how a node says it knows the field: while the switch is off it upserts its
-- own row every five seconds. The switch turns on once every v1:cluster:node
-- whose latest health is not 'stopped' has advertised within 30 seconds
-- (EnableAccountStampingWhenReady in
-- component/database/memory-nodes/account_stamping.go: the reader handshake of
-- 20261013050000_liveness, on tables of its own). A node of an older release
-- never advertises, and that is what holds the switch during a rollout. Nothing
-- turns it off again: rows already carry the field.
--
-- The node's release is not written on its v1:cluster:node row instead, because
-- nodes write each other's rows (health, retirement) and a node of the previous
-- release would refuse a row carrying a key it does not declare.
--
-- An operator holds the switch with MEMQL_ACCOUNT_STAMPING_ACTIVATE=false. A
-- held node still advertises, so releasing the hold takes effect at the next
-- check, but it never turns the switch on, and it neither stamps nor assigns
-- even where another replica already has (AccountOwnershipRunner in
-- component/memql/account_ownership_runner.go).
CREATE TABLE account_stamping_storage (
 singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
 enabled boolean NOT NULL DEFAULT false,
 next_check_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO account_stamping_storage DEFAULT VALUES;
CREATE TABLE account_stamping_readers (node_id text PRIMARY KEY, seen_at timestamptz NOT NULL);

-- THE JOB. One row per engine-managed concept (AccountAssignment in
-- component/memql/account_assignment.go). A call claims one row FOR UPDATE
-- SKIP LOCKED and walks at most 500 of the concept's ids in the same
-- transaction that moves the cursor, so a crash rolls both back and any
-- replica resumes from the last committed batch; the row lock is the lease.
--
--  cursor_id, cursor_at   where the current pass stands. A full pass walks ids
--                         in order (cursor_id); a later pass walks the versions
--                         written since verified_through in ("createdAt", id)
--                         order (both).
--  verified_through       the START of the last finished pass: every row whose
--                         newest version is older was judged. Never the finish
--                         time, which would skip rows written during the pass.
--                         NULL until the first full pass finishes.
--  pass_full              whether the current or next pass walks every id.
--  full_pass_at           the safe point of the last finished full pass: the
--                         earlier of its start and its late-commit horizon,
--                         so everything committed before it was read by that
--                         pass. A full pass is due again a day after it, for
--                         rows that were waiting on a parent; a requested pass
--                         is pending, and a change record unsettled, until a
--                         full pass finishes whose safe point is later.
--  pass_started_at/_finished_at, next_at
--                         when the current pass began, when the last one ended,
--                         and when the row may next be claimed (a minute after
--                         a pass ends, at once while one is under way).
--  assigned, kept, shared, conflicts
--                         the judgments of the CURRENT pass, reset when a pass
--                         starts, so a finished row reads as its last pass.
--  waiting                rows left waiting on a parent by the last FULL pass
--                         plus any found waiting since; reset only when a full
--                         pass starts. A concept is walked in full again only
--                         while this is above zero, a day after its last full
--                         pass; one with nothing waiting is never re-walked.
--  batch_size             this concept's own bound on a batch, at most 500. A
--                         batch that fails (a deadline, a statement timeout: a
--                         concept with large payloads rewrites a lot of TOAST)
--                         halves it, floor 1, and pushes next_at 30 seconds out
--                         so other concepts go on; a batch that succeeds grows
--                         it back by a quarter.
CREATE TABLE account_assignment_progress (
 concept text PRIMARY KEY,
 cursor_at timestamptz,
 cursor_id text,
 verified_through timestamptz,
 pass_full boolean NOT NULL DEFAULT true,
 full_pass_at timestamptz,
 assigned bigint NOT NULL DEFAULT 0,
 kept bigint NOT NULL DEFAULT 0,
 conflicts bigint NOT NULL DEFAULT 0,
 shared bigint NOT NULL DEFAULT 0,
 waiting bigint NOT NULL DEFAULT 0,
 batch_size integer NOT NULL DEFAULT 500 CHECK (batch_size >= 1),
 pass_started_at timestamptz,
 pass_finished_at timestamptz,
 next_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The rows the job does not guess at, each left without an account until an
-- owner places it. One row per (concept, row, kind of conflict), so finding the
-- same conflict on a later pass records nothing new.
CREATE TABLE account_assignment_conflict (
 concept text NOT NULL,
 row_id text NOT NULL,
 kind text NOT NULL,
 detail text NOT NULL DEFAULT '',
 found_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 resolved_at timestamptz,
 PRIMARY KEY (concept, row_id, kind)
);
