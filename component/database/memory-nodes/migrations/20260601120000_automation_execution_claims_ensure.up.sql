-- Ensure automation_execution_claims exists (memql#624).
--
-- The table is created by 20260531120000_automation_execution_claims, but
-- on the staging DB that earlier migration is recorded-applied-but-empty:
-- bun_migrations marks it applied (so bun will never re-run it) while the
-- table itself was never materialised (an artifact of the #570 empty-query
-- abort that recorded the version before the body executed). The result is
-- a runtime where event-triggered automations fall back to the
-- per-process EventBridge dedup and can double-fire across replicas --
-- exactly the cross-replica guard #561 was meant to provide.
--
-- A fresh-timestamp migration is the correct remedy: bun runs it
-- regardless of the poisoned ledger row for the original migration, and
-- CREATE ... IF NOT EXISTS makes it a no-op on any DB where the original
-- migration did materialise the table. This migration is intentionally a
-- byte-for-byte mirror of the original table + index so the two converge.
--
-- NOTE: no `--bun:split` between this header and the first statement --
-- a leading comment-only segment makes bun emit an empty query and abort
-- (the very failure mode that poisoned the original). memql#570.

CREATE TABLE IF NOT EXISTS automation_execution_claims (
  automation_name text        NOT NULL,
  dedup_key       text        NOT NULL,
  claimed_by      text        NOT NULL,
  claimed_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (automation_name, dedup_key)
);

--bun:split

CREATE INDEX IF NOT EXISTS idx_automation_execution_claims_claimed_at
  ON automation_execution_claims (claimed_at);
